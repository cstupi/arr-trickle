// Package web is the web UI: server-rendered pages, no JavaScript.
//
// Every request passes the guard first, which enforces, in order: the client network allowlist, login
// (deny by default: only publicPaths and /static/ are reachable without a session), and for every
// non-GET request a same-origin check plus the session's CSRF token. Pages never receive an API key:
// the templates only get settings whose key sources are paths or variable names.
package web

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"maps"
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

const (
	cookieName   = "arr_trickle_session"
	maxBody      = 64 * 1024
	maxFields    = 100
	pageSize     = 50
	staticPrefix = "/static/" // reachable without a session (the stylesheet)
)

// publicPaths are the only routes reachable without a session.
var publicPaths = []string{"/login", "/setup", "/healthz"}

var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; script-src 'none'; object-src 'none'; base-uri 'none'; " +
		"frame-ancestors 'none'; form-action 'self'",
	"X-Content-Type-Options": "nosniff",
	"X-Frame-Options":        "DENY",
	// same-origin, not no-referrer: with no-referrer, browsers send `Origin: null` on form posts,
	// which would defeat the same-origin check. Nothing is ever sent to another site.
	"Referrer-Policy":              "same-origin",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Cache-Control":                "no-store", // removed again for static files
}

type route struct {
	pattern string // "METHOD /path"
	handle  func(*Server, http.ResponseWriter, *http.Request)
}

// routes is the whole route table. The guard protects every route not in publicPaths, so a new route
// is protected automatically, and a test walks this table to prove it.
var routes = []route{
	{"GET /healthz", func(_ *Server, w http.ResponseWriter, _ *http.Request) { plain(w, http.StatusOK, "ok") }},
	{"GET /login", (*Server).loginPage},
	{"POST /login", (*Server).loginSubmit},
	{"GET /setup", (*Server).setupPage},
	{"POST /setup", (*Server).setupSubmit},
	{"POST /logout", (*Server).logout},
	{"GET /{$}", (*Server).dashboard},
	{"POST /control/run-now", (*Server).runNow},
	{"POST /control/pause", (*Server).pause},
	{"POST /control/resume", (*Server).resume},
	{"POST /control/dry-run", (*Server).toggleDryRun},
	{"POST /control/check", (*Server).check},
	{"GET /activity", (*Server).activity},
	{"GET /settings", (*Server).settingsPage},
	{"POST /settings", (*Server).settingsSave},
	{"GET /settings/instances/new", (*Server).instanceNew},
	{"POST /settings/instances/new", (*Server).instanceCreate},
	{"GET /settings/instances/{kind}/{index}", (*Server).instanceEdit},
	{"POST /settings/instances/{kind}/{index}", (*Server).instanceUpdate},
	{"POST /settings/instances/{kind}/{index}/delete", (*Server).instanceDelete},
	{"GET " + staticPrefix, (*Server).static},
}

// Server is the web UI.
type Server struct {
	scheduler    *service.Scheduler
	editor       *editor
	store        *AuthStore
	sessions     *sessions
	limiter      *rateLimiter
	allowed      []netip.Prefix
	trusted      []netip.Prefix
	cookieSecure bool
	mux          *http.ServeMux
	inflight     chan struct{}
	pages        map[string]*template.Template
}

// New builds the UI for the given settings; configPath is where Settings saves config.toml.
func New(s config.Settings, sched *service.Scheduler, configPath string) *Server {
	srv := &Server{
		scheduler:    sched,
		editor:       &editor{path: configPath, scheduler: sched},
		store:        NewAuthStore(s.StateDir),
		sessions:     newSessions(time.Duration(s.Web.SessionHours)*time.Hour, time.Hour),
		limiter:      newRateLimiter(),
		allowed:      networks(s.Web.AllowedNetworks),
		trusted:      networks(s.Web.TrustedProxies),
		cookieSecure: s.Web.CookieSecure,
		mux:          http.NewServeMux(),
		inflight:     make(chan struct{}, 64),
		pages:        parseTemplates(),
	}
	for _, rt := range routes {
		srv.mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { rt.handle(srv, w, r) })
	}
	srv.mux.HandleFunc("/", srv.notFound)
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
	ip, host string
	https    bool
	session  *session
}

