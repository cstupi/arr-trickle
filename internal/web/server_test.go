package web

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

// --- route protection ----------------------------------------------------------------

func concrete(pattern string) string {
	p := strings.NewReplacer("{kind}", "radarr", "{index}", "0", "{$}", "").Replace(pattern)
	if p == staticPrefix {
		return "/static/style.css"
	}
	return p
}

func endpoint(rt route) (method, path string) {
	method, path, _ = strings.Cut(rt.pattern, " ")
	return method, concrete(path)
}

func TestEveryRouteRequiresLogin(t *testing.T) {
	for _, rt := range routes { // the route table itself: a new route is covered automatically
		method, path := endpoint(rt)
		r := newEnv(t, opts{admin: true}).browser(lanIP).do(method, path, "", map[string]string{"Origin": origin})
		switch {
		case isPublic(path):
			want(t, r.Code != 401 && r.Header().Get("Location") != "/login", "%s is public but asked for a login", rt.pattern)
		case method == "GET":
			want(t, r.Code == 303 && r.Header().Get("Location") == "/login", "%s: %d %s", rt.pattern, r.Code, r.Header().Get("Location"))
		default:
			want(t, r.Code == 401, "%s: %d", rt.pattern, r.Code)
		}
	}
}

func TestOnlyLoginSetupHealthzAndStaticArePublic(t *testing.T) {
	want(t, slices.Equal(publicPaths, []string{"/login", "/setup", "/healthz"}), "%v", publicPaths)
	for _, p := range publicPaths {
		want(t, slices.ContainsFunc(routes, func(rt route) bool { _, path := endpoint(rt); return path == p }), "public path %s has no route", p)
	}
	want(t, isPublic("/static/style.css") && !isPublic("/staticx") && !isPublic("/settings"), "isPublic")
}

func TestUnknownPathsNeedLoginToo(t *testing.T) {
	e := newEnv(t, opts{})
	r := e.browser(lanIP).get("/nope")
	want(t, r.Code == 303, "unknown path without a session: %d", r.Code)
}

func TestEveryPostNeedsCSRFTokenAndSameOrigin(t *testing.T) {
	for _, rt := range routes {
		method, path := endpoint(rt)
		if method != "POST" || isPublic(path) {
			continue
		}
		_, b := web(t)
		data := form("name", "radarr", "kind", "radarr")
		want(t, b.post(path, form("csrf", "forged", "name", "radarr")).Code == 403, "%s: forged token", path)
		want(t, b.post(path, data, map[string]string{"Origin": "http://evil.example"}).Code == 403, "%s: evil origin", path)
		want(t, b.post(path, data, map[string]string{"Origin": "null"}).Code == 403, "%s: null origin", path)
		want(t, b.post(path, data, map[string]string{"Origin": ""}).Code == 403, "%s: no origin or referer", path)
		want(t, b.post(path, data, map[string]string{"Origin": "", "Referer": "http://evil.example/x"}).Code == 403, "%s: evil referer", path)
	}
}

func TestRefererIsAcceptedWhenOriginIsAbsent(t *testing.T) {
	e, b := web(t)
	r := b.post("/control/pause", nil, map[string]string{"Origin": "", "Referer": origin + "/"})
	want(t, r.Code == 303 && e.sched.Paused(), "%d", r.Code)
}

func TestLoginFormNeedsItsCSRFToken(t *testing.T) {
	e := newEnv(t, opts{admin: true})
	r := e.browser(lanIP).post("/login", form("username", "admin", "password", password))
	want(t, r.Code == 403, "%d", r.Code)
}

func TestBodySizeAndContentTypeLimits(t *testing.T) {
	_, b := web(t)
	want(t, b.post("/settings", form("x", strings.Repeat("a", 70_000))).Code == 413, "too large")
	r := b.do("POST", "/control/pause", `{"csrf":"x"}`, map[string]string{"Origin": origin, "Content-Type": "application/json"})
	want(t, r.Code == 415, "json: %d", r.Code)
	r = b.do("POST", "/control/pause", "--x--", map[string]string{"Origin": origin, "Content-Type": "multipart/form-data; boundary=x"})
	want(t, r.Code == 415, "multipart: %d", r.Code)
	many := form()
	for i := range 150 {
		many.Set(fmt.Sprintf("f%d", i), "x")
	}
	want(t, b.post("/control/check", many).Code == 400, "too many fields")
}

