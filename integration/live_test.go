//go:build integration

// Package integration holds contract tests against real Sonarr/Radarr/Lidarr (started by
// scripts/integration.sh). They check that every endpoint and command arr-trickle relies on still
// behaves as expected, so an upstream API change shows up here rather than as silently skipped searches.
package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	"github.com/cstupi/arr-trickle/internal/service"
	"github.com/cstupi/arr-trickle/internal/state"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
	"github.com/cstupi/arr-trickle/internal/web"
)

// Small, stable items used to populate a wanted list. Metadata comes from each app's upstream
// service; if that is unreachable the end-to-end tests are skipped, not failed.
var samples = map[string][2]string{
	"sonarr": {"/series/lookup?term=tvdb:78874", "/series"}, // Firefly
	"radarr": {"/movie/lookup/tmdb?tmdbId=603", "/movie"},   // The Matrix
	"lidarr": {"/artist/lookup?term=lidarr:a74b1b7f-71a5-4011-9441-d0b5e4122711", "/artist"},
}

func instance(t *testing.T, kind string) config.Instance {
	t.Helper()
	u := os.Getenv("ARR_IT_" + strings.ToUpper(kind) + "_URL")
	if u == "" {
		t.Skipf("ARR_IT_%s_URL not set (run scripts/integration.sh)", strings.ToUpper(kind))
	}
	i := config.NewInstance(kind, "it-"+kind, u)
	i.APIKeyFrom = os.Getenv("ARR_IT_" + strings.ToUpper(kind) + "_CONFIG")
	return i
}

func clientFor(t *testing.T, kind string) *client.Client {
	t.Helper()
	inst := instance(t, kind)
	key, err := inst.APIKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(kind, inst.URL, key, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var noIndexerOrClient = map[string]bool{"IndexerSearchCheck": true, "IndexerRssCheck": true, "DownloadClientCheck": true}

// withoutNoIndexerHealth: the throwaway apps have no indexers, which arr-trickle rightly treats as
// "don't search". For the end-to-end tests only, hide exactly those warnings so the search path is
// exercised; TestFreshAppWithoutIndexersIsSkipped covers the real behaviour.
type withoutNoIndexerHealth struct{ *client.Client }

func (w withoutNoIndexerHealth) Health() ([]map[string]any, error) {
	all, err := w.Client.Health()
	var out []map[string]any
	for _, h := range all {
		if src, _ := h["source"].(string); !noIndexerOrClient[src] {
			out = append(out, h)
		}
	}
	return out, err
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", v)
	}
	return m
}

func waitForCommand(t *testing.T, c *client.Client, id any) map[string]any {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		v, err := c.Request("GET", fmt.Sprintf("/command/%v", id), nil)
		if err != nil {
			t.Fatal(err)
		}
		cmd := asMap(t, v)
		switch cmd["status"] {
		case "completed", "failed", "aborted", "cancelled", "orphaned":
			return cmd
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("command %v did not finish in time", id)
	return nil
}

func eachKind(t *testing.T, f func(t *testing.T, kind string)) {
	for _, kind := range apps.Kinds {
		t.Run(kind, func(t *testing.T) { f(t, kind) })
	}
}

func TestCheckPasses(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		if res := runner.CheckInstance(instance(t, kind), nil); !res.OK {
			t.Fatal(strings.Join(res.Lines, "\n"))
		}
	})
}

func TestWrongKeyIsRejected(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		c, _ := client.New(kind, instance(t, kind).URL, strings.Repeat("0", 32), 30*time.Second)
		if _, err := c.Status(); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatal(err)
		}
	})
}

func TestWantedPagesAreSortedByLastSearch(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		for _, wanted := range []string{"missing", "cutoff"} {
			page, err := clientFor(t, kind).Wanted(wanted, 5)
			if err != nil || page.Total < len(page.Records) || page.SortKey != apps.Specs[kind].SortKey {
				t.Fatal(wanted, page, err)
			}
		}
	})
}