type ctxKey struct{}

func stateOf(r *http.Request) *reqState { return r.Context().Value(ctxKey{}).(*reqState) }

func isPublic(path string) bool {
	return slices.Contains(publicPaths, path) || strings.HasPrefix(path, staticPrefix)
}

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

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func plain(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, text)
}

// ServeHTTP is the guard: allowlist, login, origin and CSRF checks, security headers, access log.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w := &statusWriter{ResponseWriter: rw}
	start := time.Now()
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, ok := clientIP(peer, r.Header.Get("X-Forwarded-For"), s.trusted)
	defer func() {
		if p := recover(); p != nil { // generic page only: no panic text reaches the browser
			slog.Error("web: internal error", "method", r.Method, "path", r.URL.Path, "panic", p)
			if w.status == 0 {
				plain(w, http.StatusInternalServerError, "Internal error. The details are in the server log.")
			}
		}
		slog.Info("web", "client", ip, "method", r.Method, "path", r.URL.Path, "status", w.status, "duration", time.Since(start))
	}()
	for k, v := range securityHeaders {
		w.Header().Set(k, v)
	}
	if strings.HasPrefix(r.URL.Path, staticPrefix) {
		w.Header().Del("Cache-Control")
	}
	if !ok || !inAny(ip, s.allowed) {
		slog.Warn("web: refused a client not in web.allowed_networks", "client", pick(ok, ip.String(), peer))
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

	st := &reqState{ip: ip.String(), host: r.Host, https: r.TLS != nil}
	if peerIP, _ := parseIP(peer); inAny(peerIP, s.trusted) {
		st.https = st.https || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
		st.host = cmp.Or(r.Header.Get("X-Forwarded-Host"), st.host)
	}
	if c, err := r.Cookie(cookieName); err == nil {
		st.session = s.sessions.get(c.Value)
	}
	if st.session != nil && st.session.authenticated() {
		if cred, ok := s.store.credentialID(); !ok || cred != st.session.credentialID {
			s.sessions.delete(st.session.id) // the password was reset: every old session ends
			st.session = nil
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, st))

	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch {
	case !isPublic(r.URL.Path) && (st.session == nil || !st.session.authenticated()):
		if safe {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			plain(w, http.StatusUnauthorized, "unauthorized")
		}
	case !safe && !s.checkPost(w, r, st):
	default:
		s.mux.ServeHTTP(w, r)
	}
}

// pick is a conditional expression.
func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// checkPost enforces same-origin, the content type, the size limit and the CSRF token, then makes the
// (already read) body available to the handler again. It answers the request itself when refusing.
func (s *Server) checkPost(w http.ResponseWriter, r *http.Request, st *reqState) bool {
	ctype, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	values, _ := url.ParseQuery(string(body))
	switch {
	case !sameOrigin(r, st):
		slog.Warn("web: refused a cross-origin request", "method", r.Method, "path", r.URL.Path, "client", st.ip)
		plain(w, http.StatusForbidden, "cross-origin request refused")
	case !strings.EqualFold(strings.TrimSpace(ctype), "application/x-www-form-urlencoded"):
		plain(w, http.StatusUnsupportedMediaType, "unsupported content type")
	case err != nil || len(body) > maxBody:
		plain(w, http.StatusRequestEntityTooLarge, "request too large")
	case st.session == nil || !hmac.Equal([]byte(values.Get("csrf")), []byte(st.session.csrf)):
		plain(w, http.StatusForbidden, "the form expired or is invalid; go back, reload the page and try again")
	default:
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		return true
	}
	return false
}

func sameOrigin(r *http.Request, st *reqState) bool {
	expected := strings.ToLower(pick(st.https, "https", "http") + "://" + st.host)
	if origin := r.Header.Get("Origin"); origin != "" {
		return strings.ToLower(origin) == expected
	}
	u, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && u.Host != "" && strings.ToLower(u.Scheme+"://"+u.Host) == expected
}

