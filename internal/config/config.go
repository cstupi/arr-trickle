// Package config loads settings from a TOML file (preferred) or environment variables
// (single-instance shortcut), with strict validation.
//
// API keys never live in the settings. Each instance names where to read its key from:
// the app's own config.xml (api_key_from), a file such as a Docker secret (api_key_file),
// or an environment variable (api_key_env).
package config

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/cstupi/arr-trickle/internal/apps"
)

// DefaultConfigPath is where the image expects config.toml.
const DefaultConfigPath = "/config/config.toml"

var (
	apiKeyRE = regexp.MustCompile(`^[A-Za-z0-9]{20,64}$`)
	// XMLKeyRE finds the key in an *arr config.xml. A regex, never an XML parser: no entity expansion.
	XMLKeyRE = regexp.MustCompile(`<ApiKey>\s*([^<\s]+)\s*</ApiKey>`)

	// The settings each table accepts, in the order the writer emits them.
	topKeys      = []string{"interval_minutes", "state_dir", "dry_run", "log_level", "allow_insecure_http"}
	webKeys      = []string{"enabled", "listen", "port", "cookie_secure", "session_hours", "allowed_networks", "trusted_proxies"}
	instanceKeys = []string{"name", "url", "api_key_from", "api_key_file", "api_key_env", "missing", "cutoff", "daily_cap", "max_queue", "exclude", "timeout_seconds"}

	// LogLevels are the accepted log_level values.
	LogLevels = []string{"DEBUG", "INFO", "WARNING", "ERROR"}
)

// FieldError is one problem, tagged with the setting it is about (e.g. "sonarr[0].missing").
type FieldError struct{ Where, Msg string }

// Error is invalid or unusable settings. The message is safe to show: it never contains a key.
// Errors lists every problem found, so the web UI can show each next to its field.
type Error struct{ Errors []FieldError }

func (e *Error) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Msg
	}
	return strings.Join(msgs, "; ")
}

// Errorf builds a single-problem Error.
func Errorf(where, format string, args ...any) *Error {
	return &Error{[]FieldError{{where, fmt.Sprintf(format, args...)}}}
}

// collector gathers every problem in one parse. Its checks record a failure and return the zero value.
type collector struct{ errs []FieldError }

func (c *collector) add(where, format string, args ...any) {
	c.errs = append(c.errs, FieldError{where, fmt.Sprintf(format, args...)})
}

func (c *collector) take(err error) {
	if e, ok := errors.AsType[*Error](err); ok {
		c.errs = append(c.errs, e.Errors...)
	}
}

func (c *collector) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return &Error{c.errs}
}

func (c *collector) int(where string, v any, lo, hi int64) int {
	n, ok := v.(int64) // a bool is not an int here: `missing = true` is a mistake, not 1
	switch {
	case !ok:
		c.add(where, "%s must be an integer", where)
	case n < lo || n > hi:
		c.add(where, "%s must be between %d and %d", where, lo, hi)
	default:
		return int(n)
	}
	return 0
}

func (c *collector) bool(where string, v any) bool {
	b, ok := v.(bool)
	if !ok {
		c.add(where, "%s must be true or false", where)
	}
	return b
}

func (c *collector) str(where string, v any) string {
	s, _ := v.(string)
	if s = strings.TrimSpace(s); s == "" {
		c.add(where, "%s must be a non-empty string", where)
	}
	return s
}

func (c *collector) url(where string, v any, allowInsecureHTTP bool) string {
	s := strings.TrimRight(c.str(where, v), "/")
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		c.add(where, "%s must be an http:// or https:// URL", where)
	case u.User != nil || strings.ContainsAny(s, "?#"):
		c.add(where, "%s must not contain credentials, a query or a fragment", where)
	case u.Scheme == "http" && !allowInsecureHTTP && !isPrivateHost(u.Hostname()):
		c.add(where, "%s: refusing to send an API key over plain http to public host '%s'; "+
			"use https or set allow_insecure_http = true", where, u.Hostname())
	default:
		return s
	}
	return ""
}

func (c *collector) networks(where string, v any) []string {
	items, ok := v.([]any)
	var out []string
	for _, it := range items {
		s, isStr := it.(string)
		if !isStr {
			ok = false
			break
		}
		p, err := ParseNetwork(strings.TrimSpace(s))
		if err != nil {
			c.add(where, "%s: '%s' does not appear to be an IPv4 or IPv6 network", where, strings.TrimSpace(s))
			return nil
		}
		out = append(out, p.String())
	}
	if !ok {
		c.add(where, "%s must be a list of networks such as \"192.168.1.0/24\"", where)
		return nil
	}
	return out
}

