package web

import (
	"strings"
	"testing"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

const withSeerr = radarr + `
[requests]
seerr_url = "http://seerr:5055"
seerr_key_env = "ARR_TEST_KEY"
`

func requestsWeb(t *testing.T, toml string) (*env, *browser) {
	t.Helper()
	t.Setenv("ARR_TEST_KEY", tu.Key)
	e := newEnv(t, opts{toml: toml})
	return e, e.browser(lanIP).login()
}

func TestRequestSyncCard(t *testing.T) {
	_, b := requestsWeb(t, withSeerr)
	page := b.get("/settings").text()
	want(t, strings.Contains(page, "Request sync") && strings.Contains(page, "Re-add missing requests"), "card")
	want(t, strings.Contains(page, "http://seerr:5055, key env <code>ARR_TEST_KEY</code>"), "connection shown read-only")
	want(t, strings.Contains(page, "not yet") && strings.Contains(page, ">off<"), "off, never run")
	want(t, !strings.Contains(page, "/control/sync-requests"), "no Sync now while off")

	_, plain := web(t)
	want(t, strings.Contains(plain.get("/settings").text(), "no request app configured"), "hint without apps")
}

func TestRequestSyncSave(t *testing.T) {
	e, b := requestsWeb(t, withSeerr)
	r := b.post("/settings/requests", form("enabled", "on", "interval_minutes", "60", "lookback_days", "7", "grace_minutes", "5"))
	want(t, r.Code == 303, "%d %s", r.Code, r.text())
	rs := e.sched.Settings().Requests
	want(t, rs.Enabled && rs.LookbackDays == 7 && rs.GraceMinutes == 5 && rs.Apps["seerr"].URL == "http://seerr:5055", "%+v", rs)
	saved := fileText(t, e.config)
	want(t, strings.Contains(saved, "[requests]\nenabled = true\nlookback_days = 7"), "%s", saved)
	page := b.get("/settings").text()
	want(t, strings.Contains(page, "Request sync settings saved") && strings.Contains(page, "/control/sync-requests"), "saved; Sync now offered")

	r = b.post("/control/sync-requests", form())
	want(t, r.Code == 303 && strings.Contains(b.get("/settings").text(), "Request sync started"), "sync now")

	r = b.post("/settings/requests", form("interval_minutes", "60", "lookback_days", "7", "grace_minutes", "5"))
	want(t, r.Code == 303 && !e.sched.Settings().Requests.Enabled, "turned off")
	r = b.post("/control/sync-requests", form())
	want(t, r.Code == 303 && strings.Contains(b.get("/settings").text(), "Request sync is off"), "sync now while off")
}

func TestRequestSyncValidation(t *testing.T) {
	e, b := web(t) // no request app configured
	before := fileText(t, e.config)
	r := b.post("/settings/requests", form("enabled", "on", "interval_minutes", "60", "lookback_days", "0", "grace_minutes", "x"))
	want(t, r.Code == 400, "%d", r.Code)
	page := r.text()
	want(t, strings.Contains(after(page, "enabled"), "Set seerr_url or ombi_url"), "%s", after(page, "enabled"))
	want(t, strings.Contains(after(page, "lookback_days"), "Must be between 1 and 365"), "%s", after(page, "lookback_days"))
	want(t, strings.Contains(after(page, "grace_minutes"), "Must be an integer"), "%s", after(page, "grace_minutes"))
	want(t, strings.Contains(page, `name="lookback_days" value="0"`), "keeps what was typed")
	want(t, fileText(t, e.config) == before && !e.sched.Settings().Requests.Enabled, "unchanged")
}