// --- headers -------------------------------------------------------------------------

func TestSecurityHeaders(t *testing.T) {
	_, b := web(t)
	r := b.get("/")
	h := r.Header()
	csp := h.Get("Content-Security-Policy")
	for _, part := range []string{"default-src 'self'", "script-src 'none'", "form-action 'self'", "frame-ancestors 'none'"} {
		want(t, strings.Contains(csp, part), "CSP missing %s", part)
	}
	want(t, h.Get("X-Content-Type-Options") == "nosniff" && h.Get("Referrer-Policy") == "same-origin" && h.Get("X-Frame-Options") == "DENY", "%v", h)
	want(t, h.Get("Cache-Control") == "no-store" && h.Get("Server") == "", "%v", h)
	want(t, !strings.Contains(strings.ToLower(r.text()), "<script"), "no scripts")
}

func TestStaticAssetsArePublicAndCacheable(t *testing.T) {
	e := newEnv(t, opts{})
	r := e.browser(lanIP).get("/static/style.css")
	want(t, r.Code == 200 && r.Header().Get("Cache-Control") == "" && r.Header().Get("X-Content-Type-Options") == "nosniff", "%d %v", r.Code, r.Header())
	want(t, strings.Contains(r.Header().Get("Content-Type"), "text/css"), "css")
	for _, p := range []string{"/static/%2e%2e/config/config.toml", "/static/nope.css", "/static/"} {
		r := e.browser(lanIP).get(p)
		want(t, r.Code == 404 || r.Code == 301 || r.Code == 303, "%s: %d", p, r.Code)
		want(t, !strings.Contains(r.text(), "state_dir"), "%s leaked a file", p)
	}
}

func TestRequestPathsCannotForgeLogLines(t *testing.T) {
	logs := tu.CaptureLogs(t)
	newEnv(t, opts{}).browser(lanIP).get("/x%0Alevel=ERROR%20msg=forged")
	want(t, !strings.Contains(logs.String(), "\nlevel=ERROR msg=forged"), "%s", logs.String())
}

func TestHealthzSaysOnlyOK(t *testing.T) {
	r := newEnv(t, opts{}).browser(lanIP).get("/healthz")
	want(t, r.Code == 200 && r.text() == "ok", "%d %q", r.Code, r.text())
}

// --- first-run setup -----------------------------------------------------------------

func TestFirstRunSetupFlow(t *testing.T) {
	logs := tu.CaptureLogs(t)
	e := newEnv(t, opts{})
	b := e.browser(lanIP)
	want(t, b.get("/").Header().Get("Location") == "/login", "/ -> /login")
	want(t, b.get("/login").Header().Get("Location") == "/setup", "/login -> /setup")
	page := b.get("/setup")
	want(t, page.Code == 200 && strings.Contains(strings.ToLower(page.text()), "setup code"), "setup page")
	token := csrfOf(t, page.text())
	code, _ := e.srv.store.NewSetupCode()
	setup := func(over ...string) resp {
		f := form("csrf", token, "code", code, "username", "admin", "password", password, "confirm", password)
		for i := 0; i+1 < len(over); i += 2 {
			f.Set(over[i], over[i+1])
		}
		return b.post("/setup", f)
	}
	r := setup("code", "AAAAA-BBBBB-CCCCC-DDDDD")
	want(t, r.Code == 400 && strings.Contains(r.text(), "not valid") && strings.Contains(logs.String(), "wrong setup code"), "wrong code")
	r = setup("password", "short", "confirm", "short")
	want(t, r.Code == 400 && strings.Contains(r.text(), "at least 12"), "short password: %s", r.text())
	r = setup("confirm", password+"x")
	want(t, r.Code == 400 && strings.Contains(r.text(), "do not match"), "mismatch")
	want(t, strings.Contains(r.text(), `value="admin"`), "the username is kept")
	r = setup()
	want(t, r.Code == 303 && r.Header().Get("Location") == "/login", "%d", r.Code)
	want(t, strings.Contains(b.get("/login").text(), "Admin account created"), "flash")
	want(t, b.get("/setup").Code == 404 && setup().Code == 404, "setup is gone")
	want(t, !strings.Contains(logs.String(), password), "password logged")
}

