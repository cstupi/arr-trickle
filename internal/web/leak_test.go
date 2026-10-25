package web

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

// The API key must never leave the process through the UI: not in any page, header, log line or activity
// entry. Instances read a known fake key from all three kinds of source; every page, form and error path
// is then rendered and the key is searched for everywhere the UI could expose it.

func TestKeyNeverAppearsAnywhere(t *testing.T) {
	logs := tu.CaptureLogs(t)
	t.Setenv("ARR_TEST_KEY", tu.Key)
	secret := filepath.Join(t.TempDir(), "secret.key")
	os.WriteFile(secret, []byte(tu.Key), 0o600)
	toml := fmt.Sprintf(`[[sonarr]]
name = "from-xml"
url = "http://sonarr:8989"
api_key_from = %q
[[radarr]]
name = "from-file"
url = "http://radarr:7878"
api_key_file = %q
[[lidarr]]
name = "from-env"
url = "http://lidarr:8686"
api_key_env = "ARR_TEST_KEY"
[[lidarr]]
name = "down"
url = "http://127.0.0.1:%d"
api_key_env = "ARR_TEST_KEY"
timeout_seconds = 5
`, tu.ArrConfigXML(t), secret, tu.FreePort(t))
	// "down" uses the real client against a closed port, so real connection errors are rendered too.
	factory := func(i config.Instance) (runner.Prober, error) {
		if i.Name == "down" {
			return runner.MakeClient(i)
		}
		return lanFactory(i)
	}
	e := newEnv(t, opts{toml: toml, factory: factory})
	b := e.browser(lanIP)
	var responses []resp
	get := func(p string) { responses = append(responses, b.get(p)) }
	post := func(p string, f url.Values, headers ...map[string]string) {
		responses = append(responses, b.post(p, f, headers...))
	}

	// before login: public pages and refusals
	for _, p := range []string{"/login", "/setup", "/healthz", "/", "/settings", "/static/style.css"} {
		get(p)
	}
	b.login()
	post("/login", form("username", "admin", "password", tu.Key))

	e.sched.RunPass() // real searches (fake apps) plus a real connection failure
	e.sched.RunPass()
	for _, p := range []string{"/", "/activity", "/activity?instance=from-env", "/settings", "/nope", "/settings/instances/plex/0"} {
		get(p)
	}
	for kind, n := range map[string]int{"sonarr": 1, "radarr": 1, "lidarr": 2} {
		get("/settings/instances/new?kind=" + kind)
		for i := range n {
			get(fmt.Sprintf("/settings/instances/%s/%d", kind, i))
		}
	}
	for _, name := range []string{"from-xml", "from-file", "from-env", "down"} {
		post("/control/check", form("name", name))
	}
	post("/control/run-now", nil)
	post("/settings", form("interval_minutes", "junk", "log_level", "junk"))
	f := form("kind", "radarr", "url", "http://r:1", "key_source", "api_key_env", "key_ref", "ARR_TEST_KEY")
	post("/settings/instances/new", with(f, "name", "bad name", "api_key", tu.Key)) // key + invalid form
	post("/settings/instances/new", with(f, "name", "typed", "url", "ftp://x", "api_key", tu.Key))
	post("/settings/instances/new", with(f, "name", "stored", "api_key", tu.Key)) // valid: key stored
	get("/settings/instances/radarr/1")
	post("/settings/instances/radarr/1", with(f, "name", "stored", "original_name", "stored", "api_key", tu.Key))
	post("/control/check", form("name", "stored"))
	post("/control/pause", form("csrf", "forged"))
	post("/control/pause", nil, map[string]string{"Origin": "http://evil.example"})
	responses = append(responses, b.do("POST", "/control/pause", tu.Key, map[string]string{"Origin": origin, "Content-Type": "text/plain"}))
	e.srv.pages["dashboard.html"] = nil // an internal error
	get("/")
	post("/logout", nil)
	get("/")

	// positive controls: the key really was read, and the one entered in the UI was stored
	want(t, strings.TrimSpace(fileText(t, filepath.Join(e.state, "secrets", "stored.key"))) == tu.Key, "stored key")
	sawSearch, sawFailure := false, false
	for _, r := range responses {
		sawSearch = sawSearch || strings.Contains(r.text(), "search(es)")
		sawFailure = sawFailure || strings.Contains(r.text(), "connection refused") || strings.Contains(r.text(), "FAIL")
	}
	want(t, sawSearch && sawFailure, "the pages exercised real searches and real failures")

	for _, r := range responses {
		want(t, !strings.Contains(r.text(), tu.Key), "key in body of %s %s", r.method, r.path)
		want(t, !strings.Contains(fmt.Sprint(r.Header()), tu.Key), "key in headers of %s %s", r.method, r.path)
	}
	want(t, !strings.Contains(logs.String(), tu.Key), "key in the log")
	for _, p := range []string{filepath.Join(e.state, "activity.jsonl"), e.config, e.config + ".bak", filepath.Join(e.state, "auth.json")} {
		want(t, !strings.Contains(fileText(t, p), tu.Key), "key in %s", p)
	}
	entries := e.sched.Activity.Entries("")
	want(t, len(entries) > 0, "activity recorded")
	for _, en := range entries {
		want(t, !strings.Contains(fmt.Sprintf("%#v", en), tu.Key), "key in activity")
	}
}
