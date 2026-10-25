// Package web is the web UI: server-rendered pages, no JavaScript.
//
// Every request passes the guard first, which enforces, in order: the client network allowlist, login
// (deny by default: only PublicPaths and /static/ are reachable without a session), and for every
// non-GET request a same-origin check plus the session's CSRF token. Pages never receive an API key:
// the templates only get settings whose key sources are paths or variable names.
package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cstupi/arr-trickle/internal/apps"
	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/keystore"
	"github.com/cstupi/arr-trickle/internal/service"
	"github.com/cstupi/arr-trickle/internal/version"
)

//go:embed templates/*.html static/*.css
var assets embed.FS

// Cookie is the session cookie's name.
const Cookie = "arr_trickle_session"

const (
	maxBody   = 64 * 1024
	maxFields = 100
	pageSize  = 50
	// staticPrefix is reachable without a session (the stylesheet).
	staticPrefix = "/static/"
)

// PublicPaths are the only routes reachable without a session.
var PublicPaths = map[string]bool{"/login": true, "/setup": true, "/healthz": true}

// SecurityHeaders go on every response.
var SecurityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; script-src 'none'; object-src 'none'; base-uri 'none'; " +
		"frame-ancestors 'none'; form-action 'self'",
	"X-Content-Type-Options": "nosniff",
	"X-Frame-Options":        "DENY",
	// same-origin, not no-referrer: with no-referrer, browsers send `Origin: null` on form posts,
	// which would defeat the same-origin check. Nothing is ever sent to another site.
	"Referrer-Policy":              "same-origin",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
}

// Route is one entry in the route table.
type Route struct {
	Method, Pattern string
	handle          func(*Server, http.ResponseWriter, *http.Request)
}

// Routes is the whole route table. The guard protects every route not in PublicPaths, so a new route
// is protected automatically, and a test walks this table to prove it.
var Routes = []Route{
	{"GET", "/healthz", (*Server).healthz},
	{"GET", "/login", (*Server).loginPage},
	{"POST", "/login", (*Server).loginSubmit},
	{"GET", "/setup", (*Server).setupPage},
	{"POST", "/setup", (*Server).setupSubmit},
	{"POST", "/logout", (*Server).logout},
	{"GET", "/{$}", (*Server).dashboard},
	{"POST", "/control/run-now", (*Server).runNow},
	{"POST", "/control/pause", (*Server).pause},
	{"POST", "/control/resume", (*Server).resume},
	{"POST", "/control/dry-run", (*Server).toggleDryRun},
	{"POST", "/control/check", (*Server).check},
	{"GET", "/activity", (*Server).activity},
	{"GET", "/settings", (*Server).settingsPage},
	{"POST", "/settings", (*Server).settingsSave},
	{"GET", "/settings/instances/new", (*Server).instanceNew},
	{"POST", "/settings/instances/new", (*Server).instanceCreate},
	{"GET", "/settings/instances/{kind}/{index}", (*Server).instanceEdit},
	{"POST", "/settings/instances/{kind}/{index}", (*Server).instanceUpdate},
	{"POST", "/settings/instances/{kind}/{index}/delete", (*Server).instanceDelete},
	{"GET", staticPrefix, (*Server).static},
}

// Server is the web UI.
type Server struct {
	Scheduler    *service.Scheduler
	Editor       *Editor
	Store        *AuthStore
	Sessions     *Sessions
	Limiter      *RateLimiter
	Allowed      []netip.Prefix
	Trusted      []netip.Prefix
	CookieSecure bool
	mux          *http.ServeMux
	inflight     chan struct{}
	pages        map[string]*template.Template
}

// New builds the UI for the given settings; configPath is where Settings saves config.toml.
func New(s config.Settings, sched *service.Scheduler, configPath string) *Server {
	srv := &Server{
		Scheduler:    sched,
		Editor:       &Editor{Path: configPath, Scheduler: sched},
		Store:        NewAuthStore(s.StateDir),
		Sessions:     NewSessions(time.Duration(s.Web.SessionHours)*time.Hour, time.Hour),
		Limiter:      NewRateLimiter(),
		Allowed:      Networks(s.Web.AllowedNetworks),
		Trusted:      Networks(s.Web.TrustedProxies),
		CookieSecure: s.Web.CookieSecure,
		mux:          http.NewServeMux(),
		inflight:     make(chan struct{}, 64),
		pages:        parseTemplates(),
	}
	for _, rt := range Routes {
		h := rt.handle
		srv.mux.HandleFunc(rt.Method+" "+rt.Pattern, func(w http.ResponseWriter, r *http.Request) { h(srv, w, r) })
	}
	srv.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { srv.errorPage(w, r, http.StatusNotFound) })
	return srv
}