func TestSetupCodeIsPrintedOnceAtStartup(t *testing.T) {
	var out strings.Builder
	e := newEnv(t, opts{})
	if err := Announce(e.sched.Settings(), &out); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`Setup code: (\S+)`).FindStringSubmatch(out.String())
	want(t, m != nil && e.srv.store.CheckSetupCode(m[1]), "the code printed works: %s", out.String())
	e.makeAdmin()
	out.Reset()
	Announce(e.sched.Settings(), &out)
	want(t, out.Len() == 0, "no code once the admin exists")
}

func TestSetupIsRateLimited(t *testing.T) {
	e := newEnv(t, opts{})
	e.srv.limiter.free = 2
	b := e.browser(lanIP)
	token := csrfOf(t, b.get("/setup").text())
	for range 2 {
		b.post("/setup", form("csrf", token, "code", "nope"))
	}
	want(t, b.post("/setup", form("csrf", token, "code", "nope")).Code == 429, "throttled")
}

// --- login, sessions -----------------------------------------------------------------

func TestLoginRotatesSessionAndLogoutEndsIt(t *testing.T) {
	e := newEnv(t, opts{admin: true})
	b := e.browser(lanIP)
	b.get("/login")
	pre := b.cookies[cookieName]
	b.login()
	post := b.cookies[cookieName]
	want(t, pre != "" && pre != post, "the id changes on login")
	want(t, b.get("/").Code == 200, "logged in")
	b.cookies[cookieName] = pre
	want(t, b.get("/").Code == 303, "the pre-login id is dead")
	b.cookies[cookieName] = post
	want(t, b.post("/logout", nil).Header().Get("Location") == "/login", "logout")
	b.cookies[cookieName] = post // replaying the old cookie after logout
	want(t, b.get("/").Header().Get("Location") == "/login", "logout ends the session server-side")
}

func TestAlreadyLoggedInLoginPageRedirects(t *testing.T) {
	_, b := web(t)
	want(t, b.get("/login").Header().Get("Location") == "/", "redirect")
}

func TestCookieFlags(t *testing.T) {
	e := newEnv(t, opts{admin: true})
	c := strings.ToLower(e.browser(lanIP).get("/login").Header().Get("Set-Cookie"))
	want(t, strings.Contains(c, "httponly") && strings.Contains(c, "samesite=strict") && strings.Contains(c, "path=/") && !strings.Contains(c, "secure"), "%s", c)
	want(t, secureCookie(newEnv(t, opts{toml: "[web]\ncookie_secure = true\n", admin: true}).browser(lanIP).get("/login")), "cookie_secure")
	hb := newEnv(t, opts{admin: true}).browser(lanIP)
	hb.scheme = "https" // httptest marks https:// requests as TLS
	want(t, secureCookie(hb.get("/login")), "https request")
	hb.login()
	out := hb.post("/logout", nil)
	want(t, secureCookie(out) && strings.Contains(out.Header().Get("Set-Cookie"), "Max-Age=0"), "logout clears the cookie with the same flags: %s", out.Header().Get("Set-Cookie"))
}

func TestSecureCookieAndOriginBehindATrustedTLSProxy(t *testing.T) {
	b := newEnv(t, opts{toml: "[web]\ntrusted_proxies = [\"10.0.0.2\"]\n", admin: true}).browser("10.0.0.2")
	headers := map[string]string{"X-Forwarded-For": lanIP, "X-Forwarded-Proto": "https", "X-Forwarded-Host": "trickle.lan"}
	page := b.do("GET", "/login", "", headers)
	want(t, secureCookie(page), "secure via proxy")
	r := b.post("/login", form("csrf", csrfOf(t, page.text()), "username", "admin", "password", password), headers, map[string]string{"Origin": "https://trickle.lan"})
	want(t, r.Code == 303, "login through the proxy: %d %s", r.Code, r.text())
	want(t, !secureCookie(newEnv(t, opts{admin: true}).browser(lanIP).do("GET", "/login", "", headers)), "the same headers from an untrusted peer are ignored")
}