func TestSearchCommandsAreAccepted(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		c := clientFor(t, kind)
		spec := apps.Specs[kind]
		for _, name := range []string{spec.MissingCommand, spec.CutoffCommand} {
			if name == "SeasonSearch" {
				continue
			}
			cmd, err := c.Command(map[string]any{"name": name, spec.IDsField: []int{}})
			if err != nil || cmd["name"] != name {
				t.Fatal(cmd, err)
			}
			if st := waitForCommand(t, c, cmd["id"])["status"]; st != "completed" {
				t.Fatal(name, st)
			}
		}
	})
}

func addSample(t *testing.T, kind string, c *client.Client) {
	t.Helper()
	found, err := c.Request("GET", samples[kind][0], nil)
	if err != nil {
		t.Skipf("%s metadata lookup unavailable: %v", kind, err)
	}
	item, _ := found.(map[string]any)
	if list, ok := found.([]any); ok && len(list) > 0 {
		item, _ = list[0].(map[string]any)
	}
	if item == nil {
		t.Skipf("%s metadata lookup returned nothing", kind)
	}
	root := "/config/media"
	folders, err := c.Request("GET", "/rootfolder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(folders.([]any), func(f any) bool { return strings.TrimRight(fmt.Sprint(asMap(t, f)["path"]), "/") == root }) {
		body := map[string]any{"path": root}
		if kind == "lidarr" {
			body["name"], body["defaultQualityProfileId"], body["defaultMetadataProfileId"] = "media", 1, 1
		}
		if _, err := c.Request("POST", "/rootfolder", body); err != nil {
			t.Fatal(err)
		}
	}
	profiles, _ := c.Request("GET", "/qualityprofile", nil)
	item["qualityProfileId"] = asMap(t, profiles.([]any)[0])["id"]
	item["rootFolderPath"], item["monitored"] = root, true
	// Never search on add: the test drives the search itself.
	item["addOptions"] = map[string]map[string]any{
		"sonarr": {"monitor": "all", "searchForMissingEpisodes": false},
		"radarr": {"monitor": "movieOnly", "searchForMovie": false},
		"lidarr": {"monitor": "all", "searchForMissingAlbums": false},
	}[kind]
	switch kind {
	case "sonarr":
		item["seasonFolder"] = true
		delete(item, "languageProfileId")
	case "radarr":
		item["minimumAvailability"] = "released"
	case "lidarr":
		meta, _ := c.Request("GET", "/metadataprofile", nil)
		item["metadataProfileId"] = asMap(t, meta.([]any)[0])["id"]
	}
	if _, err := c.Request("POST", samples[kind][1], item); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		t.Fatal(err) // "already added" just means an earlier test added it
	}
	// The app fills in episodes/albums asynchronously after the add.
	deadline := time.Now().Add(180 * time.Second)
	for {
		page, err := c.Wanted("missing", 50)
		if err == nil && len(page.Records) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Skipf("%s: wanted list stayed empty (metadata refresh unavailable)", kind)
		}
		time.Sleep(3 * time.Second)
	}
}

func commandIDs(t *testing.T, c *client.Client) map[string]bool {
	t.Helper()
	v, err := c.Request("GET", "/command", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, it := range v.([]any) {
		ids[fmt.Sprint(asMap(t, it)["id"])] = true
	}
	return ids
}

func newCommands(t *testing.T, c *client.Client, before map[string]bool, name string) []map[string]any {
	v, _ := c.Request("GET", "/command", nil)
	var out []map[string]any
	for _, it := range v.([]any) {
		m := asMap(t, it)
		if !before[fmt.Sprint(m["id"])] && m["name"] == name {
			out = append(out, m)
		}
	}
	return out
}

func TestFreshAppWithoutIndexersIsSkipped(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		c := clientFor(t, kind)
		health, _ := c.Health()
		has := false
		for _, h := range health {
			has = has || h["source"] == "IndexerSearchCheck"
		}
		r := runner.RunInstance(instance(t, kind), c, state.NewBudget(filepath.Join(t.TempDir(), "b.json")), time.Time{})
		if !has || !r.OK() || r.Spent() != 0 || r.Skipped == "" {
			t.Fatal(health, r)
		}
	})
}