// Instance is one configured *arr app.
type Instance struct {
	Kind, Name, URL                     string
	APIKeyFrom, APIKeyFile, APIKeyEnv   string
	Missing, Cutoff, DailyCap, MaxQueue int
	Exclude                             []int // sorted, unique
	TimeoutSeconds                      int
}

// NewInstance has the same defaults as a [[kind]] table that sets nothing else.
func NewInstance(kind, name, url string) Instance {
	return Instance{Kind: kind, Name: name, URL: url, Missing: 5, TimeoutSeconds: 60}
}

// Environ looks up environment variables; nil means the process environment.
type Environ map[string]string

func (e Environ) get(name string) string {
	if e == nil {
		return os.Getenv(name)
	}
	return e[name]
}

// APIKey reads the instance's key from its source.
func (i Instance) APIKey(env Environ) (string, error) {
	var key string
	var err error
	switch {
	case i.APIKeyFrom != "":
		key, err = keyFromArrConfig(i.APIKeyFrom)
	case i.APIKeyFile != "":
		key, err = readSecretFile(i.APIKeyFile)
	default:
		if key = strings.TrimSpace(env.get(i.APIKeyEnv)); key == "" {
			err = Errorf("", "%s: environment variable %s is empty", i.Name, i.APIKeyEnv)
		}
	}
	if err == nil && !IsAPIKeyFormat(key) {
		err = Errorf("", "%s: API key has an unexpected format", i.Name)
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

// IsAPIKeyFormat reports whether key looks like an *arr API key.
func IsAPIKeyFormat(key string) bool { return apiKeyRE.MatchString(key) }

// Web is the [web] table.
type Web struct {
	Enabled bool
	// All interfaces inside the container so other LAN machines can reach it; AllowedNetworks gates clients.
	Listen          string
	Port            int
	CookieSecure    bool
	SessionHours    int
	AllowedNetworks []string
	TrustedProxies  []string
}

// DefaultWeb is the [web] table when nothing is set: loopback, RFC 1918 and IPv6 unique-local clients
// may connect (the LAN, never the internet).
func DefaultWeb() Web {
	return Web{Enabled: true, Listen: "0.0.0.0", Port: 8790, SessionHours: 12, AllowedNetworks: []string{
		"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
	}}
}

// ListenAddr is the host:port the web UI listens on.
func (w Web) ListenAddr() string { return net.JoinHostPort(w.Listen, strconv.Itoa(w.Port)) }

// Settings is a whole validated configuration.
type Settings struct {
	Instances         []Instance
	IntervalMinutes   int
	StateDir          string
	DryRun            bool
	LogLevel          string
	AllowInsecureHTTP bool
	Web               Web
}

// unknown lists the keys of m that are not allowed, sorted and comma-separated.
func unknown(m map[string]any, allowed ...string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, k) {
			out = append(out, k)
		}
	}
	return strings.Join(out, ", ")
}

// isPrivateHost reports whether plain http to host doesn't cross an untrusted network.
func isPrivateHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" || !strings.Contains(host, ".") || // localhost or a Docker service name
		slices.ContainsFunc([]string{".local", ".internal", ".lan", ".home.arpa"}, func(s string) bool { return strings.HasSuffix(host, s) })
}

func anys[S ~[]E, E any](s S) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// tables accepts an array of tables, or a single table for one instance ([sonarr]).
func tables(v any) ([]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return []any{t}, true
	case []map[string]any:
		return anys(t), true
	case []any:
		return t, true
	}
	return nil, false
}

func idList(v any) ([]int, bool) {
	items, ok := v.([]any)
	out := make([]int, 0, len(items))
	for _, it := range items {
		n, isInt := it.(int64)
		if !isInt || n <= 0 || n > math.MaxInt32 {
			return []int{}, false
		}
		out = append(out, int(n))
	}
	slices.Sort(out)
	return slices.Compact(out), ok
}

