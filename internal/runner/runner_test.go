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
	r := RunInstance(inst("lidarr", nil), f, b, nil, tu.Now)
	if r.Spent() != 3 || !r.OK() || r.Skipped != "" {
		t.Fatal(r)
	}
	if !reflect.DeepEqual(f.Sent(), []map[string]any{{"name": "AlbumSearch", "albumIds": []int{1, 2, 3}}}) || b.Used("lidarr") != 3 {
		t.Fatal(f.Sent())
	}
	want := []Search{{"missing", "AlbumSearch", []string{"X - A1", "X - A2", "X - A3"}, 3}}
	if !reflect.DeepEqual(r.Searches, want) || *r.MissingTotal != 9 || *r.CutoffTotal != 1 || *r.Queue != 2 {
		t.Fatal(r.Searches)
	}
}

func TestSkipsWhenIndexerUnhealthy(t *testing.T) {
	f := tu.NewFake()
	f.Missing = []apps.Record{tu.A(1)}
	f.HealthItems = []map[string]any{{"source": "IndexerStatusCheck", "message": "down"}}
	r := RunInstance(inst("lidarr", nil), f, budget(t), nil, tu.Now)
	if r.Spent() != 0 || r.Skipped != "down" || !reflect.DeepEqual(r.Health, []string{"down"}) || *r.MissingTotal != 1 || len(f.Sent()) != 0 {
		t.Fatal(r)
	}
	f.HealthItems = []map[string]any{{"source": "DownloadClientCheck"}}
	if r := RunInstance(inst("lidarr", nil), f, budget(t), nil, tu.Now); r.Skipped != "DownloadClientCheck" {
		t.Fatal("a message-less health item is named by its source")
	}
}

func TestIgnoresUnrelatedHealthWarnings(t *testing.T) {
	f := tu.NewFake()
	f.Missing = []apps.Record{tu.A(1)}
	f.HealthItems = []map[string]any{{"source": "UpdateCheck", "message": "update"}}
	if RunInstance(inst("lidarr", nil), f, budget(t), nil, tu.Now).Spent() != 1 {
		t.Fatal("unrelated warnings must not block")
	}
}

func TestSkipsWhenQueueFull(t *testing.T) {
	f := tu.NewFake()
	f.Missing, f.Queue = []apps.Record{tu.A(1)}, 50
	r := RunInstance(inst("lidarr", func(i *config.Instance) { i.MaxQueue = 50 }), f, budget(t), nil, tu.Now)
	if r.Spent() != 0 || !strings.Contains(r.Skipped, "queue") {
		t.Fatal(r)
	}
}

func TestDailyCapLimitsAcrossRunsAndRestarts(t *testing.T) {
	b := budget(t)
	f := tu.NewFake()
	f.Missing, f.Cutoff = albums(9), []apps.Record{tu.A(20)}
	i := inst("lidarr", func(i *config.Instance) { i.Missing, i.Cutoff, i.DailyCap = 3, 1, 4 })
	RunInstance(i, f, b, nil, tu.Now)
	third := RunInstance(i, f, state.NewBudget(b.Path()), nil, tu.Now)
	total := 0
	for _, c := range f.Sent() {
		total += planner.QueryCost(c)
	}
	if total != 4 || !third.OK() {
		t.Fatal(total, third)
	}
	if r := RunInstance(i, f, state.NewBudget(b.Path()), nil, tu.Now); !strings.Contains(r.Skipped, "daily cap") {
		t.Fatal(r)
	}
}

func TestCutoffSkipsProfilesWithoutUpgrades(t *testing.T) {
	f := tu.For("radarr")
	f.Cutoff, f.Profiles = []apps.Record{tu.Movie(1, true, 4)}, map[int]bool{}
	if RunInstance(inst("radarr", func(i *config.Instance) { i.Missing, i.Cutoff = 0, 2 }), f, budget(t), nil, tu.Now).Spent() != 0 {
		t.Fatal("no profile allows upgrades")
	}
}

