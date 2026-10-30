// Package runner does one pass over an instance, and the `check` connectivity probe.
package runner

import (
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/planner"
	"github.com/cstupi/arr-trickle/internal/state"
)

// blockingHealth is the health checks meaning searches would fail or pile up right now
// (indexer in backoff, no client).
var blockingHealth = map[string]bool{
	"IndexerStatusCheck":         true,
	"IndexerLongTermStatusCheck": true,
	"IndexerSearchCheck":         true,
	"DownloadClientCheck":        true,
	"DownloadClientStatusCheck":  true,
}

// Client is what a pass needs from an *arr app (client.Client, or a fake in tests).
type Client interface {
	Health() ([]map[string]any, error)
	QueueSize() (int, error)
	UpgradeProfiles() (map[int]bool, error)
	Wanted(kind string, page, pageSize int) (client.WantedPage, error)
	Command(body map[string]any) (map[string]any, error)
	CommandStatus(id int) (string, error)
}

// maxWantedPages bounds the wanted pages one pass reads looking past retired records.
const maxWantedPages = 40

// Prober is a Client that also answers /system/status (for `check`).
type Prober interface {
	Client
	Status() (map[string]any, error)
}

// Search is one search command sent.
type Search struct {
	Wanted  string   // "missing" or "cutoff"
	Command string   // e.g. "AlbumSearch"
	Items   []string // human-readable descriptions of what was searched
	Cost    int      // approximate queries per indexer
}

// Report is what one pass did for one instance.
type Report struct {
	Name         string
	Kind         string
	Skipped      string // why nothing was searched, if the whole instance was skipped
	Searches     []Search
	MissingTotal *int
	CutoffTotal  *int
	Queue        *int
	Health       []string // health problems that block searching
	Retired      int      // records retired this pass: their search finished and found nothing
	Error        string
	Unreachable  bool // Error is because the app never answered, not because it answered with an error
}

// OK reports whether the pass finished without an error.
func (r *Report) OK() bool { return r.Error == "" }

// Spent is the approximate queries sent.
func (r *Report) Spent() int {
	n := 0
	for _, s := range r.Searches {
		n += s.Cost
	}
	return n
}

// RunInstance sends this pass's searches for one instance. Errors are reported, not returned.
// retired (may be nil) is the records not to search again; this pass adds to it.
func RunInstance(inst config.Instance, c Client, budget *state.Budget, retired *state.Retired, now time.Time) *Report {
	r := &Report{Name: inst.Name, Kind: inst.Kind}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if retired == nil {
		retired = state.NewRetired("")
	}
	if err := run(inst, c, budget, retired, now, r); err != nil {
		slog.Warn("run failed", "instance", inst.Name, "err", err)
		r.Error, r.Unreachable = err.Error(), client.Unreachable(err)
	}
	return r
}

// resolve settles the searches sent by earlier passes. One that completed retires its records:
// the ones that were grabbed have left the wanted list anyway. One that failed, or that the app
// has forgotten, leaves them in rotation. While an indexer is unhealthy a completed search may
// not have reached it, so nothing is retired then.
func resolve(inst config.Instance, c Client, retired *state.Retired, r *Report) error {
	for _, p := range retired.Pending(inst.Name) {
		status, err := c.CommandStatus(p.Command)
		if err != nil {
			return err
		}
		if status == "queued" || status == "started" {
			continue
		}
		n, err := retired.Resolve(inst.Name, p.Command, status == "completed" && len(r.Health) == 0)
		if err != nil {
			return fmt.Errorf("could not save the retired list: %w", err)
		}
		r.Retired += n
	}
	if r.Retired > 0 {
		slog.Info("retired", "instance", inst.Name, "records", r.Retired, "total", retired.Count(inst.Name))
	}
	return nil
}