func get(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func parseInstance(kind string, index int, raw any, allowInsecureHTTP bool) (Instance, error) {
	where := fmt.Sprintf("%s[%d]", kind, index)
	m, ok := raw.(map[string]any)
	if !ok {
		return Instance{}, Errorf(where, "%s must be a table ([[%s]])", where, kind)
	}
	var c collector
	if u := unknown(m, instanceKeys...); u != "" {
		c.add(where, "%s: unknown setting(s) %s", where, u)
	}
	at := func(k string) string { return where + "." + k }
	defaultName := kind
	if index > 0 {
		defaultName = fmt.Sprintf("%s-%d", kind, index+1)
	}
	inst := Instance{
		Kind:           kind,
		Name:           c.str(at("name"), get(m, "name", defaultName)),
		URL:            c.url(at("url"), m["url"], allowInsecureHTTP),
		Missing:        c.int(at("missing"), get(m, "missing", int64(5)), 0, 100),
		Cutoff:         c.int(at("cutoff"), get(m, "cutoff", int64(0)), 0, 100),
		DailyCap:       c.int(at("daily_cap"), get(m, "daily_cap", int64(0)), 0, 100_000),
		MaxQueue:       c.int(at("max_queue"), get(m, "max_queue", int64(0)), 0, 100_000),
		TimeoutSeconds: c.int(at("timeout_seconds"), get(m, "timeout_seconds", int64(60)), 5, 600),
	}
	sources := map[string]*string{"api_key_from": &inst.APIKeyFrom, "api_key_file": &inst.APIKeyFile, "api_key_env": &inst.APIKeyEnv}
	set := 0
	for k, dst := range sources {
		if v, ok := m[k]; ok {
			*dst = c.str(at(k), v)
			set++
		}
	}
	if set != 1 {
		c.add(at("api_key"), "%s: set exactly one of api_key_from, api_key_file, api_key_env", where)
	}
	if inst.Exclude, ok = idList(get(m, "exclude", []any{})); !ok {
		c.add(at("exclude"), "%s must be a list of positive integer ids", at("exclude"))
	}
	return inst, c.err()
}

func parseWeb(raw any) (Web, error) {
	w := DefaultWeb()
	m, ok := raw.(map[string]any)
	if !ok {
		return w, Errorf("web", "web must be a table ([web])")
	}
	var c collector
	if u := unknown(m, webKeys...); u != "" {
		c.add("web", "web: unknown setting(s) %s", u)
	}
	w.Enabled = c.bool("web.enabled", get(m, "enabled", true))
	if v, ok := m["listen"]; ok {
		s, _ := v.(string)
		if a, err := netip.ParseAddr(strings.TrimSpace(s)); err != nil {
			c.add("web.listen", "web.listen must be an IP address such as 0.0.0.0 or 127.0.0.1")
		} else {
			w.Listen = a.String()
		}
	}
	w.Port = c.int("web.port", get(m, "port", int64(w.Port)), 1, 65535)
	w.CookieSecure = c.bool("web.cookie_secure", get(m, "cookie_secure", false))
	w.SessionHours = c.int("web.session_hours", get(m, "session_hours", int64(w.SessionHours)), 1, 720)
	if v, ok := m["allowed_networks"]; ok {
		w.AllowedNetworks = c.networks("web.allowed_networks", v)
	}
	if v, ok := m["trusted_proxies"]; ok {
		w.TrustedProxies = c.networks("web.trusted_proxies", v)
	}
	return w, c.err()
}

// Parse validates settings. allowEmpty permits zero instances (the web UI starts that way, then adds them).
func Parse(data map[string]any, allowEmpty bool) (Settings, error) {
	var c collector
	if u := unknown(data, slices.Concat(topKeys, []string{"web"}, apps.Kinds)...); u != "" {
		c.add("", "unknown setting(s) %s", u)
	}
	s := Settings{
		Instances:         []Instance{},
		AllowInsecureHTTP: c.bool("allow_insecure_http", get(data, "allow_insecure_http", false)),
		IntervalMinutes:   c.int("interval_minutes", get(data, "interval_minutes", int64(60)), 5, 1440),
		StateDir:          c.str("state_dir", get(data, "state_dir", "/state")),
		DryRun:            c.bool("dry_run", get(data, "dry_run", false)),
		LogLevel:          strings.ToUpper(c.str("log_level", get(data, "log_level", "INFO"))),
	}
	if s.LogLevel == "" {
		s.LogLevel = "INFO"
	} else if !slices.Contains(LogLevels, s.LogLevel) {
		c.add("log_level", "log_level must be one of %s", strings.Join(LogLevels, ", "))
	}
	configured := 0
	for _, kind := range apps.Kinds {
		raw, present := data[kind]
		if !present {
			continue
		}
		entries, ok := tables(raw)
		if !ok {
			c.add(kind, "%s must be a table or an array of tables", kind)
		}
		configured += len(entries)
		for i, e := range entries {
			inst, err := parseInstance(kind, i, e, s.AllowInsecureHTTP)
			c.take(err)
			if err == nil {
				s.Instances = append(s.Instances, inst)
			}
		}
	}
	if configured == 0 && !allowEmpty && len(c.errs) == 0 {
		c.add("", "no apps configured; add at least one [[sonarr]], [[radarr]] or [[lidarr]]")
	}
	counts := map[string]int{}
	for _, i := range s.Instances {
		counts[i.Name]++
	}
	var dups []string
	for _, n := range slices.Sorted(maps.Keys(counts)) {
		if counts[n] > 1 {
			dups = append(dups, n)
		}
	}
	if len(dups) > 0 {
		c.add("name", "duplicate instance name(s): %s", strings.Join(dups, ", "))
	}
	var err error
	s.Web, err = parseWeb(get(data, "web", map[string]any{}))
	c.take(err)
	return s, c.err()
}

// ToData is the TOML structure for s (non-default values only), so Parse(ToData(s)) equals s.
func ToData(s Settings) map[string]any {
	data := map[string]any{"interval_minutes": int64(s.IntervalMinutes)}
	if s.StateDir != "/state" {
		data["state_dir"] = s.StateDir
	}
	if s.DryRun {
		data["dry_run"] = true
	}
	if s.LogLevel != "INFO" {
		data["log_level"] = s.LogLevel
	}
	if s.AllowInsecureHTTP {
		data["allow_insecure_http"] = true
	}
	if web := webData(s.Web); len(web) > 0 {
		data["web"] = web
	}
	for _, i := range s.Instances {
		list, _ := data[i.Kind].([]any)
		data[i.Kind] = append(list, instanceData(i))
	}
	return data
}

func webData(w Web) map[string]any {
	d, out := DefaultWeb(), map[string]any{}
	if !w.Enabled {
		out["enabled"] = false
	}
	if w.Listen != d.Listen {
		out["listen"] = w.Listen
	}
	if w.Port != d.Port {
		out["port"] = int64(w.Port)
	}
	if w.CookieSecure {
		out["cookie_secure"] = true
	}
	if w.SessionHours != d.SessionHours {
		out["session_hours"] = int64(w.SessionHours)
	}
	if !slices.Equal(w.AllowedNetworks, d.AllowedNetworks) {
		out["allowed_networks"] = anys(w.AllowedNetworks)
	}
	if len(w.TrustedProxies) > 0 {
		out["trusted_proxies"] = anys(w.TrustedProxies)
	}
	return out
}

// instanceData is one [[kind]] table, with only the non-default values (name and url always).
func instanceData(i Instance) map[string]any {
	m := map[string]any{"name": i.Name, "url": i.URL}
	for k, v := range map[string]string{"api_key_from": i.APIKeyFrom, "api_key_file": i.APIKeyFile, "api_key_env": i.APIKeyEnv} {
		if v != "" {
			m[k] = v
		}
	}
	d := NewInstance(i.Kind, "", "")
	for k, v := range map[string][2]int{
		"missing": {i.Missing, d.Missing}, "cutoff": {i.Cutoff, d.Cutoff}, "daily_cap": {i.DailyCap, d.DailyCap},
		"max_queue": {i.MaxQueue, d.MaxQueue}, "timeout_seconds": {i.TimeoutSeconds, d.TimeoutSeconds},
	} {
		if v[0] != v[1] {
			m[k] = int64(v[0])
		}
	}
	if len(i.Exclude) > 0 {
		ids := make([]any, len(i.Exclude))
		for n, id := range i.Exclude {
			ids[n] = int64(id)
		}
		m["exclude"] = ids
	}
	return m
}

// SplitIDs yields the tokens of a list like "1, 2 3" (shared by the env fallback and the web UI).
func SplitIDs(raw string) iter.Seq[string] {
	return strings.FieldsFuncSeq(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

// FromEnv builds the same structure as the TOML file from SONARR_URL-style variables (one instance per app).
func FromEnv(env Environ) (map[string]any, error) {
	num := func(name string, def int64) (int64, error) {
		raw := strings.TrimSpace(env.get(name))
		if raw == "" {
			return def, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, Errorf(name, "%s must be an integer", name)
		}
		return n, nil
	}
	data := map[string]any{}
	for _, kind := range apps.Kinds {
		p := strings.ToUpper(kind)
		if env.get(p+"_URL") == "" {
			continue
		}
		entry := map[string]any{"url": env.get(p + "_URL"), "api_key_from": "/arr/" + kind + "/config.xml"}
		for key, def := range map[string]int64{"MISSING": 5, "CUTOFF": 0, "DAILY_CAP": 0, "MAX_QUEUE": 0} {
			n, err := num(p+"_"+key, def)
			if err != nil {
				return nil, err
			}
			entry[strings.ToLower(key)] = n
		}
		ids := []any{}
		for t := range SplitIDs(env.get(p + "_EXCLUDE")) {
			n, err := strconv.ParseInt(t, 10, 64)
			if err != nil {
				return nil, Errorf(p+"_EXCLUDE", "%s_EXCLUDE must be a comma-separated list of ids", p)
			}
			ids = append(ids, n)
		}
		entry["exclude"] = ids
		if f := env.get(p + "_API_KEY_FILE"); f != "" {
			delete(entry, "api_key_from")
			entry["api_key_file"] = f
		} else if c := env.get(p + "_CONFIG"); c != "" {
			entry["api_key_from"] = c
		}
		data[kind] = []any{entry}
	}
	if env.get("INTERVAL_MINUTES") != "" {
		n, err := num("INTERVAL_MINUTES", 60)
		if err != nil {
			return nil, err
		}
		data["interval_minutes"] = n
	}
	if v := env.get("STATE_DIR"); v != "" {
		data["state_dir"] = v
	}
	if v := env.get("DRY_RUN"); v != "" {
		data["dry_run"] = slices.Contains([]string{"1", "true", "yes"}, strings.ToLower(strings.TrimSpace(v)))
	}
	if v := env.get("LOG_LEVEL"); v != "" {
		data["log_level"] = v
	}
	return data, nil
}

// Path is the settings file to use: the flag, else $ARR_TRICKLE_CONFIG, else the default.
func Path(flag string, env Environ) string {
	return cmp.Or(flag, env.get("ARR_TRICKLE_CONFIG"), DefaultConfigPath)
}

// Decode parses TOML text into the generic structure Parse validates.
func Decode(text string) (map[string]any, error) {
	data := map[string]any{}
	_, err := toml.Decode(text, &data)
	return data, err
}

// Load reads the TOML file if it exists (or was named explicitly), else falls back to the environment.
func Load(flag string, env Environ, allowEmpty bool) (Settings, error) {
	path := Path(flag, env)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Settings{}, Errorf("", "cannot read %s: %s (check the file's permissions)", path, strerror(err))
		}
		data, err := Decode(string(raw))
		if err != nil {
			return Settings{}, Errorf("", "%s: %s", path, err)
		}
		return Parse(data, allowEmpty)
	}
	if path != DefaultConfigPath && !allowEmpty {
		return Settings{}, Errorf("", "config file %s not found", path)
	}
	data, err := FromEnv(env)
	if err != nil {
		return Settings{}, err
	}
	return Parse(data, allowEmpty)
}

// strerror is the OS message for err, capitalized ("No such file or directory").
func strerror(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	s := err.Error()
	return strings.ToUpper(s[:1]) + s[1:]
}

func keyFromArrConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", Errorf("", "cannot read %s: %s", path, strerror(err))
	}
	m := XMLKeyRE.FindSubmatch(raw)
	if m == nil {
		return "", Errorf("", "no <ApiKey> found in %s", path)
	}
	return string(m[1]), nil
}

func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", Errorf("", "cannot read %s: %s", path, strerror(err))
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o004 != 0 {
		slog.Warn("key file is world-readable; consider chmod 600", "path", path)
	}
	return strings.TrimSpace(string(raw)), nil
}

// ParseNetwork accepts "192.168.1.7/24" (normalized to 192.168.1.0/24) or a bare address.
func ParseNetwork(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		return netip.PrefixFrom(a, a.BitLen()), err
	}
	p, err := netip.ParsePrefix(s)
	return p.Masked(), err
}