// HTTPServer wraps the UI with conservative timeouts and limits.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      15 * time.Minute, // Check can wait on several slow app requests
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
}

type reqState struct {
	ip      string
	https   bool
	host    string
	session *Session
}

type ctxKey struct{}

func stateOf(r *http.Request) *reqState { return r.Context().Value(ctxKey{}).(*reqState) }

// IsPublic reports whether path is reachable without a session.
func IsPublic(path string) bool { return PublicPaths[path] || strings.HasPrefix(path, staticPrefix) }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func plain(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// ServeHTTP is the guard: allowlist, login, origin and CSRF checks, security headers, access log.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w := &statusWriter{ResponseWriter: rw}
	start := time.Now()
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, ok := ClientIP(peer, r.Header.Get("X-Forwarded-For"), s.Trusted)
	defer func() {
		if p := recover(); p != nil { // generic page only: no panic text reaches the browser
			slog.Error(fmt.Sprintf("web: internal error handling %s %s: %v", r.Method, r.URL.Path, p))
			if w.status == 0 {
				plain(w, http.StatusInternalServerError, "Internal error. The details are in the server log.")
			}
		}
		slog.Info(fmt.Sprintf("%s - \"%s %s\" %d %dms", ip, r.Method, r.URL.Path, w.status, time.Since(start).Milliseconds()))
	}()
	h := w.Header()
	for k, v := range SecurityHeaders {
		h.Set(k, v)
	}
	if !strings.HasPrefix(r.URL.Path, staticPrefix) {
		h.Set("Cache-Control", "no-store")
	}
	if !ok || !InAny(ip, s.Allowed) {
		shown := peer
		if ok {
			shown = ip.String()
		}
		slog.Warn(fmt.Sprintf("web: refused a request from %s (not in web.allowed_networks)", shown))
		plain(w, http.StatusForbidden, "forbidden")
		return
	}
	select { // a cheap concurrency ceiling, like a web server's connection limit
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		plain(w, http.StatusServiceUnavailable, "busy")
		return
	}

	peerIP, _ := ParseIP(peer)
	viaProxy := InAny(peerIP, s.Trusted)
	st := &reqState{ip: ip.String(), https: r.TLS != nil, host: r.Host}
	if viaProxy {
		st.https = st.https || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			st.host = fh
		}
	}
	var sess *Session
	if c, err := r.Cookie(Cookie); err == nil {
		sess = s.Sessions.Get(c.Value)
	}
	if sess != nil && sess.Authenticated() {
		if cred, ok := s.Store.CredentialID(); !ok || cred != sess.CredentialID {
			s.Sessions.Delete(sess.ID) // the password was reset: every old session ends
			sess = nil
		}
	}
	st.session = sess
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))

	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !IsPublic(r.URL.Path) && (sess == nil || !sess.Authenticated()) {
		if safe {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			plain(w, http.StatusUnauthorized, "unauthorized")
		}
		return
	}
	if !safe {
		if status, msg := s.checkPost(r, st); status != 0 {
			plain(w, status, msg)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// checkPost enforces same-origin, the content type, the size limit and the CSRF token, then makes the
// (already read) body available to the handler again.
func (s *Server) checkPost(r *http.Request, st *reqState) (int, string) {
	if !sameOrigin(r, st) {
		slog.Warn(fmt.Sprintf("web: refused a cross-origin %s %s from %s", r.Method, r.URL.Path, st.ip))
		return http.StatusForbidden, "cross-origin request refused"
	}
	ctype, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if !strings.EqualFold(strings.TrimSpace(ctype), "application/x-www-form-urlencoded") {
		return http.StatusUnsupportedMediaType, "unsupported content type"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return http.StatusRequestEntityTooLarge, "request too large"
	}
	values, _ := url.ParseQuery(string(body))
	if st.session == nil || !hmac.Equal([]byte(values.Get("csrf")), []byte(st.session.CSRF)) {
		return http.StatusForbidden, "the form expired or is invalid; go back, reload the page and try again"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return 0, ""
}

func sameOrigin(r *http.Request, st *reqState) bool {
	scheme := "http"
	if st.https {
		scheme = "https"
	}
	expected := strings.ToLower(scheme + "://" + st.host)
	if origin := r.Header.Get("Origin"); origin != "" {
		return strings.ToLower(origin) == expected
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && strings.ToLower(u.Scheme+"://"+u.Host) == expected
	}
	return false
}

// --- helpers ---------------------------------------------------------------------

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func parseTemplates() map[string]*template.Template {
	funcs := template.FuncMap{
		"when": fmtTime,
		"join": strings.Join,
		"num": func(p *int) string {
			if p == nil {
				return "–"
			}
			return strconv.Itoa(*p)
		},
		"capitalize": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"add":  func(a, b int) int { return a + b },
		"port": func(kind string) int { return map[string]int{"sonarr": 8989, "radarr": 7878, "lidarr": 8686}[kind] },
		"owner": func(kind string) string {
			return map[string]string{"sonarr": "series", "radarr": "movie", "lidarr": "artist"}[kind]
		},
		"activityTime": func(s string) string { return strings.TrimSuffix(strings.Replace(s, "T", " ", 1), "+00:00") },
	}
	pages := map[string]*template.Template{}
	names, _ := fs.Glob(assets, "templates/*.html")
	for _, n := range names {
		name := strings.TrimPrefix(n, "templates/")
		if name == "base.html" {
			continue
		}
		pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(assets, "templates/base.html", n))
	}
	return pages
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, data map[string]any) {
	st := stateOf(r)
	base := map[string]any{"CSRF": "", "Authenticated": false, "Flash": []string(nil), "Version": version.Version}
	if st.session != nil {
		base["CSRF"] = st.session.CSRF
		base["Authenticated"] = st.session.Authenticated()
		base["Flash"] = s.Sessions.TakeFlash(st.session)
	}
	for k, v := range data {
		base[k] = v
	}
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "base.html", base); err != nil {
		panic(fmt.Sprintf("template %s: %v", name, err))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int) {
	title := map[int]string{404: "Not found", 405: "Method not allowed"}[status]
	if title == "" {
		title = "Error"
	}
	s.render(w, r, "error.html", status, map[string]any{"Title": title})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, sess *Session) {
	http.SetCookie(w, &http.Cookie{
		Name: Cookie, Value: sess.ID, Path: "/", MaxAge: int(s.Sessions.Absolute.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.CookieSecure || stateOf(r).https,
	})
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, flash string) {
	if sess := stateOf(r).session; sess != nil && flash != "" {
		s.Sessions.AddFlash(sess, flash)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// anonSession is the current session, or a new not-logged-in one (it carries the CSRF token for login/setup).
func (s *Server) anonSession(r *http.Request) (*Session, bool) {
	st := stateOf(r)
	if st.session != nil {
		return st.session, false
	}
	st.session = s.Sessions.Create("")
	return st.session, true
}

func formOf(r *http.Request) (Form, bool) {
	if err := r.ParseForm(); err != nil || len(r.PostForm) > maxFields {
		return nil, false
	}
	f := Form{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			f[k] = v[0]
		}
	}
	return f, true
}

func (s *Server) form(w http.ResponseWriter, r *http.Request) (Form, bool) {
	f, ok := formOf(r)
	if !ok {
		plain(w, http.StatusBadRequest, "bad form")
	}
	return f, ok
}

func capitalized(msg string) string {
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// --- public pages ----------------------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) { plain(w, http.StatusOK, "ok") }

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	sess, created := s.anonSession(r)
	if sess.Authenticated() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.Store.HasAdmin() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if created {
		s.setCookie(w, r, sess)
	}
	s.render(w, r, "login.html", http.StatusOK, nil)
}

func (s *Server) tooMany(w http.ResponseWriter, r *http.Request, page string, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.render(w, r, page, http.StatusTooManyRequests, map[string]any{"Error": fmt.Sprintf("Too many failed attempts. Try again in %d s.", secs)})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	st := stateOf(r)
	if wait := s.Limiter.RetryAfter(st.ip); wait > 0 {
		slog.Warn(fmt.Sprintf("web: login from %s refused: too many failed attempts", st.ip))
		s.tooMany(w, r, "login.html", wait)
		return
	}
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	cred, ok := s.Store.VerifyLogin(f["username"], f["password"])
	if !ok {
		s.Limiter.Failure(st.ip)
		slog.Warn("web: failed login from " + st.ip) // never the password (nor the username, which may be one)
		s.render(w, r, "login.html", http.StatusUnauthorized, map[string]any{"Error": "Wrong username or password."})
		return
	}
	s.Limiter.Success(st.ip)
	if st.session != nil {
		s.Sessions.Delete(st.session.ID) // a fresh id on login: a planted pre-login id is worthless
	}
	st.session = s.Sessions.Create(cred)
	slog.Info("web: login from " + st.ip)
	s.setCookie(w, r, st.session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.Store.HasAdmin() {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	sess, created := s.anonSession(r)
	if created {
		s.setCookie(w, r, sess)
	}
	s.render(w, r, "setup.html", http.StatusOK, map[string]any{"Username": ""})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	st := stateOf(r)
	if s.Store.HasAdmin() {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	if wait := s.Limiter.RetryAfter(st.ip); wait > 0 {
		s.tooMany(w, r, "setup.html", wait)
		return
	}
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	if !s.Store.CheckSetupCode(f["code"]) {
		s.Limiter.Failure(st.ip)
		slog.Warn("web: wrong setup code from " + st.ip)
		s.render(w, r, "setup.html", http.StatusBadRequest, map[string]any{"Error": "The setup code is not valid. Copy it from the log.", "Username": f["username"]})
		return
	}
	if err := s.Store.CreateAdmin(f["code"], f["username"], f["password"], f["confirm"]); err != nil {
		s.render(w, r, "setup.html", http.StatusBadRequest, map[string]any{"Error": capitalized(err.Error()), "Username": f["username"]})
		return
	}
	slog.Info("web: admin account created from " + st.ip)
	s.redirect(w, r, "/login", "Admin account created. Log in.")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := stateOf(r).session; sess != nil {
		s.Sessions.Delete(sess.ID)
	}
	http.SetCookie(w, &http.Cookie{Name: Cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard and controls ------------------------------------------------------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "dashboard.html", http.StatusOK, map[string]any{"Snap": s.Scheduler.Snapshot()})
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	s.Scheduler.RunNow()
	s.redirect(w, r, "/", "A pass will start in a moment.")
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	msg := "Scheduled passes paused. Run now still works."
	if err := s.Scheduler.Pause(); err != nil {
		msg = "Could not pause: " + err.Error()
	}
	s.redirect(w, r, "/", msg)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	msg := "Scheduled passes resumed."
	if err := s.Scheduler.Resume(); err != nil {
		msg = "Could not resume: " + err.Error()
	}
	s.redirect(w, r, "/", msg)
}

func (s *Server) toggleDryRun(w http.ResponseWriter, r *http.Request) {
	s.Editor.Lock()
	defer s.Editor.Unlock()
	data := config.ToData(s.Scheduler.Settings())
	data["dry_run"] = !s.Scheduler.Settings().DryRun
	n, err := s.Editor.Save(data)
	if err != nil {
		s.redirect(w, r, "/", err.Error())
		return
	}
	state := "off"
	if n.DryRun {
		state = "on"
	}
	s.redirect(w, r, "/", "Dry run "+state+".")
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	s.render(w, r, "check.html", http.StatusOK, map[string]any{"Result": s.Scheduler.Check(f["name"])})
}

// --- activity --------------------------------------------------------------------

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	instance := r.URL.Query().Get("instance")
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	entries := s.Scheduler.Activity.Entries(instance)
	pages := max(1, (len(entries)+pageSize-1)/pageSize)
	page = min(page, pages)
	link := func(p int) string {
		q := url.Values{}
		if instance != "" {
			q.Set("instance", instance)
		}
		q.Set("page", strconv.Itoa(p))
		return "/activity?" + q.Encode()
	}
	var names []string
	for _, i := range s.Scheduler.Settings().Instances {
		names = append(names, i.Name)
	}
	data := map[string]any{
		"Entries": entries[(page-1)*pageSize : min(len(entries), page*pageSize)],
		"Page":    page, "Pages": pages, "Instance": instance, "Names": names,
		"Newer": "", "Older": "",
	}
	if page > 1 {
		data["Newer"] = link(page - 1)
	}
	if page < pages {
		data["Older"] = link(page + 1)
	}
	s.render(w, r, "activity.html", http.StatusOK, data)
}

// --- settings --------------------------------------------------------------------

type instanceRow struct {
	config.Instance
	Index   int
	Managed bool
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, nil, nil, nil)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, values map[string]any, errs Errors, general []string) {
	settings := s.Scheduler.Settings()
	if values == nil {
		values = map[string]any{
			"interval_minutes": strconv.Itoa(settings.IntervalMinutes), "log_level": settings.LogLevel,
			"dry_run": settings.DryRun, "allow_insecure_http": settings.AllowInsecureHTTP,
		}
	}
	var rows []instanceRow
	for _, kind := range apps.Kinds {
		n := 0
		for _, i := range settings.Instances {
			if i.Kind == kind {
				rows = append(rows, instanceRow{i, n, i.APIKeyFile != "" && keystore.IsManaged(settings.StateDir, i.APIKeyFile)})
				n++
			}
		}
	}
	if errs == nil {
		errs = Errors{}
	}
	s.render(w, r, "settings.html", status, map[string]any{
		"Settings": settings, "Rows": rows, "Path": s.Editor.Path, "Problem": s.Editor.Problem(),
		"Kinds": apps.Kinds, "Values": values, "Errors": errs, "General": general,
		"LogLevels": []string{"DEBUG", "INFO", "WARNING", "ERROR"},
	})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	s.Editor.Lock()
	defer s.Editor.Unlock()
	data := config.ToData(s.Scheduler.Settings())
	ApplyGlobal(data, f)
	if _, err := s.Editor.Save(data); err != nil {
		errs, general := FieldErrors(err, "")
		values := map[string]any{
			"interval_minutes": f["interval_minutes"], "log_level": f["log_level"],
			"dry_run": f["dry_run"] == "on", "allow_insecure_http": f["allow_insecure_http"] == "on",
		}
		s.renderSettings(w, r, http.StatusBadRequest, values, errs, general)
		return
	}
	s.redirect(w, r, "/settings", "Settings saved.")
}

func (s *Server) renderInstance(w http.ResponseWriter, r *http.Request, kind string, index int, values Form, status int, errs Errors, general []string) {
	settings := s.Scheduler.Settings()
	if errs == nil {
		errs = Errors{}
	}
	action := "/settings/instances/new"
	if index >= 0 {
		action = fmt.Sprintf("/settings/instances/%s/%d", kind, index)
	}
	if index >= 0 && values["original_name"] == "" {
		values["original_name"] = values["name"]
	}
	s.render(w, r, "instance.html", status, map[string]any{
		"Kind": kind, "Edit": index >= 0, "Action": action, "Values": values, "Sources": KeySources,
		"Managed": values["key_source"] == "api_key_file" && keystore.IsManaged(settings.StateDir, values["key_ref"]),
		"Errors":  errs, "General": general,
	})
}

func (s *Server) instancesOf(kind string) []config.Instance {
	var out []config.Instance
	for _, i := range s.Scheduler.Settings().Instances {
		if i.Kind == kind {
			out = append(out, i)
		}
	}
	return out
}

// locate resolves {kind}/{index}, or writes a 404.
func (s *Server) locate(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	kind := r.PathValue("kind")
	index, err := strconv.Atoi(r.PathValue("index"))
	if !apps.IsKnown(kind) || err != nil || index < 0 || index >= len(s.instancesOf(kind)) {
		s.errorPage(w, r, http.StatusNotFound)
		return "", 0, false
	}
	return kind, index, true
}

func (s *Server) instanceNew(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if !apps.IsKnown(kind) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	s.renderInstance(w, r, kind, -1, InstanceValues(nil), http.StatusOK, nil, nil)
}

func (s *Server) instanceEdit(w http.ResponseWriter, r *http.Request) {
	kind, index, ok := s.locate(w, r)
	if !ok {
		return
	}
	inst := s.instancesOf(kind)[index]
	s.renderInstance(w, r, kind, index, InstanceValues(&inst), http.StatusOK, nil, nil)
}

func (s *Server) instanceCreate(w http.ResponseWriter, r *http.Request) {
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	if !apps.IsKnown(f["kind"]) {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	s.saveInstance(w, r, f, f["kind"], -1)
}

func (s *Server) instanceUpdate(w http.ResponseWriter, r *http.Request) {
	kind, index, ok := s.locate(w, r)
	if !ok {
		return
	}
	if f, ok := s.form(w, r); ok {
		s.saveInstance(w, r, f, kind, index)
	}
}

func entriesOf(data map[string]any, kind string) []any {
	list, _ := data[kind].([]any)
	return list
}

func (s *Server) saveInstance(w http.ResponseWriter, r *http.Request, f Form, kind string, index int) {
	key := f["api_key"] // write-only: validated, stored in the state dir, never rendered or logged
	values := Form{}
	for k, v := range f {
		if k != "api_key" && k != "csrf" {
			values[k] = v
		}
	}
	s.Editor.Lock()
	defer s.Editor.Unlock()
	current := s.Scheduler.Settings()
	data := config.ToData(current)
	entries := entriesOf(data, kind)
	oldKeyFile := ""
	if index >= 0 {
		old := entries[index].(map[string]any)
		if f["original_name"] != old["name"] {
			s.redirect(w, r, "/settings", "The settings changed in the meantime; nothing was saved.")
			return
		}
		oldKeyFile, _ = old["api_key_file"].(string)
	}
	entry := InstanceEntry(f)
	errs := Errors{}
	name, _ := entry["name"].(string)
	if name == "" {
		name = kind
	}
	if key != "" {
		if !config.IsAPIKeyFormat(key) {
			errs["api_key"] = append(errs["api_key"], "The API key has an unexpected format (20-64 letters and digits).")
		}
		if path, err := keystore.Path(current.StateDir, name); err != nil {
			errs["name"] = append(errs["name"], err.Error())
		} else {
			for _, src := range []string{"api_key_from", "api_key_file", "api_key_env", "api_key_source"} {
				delete(entry, src)
			}
			entry["api_key_file"] = path
			values["key_source"], values["key_ref"] = "api_key_file", path
		}
	}
	used := index
	if index < 0 {
		used = len(entries)
		entries = append(entries, entry)
	} else {
		entries[index] = entry
	}
	data[kind] = entries
	var general []string
	if len(errs) == 0 {
		if _, err := config.Parse(data, true); err != nil {
			var more Errors
			more, general = FieldErrors(err, fmt.Sprintf("%s[%d]", kind, used))
			for k, v := range more {
				errs[k] = append(errs[k], v...)
			}
		}
	}
	if len(errs) > 0 || len(general) > 0 {
		s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, errs, general)
		return
	}
	if key != "" {
		if _, err := keystore.Store(current.StateDir, name, key); err != nil {
			s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, nil, []string{"could not save the key: " + err.Error()})
			return
		}
	}
	if _, err := s.Editor.Save(data); err != nil {
		s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, nil, []string{err.Error()})
		return
	}
	if newFile, _ := entry["api_key_file"].(string); oldKeyFile != "" && oldKeyFile != newFile {
		keystore.Remove(current.StateDir, oldKeyFile)
	}
	s.redirect(w, r, "/settings", "Instance saved.")
}

func (s *Server) instanceDelete(w http.ResponseWriter, r *http.Request) {
	kind, index, ok := s.locate(w, r)
	if !ok {
		return
	}
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	s.Editor.Lock()
	defer s.Editor.Unlock()
	current := s.Scheduler.Settings()
	data := config.ToData(current)
	entries := entriesOf(data, kind)
	removed := entries[index].(map[string]any)
	if f["name"] != removed["name"] {
		s.redirect(w, r, "/settings", "The settings changed in the meantime; nothing was removed.")
		return
	}
	entries = slices.Delete(entries, index, index+1)
	if len(entries) == 0 {
		delete(data, kind)
	} else {
		data[kind] = entries
	}
	if _, err := s.Editor.Save(data); err != nil {
		s.redirect(w, r, "/settings", err.Error())
		return
	}
	keyFile, _ := removed["api_key_file"].(string)
	keystore.Remove(current.StateDir, keyFile)
	s.redirect(w, r, "/settings", fmt.Sprintf("Removed %s.", removed["name"]))
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	data, err := assets.ReadFile(name)
	if err != nil || !strings.HasPrefix(name, "static/") || strings.Contains(name, "..") {
		s.errorPage(w, r, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(data)
}

// Announce prints a one-time setup code at startup if no admin exists yet.
func Announce(s config.Settings) error {
	store := NewAuthStore(s.StateDir)
	if store.HasAdmin() {
		return nil
	}
	code, err := store.NewSetupCode()
	if err != nil {
		return err
	}
	bar := strings.Repeat("=", 64)
	slog.Warn(fmt.Sprintf("\n%s\nNo admin account yet. Open the web UI (port %d) and go to /setup.\nSetup code: %s\n"+
		"(single use; `arr-trickle reset-password` prints a new one)\n%s", bar, s.Web.Port, code, bar))
	return nil
}