func run(inst config.Instance, c Client, budget *state.Budget, retired *state.Retired, now time.Time, r *Report) error {
	health, err := c.Health()
	if err != nil {
		return err
	}
	for _, h := range health {
		if src, _ := h["source"].(string); blockingHealth[src] {
			msg, _ := h["message"].(string)
			r.Health = append(r.Health, cmp.Or(msg, src))
		}
	}
	queue, err := c.QueueSize()
	if err != nil {
		return err
	}
	r.Queue = new(queue)
	if err := resolve(inst, c, retired, r); err != nil {
		return err
	}
	if len(r.Health) > 0 {
		r.Skipped = r.Health[0]
	} else if inst.MaxQueue > 0 && queue >= inst.MaxQueue {
		r.Skipped = fmt.Sprintf("download queue at or above %d", inst.MaxQueue)
	}
	if r.Skipped != "" {
		slog.Info("skipped", "instance", inst.Name, "reason", r.Skipped)
	}

	remaining := inst.DailyCap - budget.Used(inst.Name)
	totals := map[string]**int{"missing": &r.MissingTotal, "cutoff": &r.CutoffTotal}
	for _, wanted := range []string{"missing", "cutoff"} {
		count := map[string]int{"missing": inst.Missing, "cutoff": inst.Cutoff}[wanted]
		if r.Skipped != "" {
			count = 0
		}
		if inst.DailyCap > 0 && count > 0 {
			if remaining <= 0 {
				slog.Info("daily cap reached", "instance", inst.Name, "cap", inst.DailyCap)
				if len(r.Searches) == 0 {
					r.Skipped = fmt.Sprintf("daily cap of %d searches reached", inst.DailyCap)
				}
			}
			count = min(count, remaining)
		}
		if count <= 0 {
			// Nothing to send; one-record page just for the backlog size shown on the dashboard.
			page, err := c.Wanted(wanted, 1, 1)
			if err != nil {
				return err
			}
			*totals[wanted] = new(page.Total)
			continue
		}
		var upgrades map[int]bool
		if wanted == "cutoff" {
			if upgrades, err = c.UpgradeProfiles(); err != nil {
				return err
			}
		}
		// Over-fetch: unreleased, excluded, no-upgrade and retired records are filtered here. Retired
		// records were searched recently, so they sort late; keep reading pages until enough are picked.
		opts := planner.Options{
			Exclude: inst.Exclude, UpgradeProfiles: upgrades, PerSeason: inst.Kind == "sonarr" && wanted == "missing",
			Skip: retired.Set(inst.Name, wanted),
		}
		size := max(count*10, 50)
		if len(opts.Skip) > 0 {
			size = max(size, 250)
		}
		var records, picked []apps.Record
		for n := 1; n <= maxWantedPages; n++ {
			page, err := c.Wanted(wanted, n, size)
			if err != nil {
				return err
			}
			if n == 1 {
				*totals[wanted] = new(page.Total)
			}
			records = append(records, page.Records...)
			picked = planner.Select(inst.Kind, records, count, now, opts)
			if planner.Full(picked, count, opts.PerSeason) || len(page.Records) < size || n*size >= page.Total {
				break
			}
		}
		for _, cmd := range planner.BuildCommands(inst.Kind, wanted, picked) {
			cost := planner.QueryCost(cmd)
			covered := planner.RecordsFor(cmd, picked)
			var names []string
			for _, rec := range covered {
				names = append(names, apps.Describe(inst.Kind, rec))
			}
			shown := strings.Join(names[:min(10, len(names))], "; ")
			if len(names) > 10 {
				shown += fmt.Sprintf(" (+%d more)", len(names)-10)
			}
			slog.Info("search", "instance", inst.Name, "wanted", wanted, "command", cmd["name"], "items", shown)
			sent, err := c.Command(cmd)
			if err != nil {
				return err
			}
			if err := budget.Add(inst.Name, cost); err != nil {
				return fmt.Errorf("could not save the daily budget: %w", err)
			}
			if id, ok := apps.Int(sent, "id"); ok {
				if err := retired.AddPending(inst.Name, state.Pending{Command: id, Wanted: wanted, IDs: planner.IDs(covered)}); err != nil {
					return fmt.Errorf("could not save the retired list: %w", err)
				}
			}
			r.Searches = append(r.Searches, Search{Wanted: wanted, Command: fmt.Sprint(cmd["name"]), Items: names, Cost: cost})
			remaining -= cost
		}
	}
	return nil
}

// MakeClient builds the real client for an instance (reads its key).
func MakeClient(inst config.Instance) (Prober, error) {
	key, err := inst.APIKey(nil)
	if err != nil {
		return nil, err
	}
	c, err := client.New(inst.Kind, inst.URL, key, time.Duration(inst.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CheckResult is the outcome of `check` for one instance.
type CheckResult struct {
	Name  string
	OK    bool
	Lines []string
}

func (c *CheckResult) fail(msg string) { c.OK = false; c.Lines = append(c.Lines, "FAIL "+msg) }
func (c *CheckResult) good(msg string) { c.Lines = append(c.Lines, "ok   "+msg) }

// Fail is a CheckResult for an instance that couldn't be probed at all.
func Fail(name, msg string) CheckResult {
	return CheckResult{Name: name, Lines: []string{"FAIL " + msg}}
}

// CheckInstance is a read-only probe of every endpoint arr-trickle uses. It sends no searches.
// c may be nil to build the real client.
func CheckInstance(inst config.Instance, c Prober) CheckResult {
	res := CheckResult{Name: inst.Name, OK: true}
	if err := check(inst, c, &res); err != nil {
		res.fail(err.Error())
	}
	return res
}

func check(inst config.Instance, c Prober, res *CheckResult) error {
	if c == nil {
		var err error
		if c, err = MakeClient(inst); err != nil {
			return err
		}
	}
	status, err := c.Status()
	if err != nil {
		return err
	}
	expected := apps.Specs[inst.Kind].AppName
	if status["appName"] != expected {
		res.fail(fmt.Sprintf("%s is '%v', expected '%s'", inst.URL, status["appName"], expected))
		return nil
	}
	res.good(fmt.Sprintf("connected: %s %v", expected, status["version"]))
	health, err := c.Health()
	if err != nil {
		return err
	}
	var problems []string
	for _, h := range health {
		if src, _ := h["source"].(string); blockingHealth[src] {
			problems = append(problems, fmt.Sprint(h["message"]))
		}
	}
	if len(problems) > 0 {
		res.good("health: searches would be skipped now: " + problems[0])
	} else {
		res.good("health: no indexer or download client problems")
	}
	queue, err := c.QueueSize()
	if err != nil {
		return err
	}
	res.good(fmt.Sprintf("queue: %d item(s)", queue))
	profiles, err := c.UpgradeProfiles()
	if err != nil {
		return err
	}
	ids := slices.Sorted(maps.Keys(profiles))
	shown := "none"
	if len(ids) > 0 {
		shown = fmt.Sprint(ids)
	}
	res.good("quality profiles allowing upgrades: " + shown)
	for _, wanted := range []string{"missing", "cutoff"} {
		page, err := c.Wanted(wanted, 1, 1)
		if err != nil {
			return err
		}
		if page.SortKey != apps.Specs[inst.Kind].SortKey {
			res.fail(fmt.Sprintf("wanted/%s: app ignored sortKey (got '%s'); oldest-first order not guaranteed", wanted, page.SortKey))
		} else {
			res.good(fmt.Sprintf("wanted/%s: reachable, sorted by %s; %d item(s)", wanted, page.SortKey, page.Total))
		}
	}
	return nil
}