func TestEndToEndSearchOfARealMissingItem(t *testing.T) {
	eachKind(t, func(t *testing.T, kind string) {
		c := clientFor(t, kind)
		addSample(t, kind, c)
		before := commandIDs(t, c)
		inst := instance(t, kind)
		inst.Missing = 1
		r := runner.RunInstance(inst, withoutNoIndexerHealth{c}, state.NewBudget(filepath.Join(t.TempDir(), "b.json")), time.Time{})
		if !r.OK() || r.Spent() < 1 || r.MissingTotal == nil || *r.MissingTotal == 0 {
			t.Fatal(r)
		}
		sent := newCommands(t, c, before, apps.Specs[kind].MissingCommand)
		if len(sent) == 0 {
			t.Fatal("arr-trickle's search command never reached the app")
		}
		if st := waitForCommand(t, c, sent[0]["id"])["status"]; st != "completed" {
			t.Fatal(st)
		}
	})
}

// --- the web UI, end to end ---------------------------------------------------------

var (
	csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	codeRE = regexp.MustCompile(`Setup code: (\S+)`)
)

// Starts the real server on a free local port, completes first-run setup with the code from the log,
// logs in, adds every app on the Settings page using the write-only key field, presses Run now, and
// checks that the searches reached the apps and show up in Activity.
func TestWebUISetupLoginAddInstancesRunNowAndSeeActivity(t *testing.T) {
	for _, kind := range apps.Kinds {
		instance(t, kind) // skips unless the apps are running
	}
	logs := tu.CaptureLogs(t)
	dir, port := t.TempDir(), tu.FreePort(t)
	cfg := filepath.Join(dir, "config", "config.toml")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.MkdirAll(filepath.Join(dir, "state"), 0o755)
	os.WriteFile(cfg, fmt.Appendf(nil, "state_dir = %q\n[web]\nlisten = \"127.0.0.1\"\nport = %d\n", filepath.Join(dir, "state"), port), 0o644)
	settings, err := config.Load(cfg, config.Environ{}, true)
	if err != nil {
		t.Fatal(err)
	}
	// The apps have no indexers; hide exactly that health warning, as the end-to-end contract test does.
	sched := service.New(settings, func(i config.Instance) (runner.Prober, error) {
		p, err := runner.MakeClient(i)
		if err != nil {
			return nil, err
		}
		return withoutNoIndexerHealth{p.(*client.Client)}, nil
	}, nil)
	if err := web.Announce(settings, logs); err != nil {
		t.Fatal(err)
	}
	m := codeRE.FindStringSubmatch(logs.String()) // what the operator reads in the container log
	if m == nil {
		t.Fatal("no setup code in the log")
	}
	code := m[1]
	srv := web.New(settings, sched, cfg).HTTPServer(settings.Web.ListenAddr())
	go srv.ListenAndServe()
	defer srv.Close()
	sched.Start()
	defer sched.Stop(10 * time.Second)

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var resp *http.Response
	for range 100 {
		if resp, err = browser.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	do := func(method, path string, f url.Values) (int, string, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(f.Encode()))
		req.Header.Set("Origin", base)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(body)
	}
	get := func(path string) (int, string, string) { return do(http.MethodGet, path, nil) }
	post := func(path string, f url.Values) (int, string, string) { return do(http.MethodPost, path, f) }
	csrf := func(html string) string {
		m := csrfRE.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("no CSRF field:\n%s", html)
		}
		return m[1]
	}

	// first run: no admin, the code is only in the log
	if _, loc, _ := get("/"); loc != "/login" {
		t.Fatal(loc)
	}
	if _, loc, _ := get("/login"); loc != "/setup" {
		t.Fatal(loc)
	}
	_, _, page := get("/setup")
	pw := "integration test password"
	if _, loc, body := post("/setup", url.Values{"csrf": {csrf(page)}, "code": {code}, "username": {"admin"}, "password": {pw}, "confirm": {pw}}); loc != "/login" {
		t.Fatal(body)
	}
	if st, _, _ := get("/setup"); st != 404 {
		t.Fatal("setup must be gone", st)
	}
	_, _, page = get("/login")
	if _, loc, _ := post("/login", url.Values{"csrf": {csrf(page)}, "username": {"admin"}, "password": {pw}}); loc != "/" {
		t.Fatal(loc)
	}
	_, _, page = get("/")
	token := csrf(page)

	keys := map[string]string{}
	for _, kind := range apps.Kinds {
		addSample(t, kind, clientFor(t, kind))
		raw, _ := os.ReadFile(os.Getenv("ARR_IT_" + strings.ToUpper(kind) + "_CONFIG"))
		keys[kind] = config.XMLKeyRE.FindStringSubmatch(string(raw))[1]
		f := url.Values{
			"csrf": {token}, "kind": {kind}, "name": {"ui-" + kind}, "url": {os.Getenv("ARR_IT_" + strings.ToUpper(kind) + "_URL")},
			"key_source": {"api_key_from"}, "key_ref": {""}, "api_key": {keys[kind]}, // the write-only field
			"missing": {"1"}, "cutoff": {"0"},
		}
		if st, _, body := post("/settings/instances/new", f); st != 303 {
			t.Fatal(kind, st, body)
		}
	}
	text, _ := os.ReadFile(cfg)
	_, _, settingsPage := get("/settings")
	for kind, key := range keys {
		if strings.Contains(string(text), key) || !strings.Contains(string(text), "ui-"+kind+".key") || strings.Contains(settingsPage, key) {
			t.Fatalf("%s: the key leaked or was not stored", kind)
		}
	}

	// every endpoint answers through the UI's Check button
	for _, kind := range apps.Kinds {
		if _, _, body := post("/control/check", url.Values{"csrf": {token}, "name": {"ui-" + kind}}); !strings.Contains(body, "All checks passed") {
			t.Fatal(body)
		}
	}

	before := map[string]map[string]bool{}
	for _, kind := range apps.Kinds {
		before[kind] = commandIDs(t, clientFor(t, kind))
	}
	last := sched.Snapshot().LastPass
	if st, _, _ := post("/control/run-now", url.Values{"csrf": {token}}); st != 303 {
		t.Fatal(st)
	}
	deadline := time.Now().Add(120 * time.Second)
	for snap := sched.Snapshot(); snap.LastPass.Equal(last) || snap.Running; snap = sched.Snapshot() {
		if time.Now().After(deadline) {
			t.Fatal("Run now never ran a pass")
		}
		time.Sleep(500 * time.Millisecond)
	}

	_, _, activity := get("/activity")
	_, _, dashboard := get("/")
	for _, kind := range apps.Kinds {
		command := apps.Specs[kind].MissingCommand
		if len(newCommands(t, clientFor(t, kind), before[kind], command)) == 0 {
			t.Fatalf("%s: the search sent from the UI never reached the app", kind)
		}
		if !strings.Contains(activity, "ui-"+kind) || !strings.Contains(activity, command) || !strings.Contains(dashboard, "ui-"+kind) {
			t.Fatalf("%s missing from Activity or the dashboard", kind)
		}
	}
	for _, key := range keys {
		if strings.Contains(activity, key) || strings.Contains(dashboard, key) || strings.Contains(logs.String(), key) {
			t.Fatal("a key leaked into a page or the log")
		}
	}
}
