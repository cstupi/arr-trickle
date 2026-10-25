package web

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	"github.com/cstupi/arr-trickle/internal/service"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

const (
	password = "correct horse battery staple"
	origin   = "http://testserver"
	lanIP    = "192.168.1.10"
	radarr   = "[[radarr]]\nname = \"radarr\"\nurl = \"http://radarr:7878\"\napi_key_env = \"ARR_TEST_KEY\"\n"
)

func TestMain(m *testing.M) {
	scryptN = 1 << 10 // fast hashes for tests; TestScryptHashAndVerify checks the production cost
	os.Exit(m.Run())
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfOf(t testing.TB, html string) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no CSRF field on the page:\n%s", html)
	}
	return m[1]
}

// lanFactory reads the real key (so key handling is exercised), then fakes the app.
func lanFactory(i config.Instance) (runner.Prober, error) {
	if _, err := i.APIKey(nil); err != nil {
		return nil, err
	}
	f := tu.For(i.Kind)
	for n := 1; n <= 3; n++ {
		f.Missing = append(f.Missing, map[string]func(int) map[string]any{"lidarr": tu.A, "radarr": tu.M, "sonarr": func(n int) map[string]any { return tu.E(n, 1) }}[i.Kind](n))
	}
	f.Queue = 1
	return f, nil
}

type env struct {
	t             testing.TB
	srv           *Server
	sched         *service.Scheduler
	config, state string
}

type opts struct {
	toml    string
	factory service.ClientFactory
	noFile  bool // configure from the environment, as without a config.toml
	admin   bool // create the admin account
}

func newEnv(t testing.TB, o opts) *env {
	t.Helper()
	dir := t.TempDir()
	stateDir, cfg := filepath.Join(dir, "state"), filepath.Join(dir, "config", "config.toml")
	for _, d := range []string{stateDir, filepath.Dir(cfg)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	envVars := config.Environ{}
	if o.noFile {
		envVars["STATE_DIR"] = stateDir
	} else if err := os.WriteFile(cfg, fmt.Appendf(nil, "state_dir = %q\n%s", stateDir, o.toml), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(cfg, envVars, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.factory == nil {
		o.factory = lanFactory
	}
	sched := service.New(s, o.factory, nil)
	e := &env{t, New(s, sched, cfg), sched, cfg, stateDir}
	if o.admin {
		e.makeAdmin()
	}
	return e
}

func (e *env) makeAdmin() {
	e.t.Helper()
	if !e.srv.store.HasAdmin() {
		code, _ := e.srv.store.NewSetupCode()
		if err := e.srv.store.CreateAdmin(code, "admin", password, password); err != nil {
			e.t.Fatal(err)
		}
	}
}

// browser keeps cookies like a browser would, for one client address.
type browser struct {
	e       *env
	ip      string
	scheme  string
	cookies map[string]string
	csrf    string
}

func (e *env) browser(ip string) *browser {
	return &browser{e: e, ip: ip, scheme: "http", cookies: map[string]string{}}
}

type resp struct {
	*httptest.ResponseRecorder
	method, path string
}

func (r resp) text() string { return r.Body.String() }

func (b *browser) do(method, path, body string, headers map[string]string) resp {
	req := httptest.NewRequest(method, b.scheme+"://testserver"+path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.RemoteAddr = net.JoinHostPort(b.ip, "50000")
	for name, value := range b.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.e.srv.ServeHTTP(rec, req)
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return resp{rec, method, path}
}

func (b *browser) get(path string) resp { return b.do(http.MethodGet, path, "", nil) }

// with returns a copy of base with the key/value pairs set.
func with(base url.Values, kv ...string) url.Values {
	v := url.Values{}
	maps.Copy(v, base)
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func form(kv ...string) url.Values { return with(nil, kv...) }

// post sends a same-origin form with the session's CSRF token (unless the form sets its own).
func (b *browser) post(path string, f url.Values, headers ...map[string]string) resp {
	if !f.Has("csrf") {
		f = with(f, "csrf", b.csrf)
	}
	h := map[string]string{"Origin": b.scheme + "://testserver"}
	for _, extra := range headers {
		maps.Copy(h, extra)
	}
	return b.do(http.MethodPost, path, f.Encode(), h)
}

func (b *browser) login() *browser {
	b.e.t.Helper()
	b.e.makeAdmin()
	r := b.post("/login", form("csrf", csrfOf(b.e.t, b.get("/login").text()), "username", "admin", "password", password))
	if r.Code != http.StatusSeeOther {
		b.e.t.Fatalf("login failed: %d %s", r.Code, r.text())
	}
	b.csrf = csrfOf(b.e.t, b.get("/").text())
	return b
}

// web is the common setup: one radarr instance, logged in from the LAN.
func web(t testing.TB) (*env, *browser) {
	t.Setenv("ARR_TEST_KEY", tu.Key)
	e := newEnv(t, opts{toml: radarr})
	return e, e.browser(lanIP).login()
}

func want(t testing.TB, cond bool, format string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(format, args...)
	}
}

func secureCookie(r resp) bool {
	return strings.Contains(strings.ToLower(r.Header().Get("Set-Cookie")), "secure")
}

func fileText(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func search(title string) runner.Search {
	return runner.Search{Wanted: "missing", Command: "MoviesSearch", Items: []string{title}, Cost: 1}
}