func TestSonarrSeasonSearchAndLongLogLine(t *testing.T) {
	logs := tu.CaptureLogs(t)
	f := tu.For("sonarr")
	for i := 1; i < 15; i++ {
		f.Missing = append(f.Missing, tu.E(i, 1))
	}
	r := RunInstance(inst("sonarr", func(i *config.Instance) { i.Missing = 1 }), f, budget(t), nil, tu.Now)
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
	r := RunInstance(inst("lidarr", func(i *config.Instance) { i.Cutoff = 1 }), f, budget(t), nil, tu.Now)
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
		if r := RunInstance(inst("lidarr", nil), f, budget(t), nil, tu.Now); !strings.Contains(r.Error, method) {
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

func retiredStore(t *testing.T) *state.Retired {
	return state.NewRetired(filepath.Join(t.TempDir(), "retired.json"))
}

func sentIDs(f *tu.Fake) [][]int {
	var out [][]int
	for _, c := range f.Sent() {
		out = append(out, c["albumIds"].([]int))
	}
	return out
}

func TestRetiresRecordsAfterOneCleanSearch(t *testing.T) {
	f := tu.NewFake()
	f.Missing = albums(7)
	ret, b := retiredStore(t), budget(t)
	i := inst("lidarr", nil)
	var got []int
	for range 4 {
		r := RunInstance(i, f, b, ret, tu.Now)
		got = append(got, r.Retired)
	}
	if !reflect.DeepEqual(sentIDs(f), [][]int{{1, 2, 3}, {4, 5, 6}, {7}}) || !reflect.DeepEqual(got, []int{0, 3, 3, 1}) {
		t.Fatal(sentIDs(f), got)
	}
	if ret.Count("lidarr") != 7 || len(ret.Pending("lidarr")) != 0 {
		t.Fatal(ret.Count("lidarr"))
	}
	ret.Clear("lidarr")
	RunInstance(i, f, b, ret, tu.Now)
	if s := sentIDs(f); !reflect.DeepEqual(s[len(s)-1], []int{1, 2, 3}) {
		t.Fatal("cleared records are searched again", s)
	}
}

func TestOnlyCleanSearchesRetire(t *testing.T) {
	f := tu.NewFake()
	f.Missing = albums(3)
	ret := retiredStore(t)
	i := inst("lidarr", func(i *config.Instance) { i.Missing = 1 })
	f.Statuses = map[int]string{1: "failed", 2: "started", 3: "gone", 4: "unsuccessful"}
	for range 5 {
		RunInstance(i, f, budget(t), ret, tu.Now)
	}
	if ret.Count("lidarr") != 0 || len(ret.Pending("lidarr")) != 2 { // 2 is still running, 5 was just sent
		t.Fatal(ret.Count("lidarr"), ret.Pending("lidarr"))
	}
	f.HealthItems = []map[string]any{{"source": "IndexerStatusCheck", "message": "down"}}
	f.Statuses = nil // both completed, but an indexer may have been down for them
	if r := RunInstance(i, f, budget(t), ret, tu.Now); r.Retired != 0 || len(ret.Pending("lidarr")) != 0 {
		t.Fatal(r, ret.Pending("lidarr"))
	}
	f.HealthItems = nil
	RunInstance(i, f, budget(t), ret, tu.Now) // nothing pending now; queue one more
	f.Err["commandstatus"] = errors.New("status broke")
	if r := RunInstance(i, f, budget(t), ret, tu.Now); !strings.Contains(r.Error, "status broke") {
		t.Fatal(r)
	}
}

func TestReadsPastRetiredRecords(t *testing.T) {
	f := tu.NewFake()
	f.Missing = albums(600)
	ret := retiredStore(t)
	var old []int
	for id := 1; id <= 520; id++ {
		old = append(old, id)
	}
	ret.AddPending("lidarr", state.Pending{Command: 99, Wanted: "missing", IDs: old})
	ret.Resolve("lidarr", 99, true)
	r := RunInstance(inst("lidarr", nil), f, budget(t), ret, tu.Now)
	if !reflect.DeepEqual(sentIDs(f), [][]int{{521, 522, 523}}) || *r.MissingTotal != 600 { // on page 3 of 250
		t.Fatal(sentIDs(f))
	}
	f.Missing = albums(520) // everything retired: reads to the end, sends nothing
	if r := RunInstance(inst("lidarr", nil), f, budget(t), ret, tu.Now); len(r.Searches) != 0 || !r.OK() {
		t.Fatal(r)
	}
}

func TestRetiringSonarrSeasonsAndCutoffSeparately(t *testing.T) {
	f := tu.For("sonarr")
	f.Missing = []apps.Record{tu.E(1, 1), tu.E(2, 1), tu.E(3, 2)}
	f.Cutoff = []apps.Record{tu.E(1, 1)}
	ret := retiredStore(t)
	i := inst("sonarr", func(i *config.Instance) { i.Missing, i.Cutoff = 1, 1 })
	RunInstance(i, f, budget(t), ret, tu.Now)
	RunInstance(i, f, budget(t), ret, tu.Now)
	if !ret.Set("sonarr", "missing")[1] || !ret.Set("sonarr", "missing")[2] || !ret.Set("sonarr", "cutoff")[1] || ret.Set("sonarr", "missing")[3] {
		t.Fatal(ret.Set("sonarr", "missing"), ret.Set("sonarr", "cutoff"))
	}
	if s := f.Sent(); s[2]["seasonNumber"] != 2 || len(s) != 3 { // season 1 + cutoff, then season 2; cutoff retired
		t.Fatal(s)
	}
}