func TestWrongPasswordIsLoggedWithoutThePassword(t *testing.T) {
	logs := tu.CaptureLogs(t)
	e := newEnv(t, opts{admin: true})
	b := e.browser(lanIP)
	r := b.post("/login", form("csrf", csrfOf(t, b.get("/login").text()), "username", "admin", "password", "hunter2hunter2"))
	want(t, r.Code == 401 && strings.Contains(r.text(), "Wrong username or password"), "%d", r.Code)
	want(t, strings.Contains(logs.String(), "failed login") && strings.Contains(logs.String(), "client="+lanIP) && !strings.Contains(logs.String(), "hunter2"), "%s", logs.String())
}

func TestLoginRateLimitPerIPAndGlobal(t *testing.T) {
	e := newEnv(t, opts{admin: true})
	attempt := func(b *browser, pw string) resp {
		return b.post("/login", form("csrf", csrfOf(t, b.get("/login").text()), "username", "admin", "password", pw))
	}
	b := e.browser(lanIP)
	for range 5 {
		want(t, attempt(b, "wrong password!").Code == 401, "wrong password")
	}
	r := attempt(b, password) // even the right password is refused while backing off
	secs, _ := strconv.Atoi(r.Header().Get("Retry-After"))
	want(t, r.Code == 429 && secs >= 1, "%d %d", r.Code, secs)
	want(t, attempt(e.browser("192.168.1.77"), password).Code == 303, "another client is not affected")
	e.srv.limiter = newRateLimiter()
	e.srv.limiter.globalLimit = 3
	want(t, attempt(e.browser("fd00::1:5"), "wrong").Code == 401, "an IPv6 client...")
	for range 5 {
		attempt(e.browser("fd00::1:6"), "wrong")
	}
	want(t, attempt(e.browser("fd00::1:7"), password).Code == 429, "...is throttled per /64, not per address")
	e.srv.limiter = newRateLimiter()
	e.srv.limiter.globalLimit = 3
	for _, ip := range []string{"192.168.1.1", "192.168.1.2", "192.168.1.3"} {
		e.srv.limiter.failure(ip)
	}
	want(t, attempt(e.browser("192.168.1.99"), password).Code == 429, "global ceiling")
}

func TestSessionIdleAndAbsoluteTimeouts(t *testing.T) {
	e, b := web(t)
	now := time.Now()
	e.srv.sessions.clock = func() time.Time { return now }
	now = now.Add(50 * time.Minute)
	want(t, b.get("/").Code == 200, "active")
	now = now.Add(61 * time.Minute)
	want(t, b.get("/").Header().Get("Location") == "/login", "idle timeout")
	b.login()
	for range 14 { // active every 55 minutes, past the 12 h absolute limit
		now = now.Add(55 * time.Minute)
		b.get("/")
	}
	want(t, b.get("/").Header().Get("Location") == "/login", "absolute timeout")
}

func TestPasswordResetEndsExistingSessions(t *testing.T) {
	e, b := web(t)
	e.srv.store.Reset()
	want(t, b.get("/").Header().Get("Location") == "/login", "reset")
}

// --- network allowlist ---------------------------------------------------------------

func TestClientsOutsideAllowedNetworksAreRefused(t *testing.T) {
	logs := tu.CaptureLogs(t)
	for _, p := range []string{"/healthz", "/login", "/setup", "/", "/static/style.css"} {
		r := newEnv(t, opts{}).browser("8.8.8.8").get(p)
		want(t, r.Code == 403 && r.text() == "forbidden", "%s: %d", p, r.Code)
	}
	want(t, strings.Contains(logs.String(), "not in web.allowed_networks"), "%s", logs.String())
}