// --- helpers ---------------------------------------------------------------------

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func parseTemplates() map[string]*template.Template {
	funcs := template.FuncMap{
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"num": func(p *int) string {
			if p == nil {
				return "–"
			}
			return strconv.Itoa(*p)
		},
		"join":       strings.Join,
		"hasPrefix":  strings.HasPrefix,
		"capitalize": capitalize,
		"port":       func(kind string) int { return map[string]int{"sonarr": 8989, "radarr": 7878, "lidarr": 8686}[kind] },
		"owner": func(kind string) string {
			return map[string]string{"sonarr": "series", "radarr": "movie", "lidarr": "artist"}[kind]
		},
		"activityTime": func(s string) string { return strings.TrimSuffix(strings.Replace(s, "T", " ", 1), "+00:00") },
	}
	pages := map[string]*template.Template{}
	names, _ := fs.Glob(assets, "templates/*.html")
	for _, n := range names {
		if name := strings.TrimPrefix(n, "templates/"); name != "base.html" {
			pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(assets, "templates/base.html", n))
		}
	}
	return pages
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, data map[string]any) {
	st := stateOf(r)
	base := map[string]any{"CSRF": "", "Authenticated": false, "Flash": []string(nil), "Version": version.Version, "Path": r.URL.Path}
	if st.session != nil {
		base["CSRF"], base["Authenticated"], base["Flash"] = st.session.csrf, st.session.authenticated(), s.sessions.takeFlash(st.session)
	}
	maps.Copy(base, data)
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "base.html", base); err != nil {
		panic(fmt.Sprintf("template %s: %v", name, err))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "error.html", http.StatusNotFound, map[string]any{"Title": "Not found"})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, sess *session) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sess.id, Path: "/", MaxAge: int(s.sessions.absolute.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.cookieSecure || stateOf(r).https,
	})
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, flash string) {
	if sess := stateOf(r).session; sess != nil && flash != "" {
		s.sessions.addFlash(sess, flash)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// anonSession gives the request a not-logged-in session if it has none (it carries the CSRF token for
// the login/setup forms) and sets its cookie.
func (s *Server) anonSession(w http.ResponseWriter, r *http.Request) {
	if st := stateOf(r); st.session == nil {
		st.session = s.sessions.create("")
		s.setCookie(w, r, st.session)
	}
}

// form parses the posted form (or answers 400).
func (s *Server) form(w http.ResponseWriter, r *http.Request) (postForm, bool) {
	if err := r.ParseForm(); err != nil || len(r.PostForm) > maxFields {
		plain(w, http.StatusBadRequest, "bad form")
		return nil, false
	}
	f := postForm{}
	for k, v := range r.PostForm {
		f[k] = v[0]
	}
	return f, true
}

// --- public pages ----------------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess := stateOf(r).session; sess != nil && sess.authenticated() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	} else if !s.store.HasAdmin() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	} else {
		s.anonSession(w, r)
		s.render(w, r, "login.html", http.StatusOK, nil)
	}
}

