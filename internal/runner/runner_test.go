package runner

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/client"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/planner"
	"github.com/cstupi/arr-trickle/internal/state"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func inst(kind string, mod func(*config.Instance)) config.Instance {
	i := config.NewInstance(kind, kind, "http://"+kind+":1")
	i.APIKeyEnv, i.Missing = "K", 3
	if mod != nil {
		mod(&i)
	}
	return i
}

func budget(t *testing.T) *state.Budget {
	return state.NewBudget(filepath.Join(t.TempDir(), "budget.json"))
}

func albums(n int) []apps.Record {
	var out []apps.Record
	for i := 1; i <= n; i++ {
		out = append(out, tu.A(i))
	}
	return out
}

func TestSendsMissingAndRecordsBudget(t *testing.T) {
	b := budget(t)
	f := tu.NewFake()
	f.Missing, f.Cutoff, f.Queue = albums(9), []apps.Record{tu.A(20)}, 2
	r := RunInstance(inst("lidarr", nil), f, b, false, tu.Now)
	if r.Spent() != 3 || !r.OK() || r.Skipped != "" {
		t.Fatal(r)
	}
	if !reflect.DeepEqual(f.Sent(), []map[string]any{{"name": "AlbumSearch", "albumIds": []int{1, 2, 3}}}) || b.Used("lidarr") != 3 {
		t.Fatal(f.Sent())
	}
	want := []Search{{"missing", "AlbumSearch", []string{"X - A1", "X - A2", "X - A3"}, 3, false}}
	if !reflect.DeepEqual(r.Searches, want) || *r.MissingTotal != 9 || *r.CutoffTotal != 1 || *r.Queue != 2 {
		t.Fatal(r.Searches)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	logs := tu.CaptureLogs(t)
	b := budget(t)
	f := tu.NewFake()
	f.Missing = []apps.Record{tu.A(1)}
	r := RunInstance(inst("lidarr", nil), f, b, true, tu.Now)
	if !r.Searches[0].DryRun || r.Spent() != 1 || len(f.Sent()) != 0 || b.Used("lidarr") != 0 {
		t.Fatal(r)
	}
	if !strings.Contains(logs.String(), "dry_run=true") {
		t.Fatal(logs.String())
	}
}

func TestSkipsWhenIndexerUnhealthy(t *testing.T) {
	f := tu.NewFake()
	f.Missing = []apps.Record{tu.A(1)}
	f.HealthItems = []map[string]any{{"source": "IndexerStatusCheck", "message": "down"}}
	r := RunInstance(inst("lidarr", nil), f, budget(t), false, tu.Now)
	if r.Spent() != 0 || r.Skipped != "down" || !reflect.DeepEqual(r.Health, []string{"down"}) || *r.MissingTotal != 1 || len(f.Sent()) != 0 {
		t.Fatal(r)
	}
	f.HealthItems = []map[string]any{{"source": "DownloadClientCheck"}}
	if r := RunInstance(inst("lidarr", nil), f, budget(t), false, tu.Now); r.Skipped != "DownloadClientCheck" {
		t.Fatal("a message-less health item is named by its source")
	}
}

func TestIgnoresUnrelatedHealthWarnings(t *testing.T) {
	f := tu.NewFake()
	f.Missing = []apps.Record{tu.A(1)}
	f.HealthItems = []map[string]any{{"source": "UpdateCheck", "message": "update"}}
	if RunInstance(inst("lidarr", nil), f, budget(t), false, tu.Now).Spent() != 1 {
		t.Fatal("unrelated warnings must not block")
	}
}

func TestSkipsWhenQueueFull(t *testing.T) {
	f := tu.NewFake()
	f.Missing, f.Queue = []apps.Record{tu.A(1)}, 50
	r := RunInstance(inst("lidarr", func(i *config.Instance) { i.MaxQueue = 50 }), f, budget(t), false, tu.Now)
	if r.Spent() != 0 || !strings.Contains(r.Skipped, "queue") {
		t.Fatal(r)
	}
}

func TestDailyCapLimitsAcrossRunsAndRestarts(t *testing.T) {
	b := budget(t)
	f := tu.NewFake()
	f.Missing, f.Cutoff = albums(9), []apps.Record{tu.A(20)}
	i := inst("lidarr", func(i *config.Instance) { i.Missing, i.Cutoff, i.DailyCap = 3, 1, 4 })
	RunInstance(i, f, b, false, tu.Now)
	third := RunInstance(i, f, state.NewBudget(b.Path()), false, tu.Now)
	total := 0
	for _, c := range f.Sent() {
		total += planner.QueryCost(c)
	}
	if total != 4 || !third.OK() {
		t.Fatal(total, third)
	}
	if r := RunInstance(i, f, state.NewBudget(b.Path()), false, tu.Now); !strings.Contains(r.Skipped, "daily cap") {
		t.Fatal(r)
	}
}

func TestCutoffSkipsProfilesWithoutUpgrades(t *testing.T) {
	f := tu.For("radarr")
	f.Cutoff, f.Profiles = []apps.Record{tu.Movie(1, true, 4)}, map[int]bool{}
	if RunInstance(inst("radarr", func(i *config.Instance) { i.Missing, i.Cutoff = 0, 2 }), f, budget(t), false, tu.Now).Spent() != 0 {
		t.Fatal("no profile allows upgrades")
	}
}

func TestSonarrSeasonSearchAndLongLogLine(t *testing.T) {
	logs := tu.CaptureLogs(t)
	f := tu.For("sonarr")
	for i := 1; i < 15; i++ {
		f.Missing = append(f.Missing, tu.E(i, 1))
	}
	r := RunInstance(inst("sonarr", func(i *config.Instance) { i.Missing = 1 }), f, budget(t), false, tu.Now)
	if r.Spent() != 1 || !reflect.DeepEqual(f.Sent(), []map[string]any{{"name": "SeasonSearch", "seriesId": 10, "seasonNumber": 1}}) {
		t.Fatal(f.Sent())
	}
	if !strings.Contains(logs.String(), "+4 more") {
		t.Fatal(logs.String())
	}
}

func TestErrorsMidPassAreReportedWithWhatWasSent(t *testing.T) {
	logs := tu.CaptureLogs(t)
	f := tu.NewFake()
	f.Missing, f.Cutoff = []apps.Record{tu.A(1)}, []apps.Record{tu.A(2)}
	f.Err["profiles"] = &client.Error{Msg: "GET /qualityprofile: HTTP 500"}
	r := RunInstance(inst("lidarr", func(i *config.Instance) { i.Cutoff = 1 }), f, budget(t), false, tu.Now)
	if r.OK() || !strings.Contains(r.Error, "HTTP 500") || len(r.Searches) != 1 || r.Searches[0].Command != "AlbumSearch" {
		t.Fatal(r)
	}
	if !strings.Contains(logs.String(), "run failed") {
		t.Fatal(logs.String())
	}
}

func TestEveryClientFailureIsReported(t *testing.T) {
	for _, method := range []string{"health", "queue", "wanted", "command"} {
		f := tu.NewFake()
		f.Missing = []apps.Record{tu.A(1)}
		f.Err[method] = errors.New(method + " broke")
		if r := RunInstance(inst("lidarr", nil), f, budget(t), false, tu.Now); !strings.Contains(r.Error, method) {
			t.Errorf("%s: %+v", method, r)
		}
	}
}

func TestCheckReportsEverything(t *testing.T) {
	res := CheckInstance(inst("lidarr", nil), tu.NewFake())
	text := strings.Join(res.Lines, "\n")
	if !res.OK || !strings.Contains(text, "Lidarr 9.9") || !strings.Contains(text, "wanted/missing") || !strings.Contains(text, "wanted/cutoff") {
		t.Fatal(text)
	}
	f := tu.NewFake()
	f.Profiles = map[int]bool{}
	if !strings.Contains(strings.Join(CheckInstance(inst("lidarr", nil), f).Lines, "\n"), "upgrades: none") {
		t.Fatal("no upgrade profiles")
	}
}

func TestCheckFlagsWrongAppAndIgnoredSort(t *testing.T) {
	f := tu.NewFake()
	f.App = "Sonarr"
	if CheckInstance(inst("lidarr", nil), f).OK {
		t.Fatal("wrong app")
	}
	f = tu.NewFake()
	f.SortKey = "title"
	res := CheckInstance(inst("lidarr", nil), f)
	if res.OK || !strings.Contains(strings.Join(res.Lines, "\n"), "sortKey") {
		t.Fatal(res)
	}
}

func TestCheckReportsBlockingHealthWithoutFailing(t *testing.T) {
	f := tu.NewFake()
	f.HealthItems = []map[string]any{{"source": "IndexerStatusCheck", "message": "down"}}
	res := CheckInstance(inst("lidarr", nil), f)
	if !res.OK || !strings.Contains(strings.Join(res.Lines, "\n"), "would be skipped") {
		t.Fatal(res)
	}
}

func TestCheckCatchesClientErrors(t *testing.T) {
	for _, method := range []string{"status", "health", "queue", "profiles", "wanted"} {
		f := tu.NewFake()
		f.Err[method] = errors.New("HTTP 401 (check the API key)")
		if res := CheckInstance(inst("lidarr", nil), f); res.OK || !strings.Contains(strings.Join(res.Lines, "\n"), "401") {
			t.Errorf("%s: %+v", method, res)
		}
	}
	res := CheckInstance(inst("lidarr", func(i *config.Instance) { i.APIKeyEnv = "UNSET_FOR_TEST" }), nil)
	if res.OK || !strings.Contains(res.Lines[0], "empty") {
		t.Fatal(res)
	}
	if f := Fail("x", "boom"); f.OK || f.Lines[0] != "FAIL boom" {
		t.Fatal(f)
	}
}

func TestMakeClientRejectsBadURL(t *testing.T) {
	t.Setenv("K", tu.Key)
	if _, err := MakeClient(inst("radarr", func(i *config.Instance) { i.URL = "file:///x" })); err == nil {
		t.Fatal("bad scheme")
	}
}
