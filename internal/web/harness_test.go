package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cstupi/arr-trickle/internal/apps"
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
		switch i.Kind {
		case "lidarr":
			f.Missing = append(f.Missing, tu.A(n))
		case "radarr":
			f.Missing = append(f.Missing, tu.M(n))
		default:
			f.Missing = append(f.Missing, tu.E(n, 1))
		}
	}
	f.Queue = 1
	return f, nil
}

type env struct {
	t      testing.TB
	srv    *Server
	sched  *service.Scheduler
	config string
	state  string
}

type opts struct {
	toml    string
	factory service.ClientFactory
	noFile  bool
}

func newEnv(t testing.TB, o opts) *env {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgDir := filepath.Join(dir, "config")
	os.MkdirAll(stateDir, 0o755)
	os.MkdirAll(cfgDir, 0o755)
	cfg := filepath.Join(cfgDir, "config.toml")
	var s config.Settings
	var err error
	if o.noFile {
		s, err = config.Load(cfg, config.Environ{"STATE_DIR": stateDir}, true)
	} else {
		os.WriteFile(cfg, []byte(fmt.Sprintf("state_dir = %q\n%s", stateDir, o.toml)), 0o644)
		s, err = config.Load(cfg, config.Environ{}, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	if o.factory == nil {
		o.factory = lanFactory
	}
	sched := service.New(s, o.factory, nil)
	return &env{t, New(s, sched, cfg), sched, cfg, stateDir}
}

func (e *env) makeAdmin() {
	if !e.srv.Store.HasAdmin() {
		code, _ := e.srv.Store.NewSetupCode()
		if err := e.srv.Store.CreateAdmin(code, "admin", password, password); err != nil {
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

func (b *browser) do(method, path string, body string, headers map[string]string) resp {
	var req *http.Request
	if body != "" || method == "POST" {
		req = httptest.NewRequest(method, b.scheme+"://testserver"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, b.scheme+"://testserver"+path, nil)
	}
	if b.scheme != "https" {
		req.TLS = nil
	}
	req.RemoteAddr = b.ip + ":50000"
	if strings.Contains(b.ip, ":") {
		req.RemoteAddr = "[" + b.ip + "]:50000"
	}
	for name, value := range b.cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
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

func (b *browser) get(path string) resp { return b.do("GET", path, "", nil) }

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

// post sends a same-origin form with the session's CSRF token (unless the form sets its own).
func (b *browser) post(path string, f url.Values, headers ...map[string]string) resp {
	if f == nil {
		f = url.Values{}
	}
	if !f.Has("csrf") {
		f.Set("csrf", b.csrf)
	}
	h := map[string]string{"Origin": origin}
	if b.scheme == "https" {
		h["Origin"] = "https://testserver"
	}
	for _, extra := range headers {
		for k, v := range extra {
			h[k] = v
		}
	}
	return b.do("POST", path, f.Encode(), h)
}

func (b *browser) login() *browser {
	b.e.makeAdmin()
	page := b.get("/login")
	r := b.post("/login", form("csrf", csrfOf(b.e.t, page.text()), "username", "admin", "password", password))
	if r.Code != 303 {
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

func search(title string) runner.Search {
	return runner.Search{Wanted: "missing", Command: "MoviesSearch", Items: []string{title}, Cost: 1}
}

var _ = apps.Kinds

func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	return string(raw), err
}