// throttled answers 429 if this client must wait before another login/setup attempt.
func (s *Server) throttled(w http.ResponseWriter, r *http.Request, page string) bool {
	wait := s.limiter.retryAfter(stateOf(r).ip)
	if wait <= 0 {
		return false
	}
	secs := int((wait + time.Second - 1) / time.Second) // rounded up
	slog.Warn("web: attempt refused: too many failures", "page", page, "client", stateOf(r).ip)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.render(w, r, page, http.StatusTooManyRequests, map[string]any{"Error": fmt.Sprintf("Too many failed attempts. Try again in %d s.", secs)})
	return true
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	st := stateOf(r)
	f, ok := s.form(w, r)
	if !ok || s.throttled(w, r, "login.html") {
		return
	}
	select { // at most a couple of ~32 MiB scrypt hashes at once
	case hashSlots <- struct{}{}:
		defer func() { <-hashSlots }()
	default:
		plain(w, http.StatusServiceUnavailable, "busy, try again")
		return
	}
	cred, ok := s.store.verifyLogin(f["username"], f["password"])
	if !ok {
		s.limiter.failure(st.ip)
		slog.Warn("web: failed login", "client", st.ip) // never the password (nor the username, which may be one)
		s.render(w, r, "login.html", http.StatusUnauthorized, map[string]any{"Error": "Wrong username or password."})
		return
	}
	s.limiter.success(st.ip)
	if st.session != nil {
		s.sessions.delete(st.session.id) // a fresh id on login: a planted pre-login id is worthless
	}
	st.session = s.sessions.create(cred)
	slog.Info("web: login", "client", st.ip)
	s.setCookie(w, r, st.session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.store.HasAdmin() {
		s.notFound(w, r)
		return
	}
	s.anonSession(w, r)
	s.render(w, r, "setup.html", http.StatusOK, map[string]any{"Username": ""})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	st := stateOf(r)
	if s.store.HasAdmin() {
		s.notFound(w, r)
		return
	}
	f, ok := s.form(w, r)
	if !ok || s.throttled(w, r, "setup.html") {
		return
	}
	page := map[string]any{"Username": f["username"]}
	if !s.store.CheckSetupCode(f["code"]) {
		s.limiter.failure(st.ip)
		slog.Warn("web: wrong setup code", "client", st.ip)
		page["Error"] = "The setup code is not valid. Copy it from the log."
		s.render(w, r, "setup.html", http.StatusBadRequest, page)
		return
	}
	if err := s.store.CreateAdmin(f["code"], f["username"], f["password"], f["confirm"]); err != nil {
		page["Error"] = capitalize(err.Error()) + "."
		s.render(w, r, "setup.html", http.StatusBadRequest, page)
		return
	}
	slog.Info("web: admin account created", "client", st.ip)
	s.redirect(w, r, "/login", "Admin account created. Log in.")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.sessions.delete(stateOf(r).session.id)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard and controls ------------------------------------------------------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "dashboard.html", http.StatusOK, map[string]any{"Snap": s.scheduler.Snapshot()})
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	s.scheduler.RunNow()
	s.redirect(w, r, "/", "A pass will start in a moment.")
}

func (s *Server) control(w http.ResponseWriter, r *http.Request, do func() error, verb, done string) {
	if err := do(); err != nil {
		done = "Could not " + verb + ": " + err.Error()
	}
	s.redirect(w, r, "/", done)
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, s.scheduler.Pause, "pause", "Scheduled passes paused. Run now still works.")
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, s.scheduler.Resume, "resume", "Scheduled passes resumed.")
}

func (s *Server) toggleDryRun(w http.ResponseWriter, r *http.Request) {
	s.editor.Lock()
	defer s.editor.Unlock()
	data := config.ToData(s.scheduler.Settings())
	data["dry_run"] = !s.scheduler.Settings().DryRun
	n, err := s.editor.save(data)
	msg := fmt.Sprint(err)
	if err == nil {
		msg = "Dry run " + pick(n.DryRun, "on", "off") + "."
	}
	s.redirect(w, r, "/", msg)
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.form(w, r); ok {
		s.render(w, r, "check.html", http.StatusOK, map[string]any{"Result": s.scheduler.Check(f["name"])})
	}
}

// --- activity --------------------------------------------------------------------

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	instance := r.URL.Query().Get("instance")
	entries := s.scheduler.Activity.Entries(instance)
	pages := max(1, (len(entries)+pageSize-1)/pageSize)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = min(max(page, 1), pages)
	link := func(p int) string {
		if p < 1 || p > pages {
			return ""
		}
		q := url.Values{"page": {strconv.Itoa(p)}}
		if instance != "" {
			q.Set("instance", instance)
		}
		return "/activity?" + q.Encode()
	}
	var names []string
	for _, i := range s.scheduler.Settings().Instances {
		names = append(names, i.Name)
	}
	s.render(w, r, "activity.html", http.StatusOK, map[string]any{
		"Entries": entries[(page-1)*pageSize : min(len(entries), page*pageSize)],
		"Page":    page, "Pages": pages, "Instance": instance, "Names": names,
		"Newer": link(page - 1), "Older": link(page + 1),
	})
}

// --- settings --------------------------------------------------------------------