func TestAllowedNetworksSetting(t *testing.T) {
	narrow := opts{toml: "[web]\nallowed_networks = [\"192.168.5.0/24\"]\n"}
	want(t, newEnv(t, narrow).browser(lanIP).get("/healthz").Code == 403, "outside")
	want(t, newEnv(t, narrow).browser("192.168.5.5").get("/healthz").text() == "ok", "inside")
	want(t, newEnv(t, opts{}).browser("::1").get("/healthz").Code == 200, "ipv6 loopback")
	want(t, newEnv(t, opts{}).browser("::ffff:192.168.1.4").get("/healthz").Code == 200, "ipv4-mapped")
	want(t, newEnv(t, opts{}).browser("testclient").get("/healthz").Code == 403, "not an address")
}

func TestForwardedForOnlyFromTrustedProxies(t *testing.T) {
	trusted := opts{toml: "[web]\ntrusted_proxies = [\"10.0.0.2\"]\n"}
	xff := func(v string) map[string]string { return map[string]string{"X-Forwarded-For": v} }
	want(t, newEnv(t, trusted).browser("10.0.0.2").do("GET", "/healthz", "", xff("8.8.8.8")).Code == 403, "proxied internet client")
	want(t, newEnv(t, trusted).browser("10.0.0.2").do("GET", "/healthz", "", xff(lanIP)).text() == "ok", "proxied LAN client")
	want(t, newEnv(t, opts{}).browser(lanIP).do("GET", "/healthz", "", xff("8.8.8.8")).text() == "ok", "header ignored from an untrusted peer")
	spoof := newEnv(t, opts{toml: "[web]\nallowed_networks = [\"10.9.0.0/16\"]\n"})
	want(t, spoof.browser("8.8.8.8").do("GET", "/healthz", "", xff("10.9.0.1")).Code == 403, "can't spoof in")
}

// --- dashboard, controls, activity ---------------------------------------------------

func TestDashboardStatusAfterAPass(t *testing.T) {
	e, b := web(t)
	want(t, strings.Contains(b.get("/").text(), "Not checked yet"), "before")
	e.sched.RunPass()
	page := b.get("/").text()
	want(t, strings.Contains(page, "radarr") && strings.Contains(page, "1 search(es)") && strings.Contains(page, "Healthy"), "%s", page)
}

func TestDashboardShowsErrorsAndSkips(t *testing.T) {
	t.Setenv("ARR_TEST_KEY", tu.Key)
	e := newEnv(t, opts{toml: radarr + "[[lidarr]]\nname = \"gone\"\nurl = \"http://l:1\"\napi_key_env = \"UNSET_FOR_TEST\"\n"})
	b := e.browser(lanIP).login()
	e.sched.RunPass()
	page := b.get("/").text()
	want(t, strings.Contains(page, "Unreachable") && strings.Contains(page, "UNSET_FOR_TEST is empty"), "%s", page)
}

func TestDashboardWithNoInstances(t *testing.T) {
	e := newEnv(t, opts{})
	want(t, strings.Contains(e.browser(lanIP).login().get("/").text(), "No instances yet"), "empty")
}

func TestControls(t *testing.T) {
	e, b := web(t)
	want(t, b.post("/control/run-now", nil).Code == 303, "run now")
	b.post("/control/pause", nil)
	want(t, e.sched.Paused() && strings.Contains(b.get("/").text(), "paused"), "pause")
	b.post("/control/resume", nil)
	want(t, !e.sched.Paused(), "resume")
	// Dry run was removed in 0.3.1: no control, nothing about it on the pages.
	want(t, b.post("/control/dry-run", nil).Code == 404, "dry-run route gone")
	for _, page := range []string{"/", "/settings", "/activity"} {
		want(t, !strings.Contains(strings.ToLower(b.get(page).text()), "dry run"), "%s mentions dry run", page)
	}
	want(t, !strings.Contains(strings.ToLower(b.get("/settings").text()), "insecure"), "insecure option shown")
}

func TestRunNowRunsAPassInTheBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, b := web(t)
		e.sched.Start()
		defer e.sched.Stop(time.Second)
		synctest.Wait()
		want(t, len(e.sched.Activity.Entries("")) == 1, "first pass")
		b.post("/control/run-now", nil)
		synctest.Wait()
		want(t, len(e.sched.Activity.Entries("")) == 2, "run now")
	})
}

func TestCheckButton(t *testing.T) {
	_, b := web(t)
	r := b.post("/control/check", form("name", "radarr"))
	want(t, r.Code == 200 && strings.Contains(r.text(), "All checks passed") && strings.Contains(r.text(), "wanted/missing"), "%s", r.text())
	want(t, strings.Contains(b.post("/control/check", form("name", "nope")).text(), "no such instance"), "unknown")
}

func TestActivityPagingAndFilter(t *testing.T) {
	e, b := web(t)
	for i := range 60 {
		name := "other"
		if i%2 == 1 {
			name = "radarr"
		}
		e.sched.Activity.Record(name, search(fmt.Sprintf("title %d", i)), time.Time{})
	}
	p1 := b.get("/activity").text()
	want(t, strings.Contains(p1, "title 59") && !strings.Contains(p1, "title 9<") && strings.Contains(p1, "Page 1 of 2"), "page 1")
	p2 := b.get("/activity?page=2").text()
	want(t, strings.Contains(p2, "title 0") && !strings.Contains(p2, "title 59"), "page 2")
	f := b.get("/activity?instance=radarr").text()
	want(t, strings.Contains(f, "title 59") && !strings.Contains(f, "title 58") && strings.Contains(f, "Page 1 of 1"), "filter")
	want(t, strings.Contains(b.get("/activity?page=99").text(), "Page 2 of 2"), "clamped")
	want(t, strings.Contains(b.get("/activity?page=abc").text(), "Page 1 of 2"), "junk page")
	want(t, strings.Contains(p1, `href="/activity?page=2"`), "older link")
}

func TestActivityEscapesTitles(t *testing.T) {
	e, b := web(t)
	e.sched.Activity.Record("radarr", search("<script>alert(1)</script>"), time.Time{})
	page := b.get("/activity").text()
	want(t, !strings.Contains(page, "<script>") && strings.Contains(page, "&lt;script&gt;"), "escaped")
}

// --- errors --------------------------------------------------------------------------

func TestErrorPagesAreGeneric(t *testing.T) {
	logs := tu.CaptureLogs(t)
	e, b := web(t)
	r := b.get("/nope")
	want(t, r.Code == 404 && strings.Contains(r.text(), "Not found"), "404")
	e.srv.pages["dashboard.html"] = nil // make the dashboard blow up
	r = b.get("/")
	want(t, r.Code == 500 && !strings.Contains(r.text(), "nil pointer") && !strings.Contains(r.text(), "goroutine"), "%s", r.text())
	want(t, r.Header().Get("Cache-Control") == "no-store" && r.Header().Get("Content-Security-Policy") != "", "headers on 500")
	want(t, strings.Contains(logs.String(), "internal error") && strings.Contains(logs.String(), "path=/"), "%s", logs.String())
}

func TestBusyServerRefusesPolitely(t *testing.T) {
	e, b := web(t)
	for range cap(e.srv.inflight) {
		e.srv.inflight <- struct{}{}
	}
	want(t, b.get("/healthz").Code == 503, "busy")
}

func TestPasswordHashesAreCapped(t *testing.T) {
	b := newEnv(t, opts{admin: true}).browser(lanIP)
	token := csrfOf(t, b.get("/login").text())
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	defer func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	}()
	r := b.post("/login", form("csrf", token, "username", "admin", "password", password))
	want(t, r.Code == 503, "a third concurrent hash must wait: %d", r.Code)
}

func TestHTTPServerHasTimeouts(t *testing.T) {
	e, _ := web(t)
	s := e.srv.HTTPServer("127.0.0.1:0")
	want(t, s.ReadHeaderTimeout > 0 && s.ReadTimeout > 0 && s.IdleTimeout > 0 && s.MaxHeaderBytes > 0, "%+v", s)
}