type instanceRow struct {
	config.Instance
	Index   int
	Managed bool
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	st := s.scheduler.Settings()
	s.renderSettings(w, r, http.StatusOK, map[string]any{
		"interval_minutes": strconv.Itoa(st.IntervalMinutes), "log_level": st.LogLevel,
		"dry_run": st.DryRun, "allow_insecure_http": st.AllowInsecureHTTP,
	}, nil, nil)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, values map[string]any, errs fieldErrors, general []string) {
	settings := s.scheduler.Settings()
	var rows []instanceRow
	for _, kind := range apps.Kinds {
		for n, i := range s.instancesOf(kind) {
			rows = append(rows, instanceRow{i, n, keystore.IsManaged(settings.StateDir, i.APIKeyFile)})
		}
	}
	s.render(w, r, "settings.html", status, map[string]any{
		"Settings": settings, "Rows": rows, "Path": s.editor.path, "Problem": s.editor.problem(),
		"Kinds": apps.Kinds, "Values": values, "Errors": errs, "General": general, "LogLevels": config.LogLevels,
	})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	f, ok := s.form(w, r)
	if !ok {
		return
	}
	s.editor.Lock()
	defer s.editor.Unlock()
	data := config.ToData(s.scheduler.Settings())
	applyGlobal(data, f)
	if _, err := s.editor.save(data); err != nil {
		errs, general := splitErrors(err, "")
		s.renderSettings(w, r, http.StatusBadRequest, map[string]any{
			"interval_minutes": f["interval_minutes"], "log_level": f["log_level"],
			"dry_run": f["dry_run"] == "on", "allow_insecure_http": f["allow_insecure_http"] == "on",
		}, errs, general)
		return
	}
	s.redirect(w, r, "/settings", "Settings saved.")
}

func (s *Server) renderInstance(w http.ResponseWriter, r *http.Request, kind string, index int, values postForm, status int, errs fieldErrors, general ...string) {
	action := "/settings/instances/new"
	if index >= 0 {
		action = fmt.Sprintf("/settings/instances/%s/%d", kind, index)
		values["original_name"] = cmp.Or(values["original_name"], values["name"])
	}
	s.render(w, r, "instance.html", status, map[string]any{
		"Kind": kind, "Edit": index >= 0, "Action": action, "Values": values, "Sources": keySources,
		"Managed": values["key_source"] == "api_key_file" && keystore.IsManaged(s.scheduler.Settings().StateDir, values["key_ref"]),
		"Errors":  errs, "General": general,
	})
}

func (s *Server) instancesOf(kind string) []config.Instance {
	return slices.DeleteFunc(slices.Clone(s.scheduler.Settings().Instances), func(i config.Instance) bool { return i.Kind != kind })
}

// locate resolves {kind}/{index}, or answers 404.
func (s *Server) locate(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	kind := r.PathValue("kind")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 || index >= len(s.instancesOf(kind)) {
		s.notFound(w, r)
		return "", 0, false
	}
	return kind, index, true
}

func (s *Server) instanceNew(w http.ResponseWriter, r *http.Request) {
	if kind := r.URL.Query().Get("kind"); apps.IsKnown(kind) {
		s.renderInstance(w, r, kind, -1, instanceValues(nil), http.StatusOK, nil)
	} else {
		s.notFound(w, r)
	}
}

func (s *Server) instanceEdit(w http.ResponseWriter, r *http.Request) {
	if kind, index, ok := s.locate(w, r); ok {
		s.renderInstance(w, r, kind, index, instanceValues(&s.instancesOf(kind)[index]), http.StatusOK, nil)
	}
}

func (s *Server) instanceCreate(w http.ResponseWriter, r *http.Request) {
	if f, ok := s.form(w, r); !ok {
	} else if apps.IsKnown(f["kind"]) {
		s.saveInstance(w, r, f, f["kind"], -1)
	} else {
		s.notFound(w, r)
	}
}

func (s *Server) instanceUpdate(w http.ResponseWriter, r *http.Request) {
	if kind, index, ok := s.locate(w, r); ok {
		if f, ok := s.form(w, r); ok {
			s.saveInstance(w, r, f, kind, index)
		}
	}
}

// keyUsers counts the instances reading their key from path.
func keyUsers(st config.Settings, path string) int {
	n := 0
	for _, i := range st.Instances {
		if i.APIKeyFile == path {
			n++
		}
	}
	return n
}

func (s *Server) saveInstance(w http.ResponseWriter, r *http.Request, f postForm, kind string, index int) {
	key := f["api_key"] // write-only: validated, stored in the state dir, never rendered or logged
	values := maps.Clone(f)
	delete(values, "api_key")
	delete(values, "csrf")
	s.editor.Lock()
	defer s.editor.Unlock()
	current := s.scheduler.Settings()
	data := config.ToData(current)
	entries, _ := data[kind].([]any)
	oldKeyFile := ""
	if index >= 0 {
		old := entries[index].(map[string]any)
		if f["original_name"] != old["name"] {
			s.redirect(w, r, "/settings", "The settings changed in the meantime; nothing was saved.")
			return
		}
		oldKeyFile, _ = old["api_key_file"].(string)
	}
	entry := instanceEntry(f)
	name := cmp.Or(strings.TrimSpace(f["name"]), kind)
	errs := fieldErrors{}
	keyFile := ""
	if key != "" {
		if !config.IsAPIKeyFormat(key) {
			errs["api_key"] = []string{"The API key has an unexpected format (20-64 letters and digits)."}
		}
		var err error
		if keyFile, err = keystore.Path(current.StateDir, name); err != nil {
			errs["name"] = []string{err.Error()}
		}
		for _, k := range []string{"api_key_from", "api_key_env", "api_key_source"} {
			delete(entry, k)
		}
		entry["api_key_file"], values["key_source"], values["key_ref"] = keyFile, "api_key_file", keyFile
	}
	used := index
	if index < 0 {
		used, entries = len(entries), append(entries, entry)
	} else {
		entries[index] = entry
	}
	data[kind] = entries
	var general []string
	if len(errs) == 0 {
		if parsed, err := config.Parse(data, true); err != nil {
			errs, general = splitErrors(err, fmt.Sprintf("%s[%d]", kind, used))
		} else if keyFile != "" && keyUsers(parsed, keyFile) > 1 {
			errs["name"] = []string{"Another instance already reads the key stored under this name; choose another name."}
		}
	}
	if len(errs) > 0 || len(general) > 0 {
		s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, errs, general...)
		return
	}
	if key != "" {
		if _, err := keystore.Store(current.StateDir, name, key); err != nil {
			s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, nil, "could not save the key: "+err.Error())
			return
		}
	}
	saved, err := s.editor.save(data)
	if err != nil {
		s.renderInstance(w, r, kind, index, values, http.StatusBadRequest, nil, err.Error())
		return
	}
	if keyUsers(saved, oldKeyFile) == 0 {
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
	s.editor.Lock()
	defer s.editor.Unlock()
	data := config.ToData(s.scheduler.Settings())
	entries := data[kind].([]any)
	removed := entries[index].(map[string]any)
	if f["name"] != removed["name"] {
		s.redirect(w, r, "/settings", "The settings changed in the meantime; nothing was removed.")
		return
	}
	data[kind] = slices.Delete(entries, index, index+1)
	saved, err := s.editor.save(data)
	if err != nil {
		s.redirect(w, r, "/settings", err.Error())
		return
	}
	if keyFile, _ := removed["api_key_file"].(string); keyUsers(saved, keyFile) == 0 {
		keystore.Remove(saved.StateDir, keyFile)
	}
	s.redirect(w, r, "/settings", fmt.Sprintf("Removed %s.", removed["name"]))
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if name := strings.TrimPrefix(r.URL.Path, "/"); strings.HasSuffix(name, ".css") {
		http.ServeFileFS(w, r, assets, name)
	} else {
		s.notFound(w, r)
	}
}

// Announce prints a one-time setup code to w at startup if no admin exists yet.
func Announce(s config.Settings, w io.Writer) error {
	store := NewAuthStore(s.StateDir)
	if store.HasAdmin() {
		return nil
	}
	code, err := store.NewSetupCode()
	if err != nil {
		return err
	}
	bar := strings.Repeat("=", 64)
	_, err = fmt.Fprintf(w, "%s\nNo admin account yet. Open the web UI (port %d) and go to /setup.\nSetup code: %s\n"+
		"(single use; `arr-trickle reset-password` prints a new one)\n%s\n", bar, s.Web.Port, code, bar)
	return err
}
