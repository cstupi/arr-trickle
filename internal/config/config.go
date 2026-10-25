// Package config loads settings from a TOML file (preferred) or environment variables
// (single-instance shortcut), with strict validation.
//
// API keys never live in the settings. Each instance names where to read its key from:
// the app's own config.xml (api_key_from), a file such as a Docker secret (api_key_file),
// or an environment variable (api_key_env).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/cstupi/arr-trickle/internal/apps"
)

// DefaultConfigPath is where the image expects config.toml.
const DefaultConfigPath = "/config/config.toml"

var (
	apiKeyRE = regexp.MustCompile(`^[A-Za-z0-9]{20,64}$`)
	// XMLKeyRE finds the key in an *arr config.xml. A regex, never an XML parser: no entity expansion.
	XMLKeyRE = regexp.MustCompile(`<ApiKey>\s*([^<\s]+)\s*</ApiKey>`)
)

// FieldError is one problem, tagged with the setting it is about (e.g. "sonarr[0].missing").
type FieldError struct {
	Where string
	Msg   string
}

// Error is invalid or unusable settings. The message is safe to show: it never contains a key.
// Errors lists every problem found, so the web UI can show each next to its field.
type Error struct {
	Errors []FieldError
}

func (e *Error) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Msg
	}
	return strings.Join(msgs, "; ")
}

// Errorf builds a single-problem Error.
func Errorf(where, format string, args ...any) *Error {
	return &Error{Errors: []FieldError{{where, fmt.Sprintf(format, args...)}}}
}

type collector struct{ errs []FieldError }

func (c *collector) add(where, format string, args ...any) {
	c.errs = append(c.errs, FieldError{where, fmt.Sprintf(format, args...)})
}

func (c *collector) take(err error) {
	var e *Error
	if errors.As(err, &e) {
		c.errs = append(c.errs, e.Errors...)
	} else if err != nil {
		c.errs = append(c.errs, FieldError{"", err.Error()})
	}
}

func (c *collector) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return &Error{Errors: c.errs}
}

// Instance is one configured *arr app.
type Instance struct {
	Kind           string
	Name           string
	URL            string
	APIKeyFrom     string
	APIKeyFile     string
	APIKeyEnv      string
	Missing        int
	Cutoff         int
	DailyCap       int
	MaxQueue       int
	Exclude        []int // sorted, unique
	TimeoutSeconds int
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
		key = strings.TrimSpace(env.get(i.APIKeyEnv))
		if key == "" {
			err = Errorf("", "%s: environment variable %s is empty", i.Name, i.APIKeyEnv)
		}
	}
	if err != nil {
		return "", err
	}
	if !IsAPIKeyFormat(key) {
		return "", Errorf("", "%s: API key has an unexpected format", i.Name)
	}
	return key, nil
}

// IsAPIKeyFormat reports whether key looks like an *arr API key.
func IsAPIKeyFormat(key string) bool { return apiKeyRE.MatchString(key) }

// DefaultAllowedNetworks is loopback, RFC 1918 and IPv6 unique-local: the LAN, never the internet.
var DefaultAllowedNetworks = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

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

// DefaultWeb is the [web] table when nothing is set.
func DefaultWeb() Web {
	return Web{
		Enabled: true, Listen: "0.0.0.0", Port: 8790, SessionHours: 12,
		AllowedNetworks: slices.Clone(DefaultAllowedNetworks), TrustedProxies: []string{},
	}
}

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

var (
	topKeys      = keySet("interval_minutes", "state_dir", "dry_run", "log_level", "allow_insecure_http", "web", "sonarr", "radarr", "lidarr")
	instanceKeys = keySet("name", "url", "api_key_from", "api_key_file", "api_key_env", "missing", "cutoff", "daily_cap", "max_queue", "exclude", "timeout_seconds")
	webKeys      = keySet("enabled", "listen", "port", "cookie_secure", "session_hours", "allowed_networks", "trusted_proxies")
	logLevels    = []string{"DEBUG", "ERROR", "INFO", "WARNING"}
	keySources   = []string{"api_key_from", "api_key_file", "api_key_env"}
)

func keySet(keys ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func unknown(data map[string]any, allowed map[string]bool) []string {
	var out []string
	for k := range data {
		if !allowed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) { // bool is not an int here: `missing = true` is a mistake, not 1
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func toInt(where string, v any, lo, hi int64) (int, error) {
	n, ok := asInt(v)
	if !ok {
		return 0, Errorf(where, "%s must be an integer", where)
	}
	if n < lo || n > hi {
		return 0, Errorf(where, "%s must be between %d and %d", where, lo, hi)
	}
	return int(n), nil
}

func toBool(where string, v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, Errorf(where, "%s must be true or false", where)
	}
	return b, nil
}

func toStr(where string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", Errorf(where, "%s must be a non-empty string", where)
	}
	return strings.TrimSpace(s), nil
}

// IsPrivateHost reports whether plain http to host doesn't cross an untrusted network.
func IsPrivateHost(host string) bool {
	if host == "localhost" || !strings.Contains(host, ".") { // localhost or a Docker service name
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		for _, suffix := range []string{".local", ".internal", ".lan", ".home.arpa"} {
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func toURL(where string, v any, allowInsecureHTTP bool) (string, error) {
	s, err := toStr(where, v)
	if err != nil {
		return "", err
	}
	s = strings.TrimRight(s, "/")
	u, perr := url.Parse(s)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", Errorf(where, "%s must be an http:// or https:// URL", where)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, "?#") {
		return "", Errorf(where, "%s must not contain credentials, a query or a fragment", where)
	}
	host := u.Hostname()
	if u.Scheme == "http" && !allowInsecureHTTP && !IsPrivateHost(host) {
		return "", Errorf(where, "%s: refusing to send an API key over plain http to public host '%s'; "+
			"use https or set allow_insecure_http = true", where, host)
	}
	return s, nil
}

// tables accepts an array of tables, or a single table for one instance ([sonarr]).
func tables(v any) ([]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return []any{t}, true
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	case []any:
		return t, true
	}
	return nil, false
}

func idList(v any) ([]int, bool) {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case []int64:
		for _, n := range t {
			items = append(items, n)
		}
	case []int:
		for _, n := range t {
			items = append(items, n)
		}
	default:
		return nil, false
	}
	seen := map[int]bool{}
	for _, it := range items {
		n, ok := asInt(it)
		if !ok || n <= 0 || n > 1<<31 {
			return nil, false
		}
		seen[int(n)] = true
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, true
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
	if u := unknown(m, instanceKeys); len(u) > 0 {
		c.add(where, "%s: unknown setting(s) %s", where, strings.Join(u, ", "))
	}
	var sources []string
	for _, k := range keySources {
		if _, ok := m[k]; ok {
			sources = append(sources, k)
		}
	}
	if len(sources) != 1 {
		c.add(where+".api_key", "%s: set exactly one of api_key_from, api_key_file, api_key_env", where)
	}
	exclude, ok := idList(get(m, "exclude", []any{}))
	if !ok {
		c.add(where+".exclude", "%s.exclude must be a list of positive integer ids", where)
		exclude = []int{}
	}
	defaultName := kind
	if index > 0 {
		defaultName = fmt.Sprintf("%s-%d", kind, index+1)
	}
	inst := Instance{Kind: kind, Exclude: exclude}
	var err error
	inst.Name, err = toStr(where+".name", get(m, "name", defaultName))
	c.take(err)
	inst.URL, err = toURL(where+".url", m["url"], allowInsecureHTTP)
	c.take(err)
	for _, k := range sources {
		v, err := toStr(where+"."+k, m[k])
		c.take(err)
		switch k {
		case "api_key_from":
			inst.APIKeyFrom = v
		case "api_key_file":
			inst.APIKeyFile = v
		default:
			inst.APIKeyEnv = v
		}
	}
	ints := []struct {
		key     string
		def, hi int64
		dst     *int
	}{
		{"missing", 5, 100, &inst.Missing},
		{"cutoff", 0, 100, &inst.Cutoff},
		{"daily_cap", 0, 100_000, &inst.DailyCap},
		{"max_queue", 0, 100_000, &inst.MaxQueue},
	}
	for _, f := range ints {
		*f.dst, err = toInt(where+"."+f.key, get(m, f.key, f.def), 0, f.hi)
		c.take(err)
	}
	inst.TimeoutSeconds, err = toInt(where+".timeout_seconds", get(m, "timeout_seconds", int64(60)), 5, 600)
	c.take(err)
	return inst, c.err()
}

func networks(where string, v any) ([]string, error) {
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case []string:
		for _, s := range t {
			items = append(items, s)
		}
	default:
		return nil, Errorf(where, "%s must be a list of networks such as \"192.168.1.0/24\"", where)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, Errorf(where, "%s must be a list of networks such as \"192.168.1.0/24\"", where)
		}
		p, err := ParseNetwork(strings.TrimSpace(s))
		if err != nil {
			return nil, Errorf(where, "%s: '%s' does not appear to be an IPv4 or IPv6 network", where, strings.TrimSpace(s))
		}
		out = append(out, p.String())
	}
	return out, nil
}

// ParseNetwork accepts "192.168.1.7/24" (normalized to 192.168.1.0/24) or a bare address.
func ParseNetwork(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return p.Masked(), nil
}

func parseWeb(raw any) (Web, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return DefaultWeb(), Errorf("web", "web must be a table ([web])")
	}
	var c collector
	if u := unknown(m, webKeys); len(u) > 0 {
		c.add("web", "web: unknown setting(s) %s", strings.Join(u, ", "))
	}
	w := DefaultWeb()
	var err error
	w.Enabled, err = toBool("web.enabled", get(m, "enabled", true))
	c.take(err)
	if v, ok := m["listen"]; ok {
		s, serr := toStr("web.listen", v)
		a, perr := netip.ParseAddr(s)
		if serr != nil || perr != nil {
			c.add("web.listen", "web.listen must be an IP address such as 0.0.0.0 or 127.0.0.1")
		} else {
			w.Listen = a.String()
		}
	}
	w.Port, err = toInt("web.port", get(m, "port", int64(8790)), 1, 65535)
	c.take(err)
	w.CookieSecure, err = toBool("web.cookie_secure", get(m, "cookie_secure", false))
	c.take(err)
	w.SessionHours, err = toInt("web.session_hours", get(m, "session_hours", int64(12)), 1, 720)
	c.take(err)
	if v, ok := m["allowed_networks"]; ok {
		w.AllowedNetworks, err = networks("web.allowed_networks", v)
		c.take(err)
	}
	if v, ok := m["trusted_proxies"]; ok {
		w.TrustedProxies, err = networks("web.trusted_proxies", v)
		c.take(err)
	}
	return w, c.err()
}

// Parse validates settings. allowEmpty permits zero instances (the web UI starts that way, then adds them).
func Parse(data map[string]any, allowEmpty bool) (Settings, error) {
	var c collector
	if u := unknown(data, topKeys); len(u) > 0 {
		c.add("", "unknown setting(s) %s", strings.Join(u, ", "))
	}
	allowHTTP, err := toBool("allow_insecure_http", get(data, "allow_insecure_http", false))
	c.take(err)
	s := Settings{AllowInsecureHTTP: allowHTTP}
	configured := 0
	for _, kind := range apps.Kinds {
		raw, present := data[kind]
		if !present {
			continue
		}
		entries, ok := tables(raw)
		if !ok {
			c.add(kind, "%s must be a table or an array of tables", kind)
			continue
		}
		configured += len(entries)
		for i, e := range entries {
			inst, err := parseInstance(kind, i, e, allowHTTP)
			if err != nil {
				c.take(err)
				continue
			}
			s.Instances = append(s.Instances, inst)
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
	for n, k := range counts {
		if k > 1 {
			dups = append(dups, n)
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		c.add("name", "duplicate instance name(s): %s", strings.Join(dups, ", "))
	}
	level, err := toStr("log_level", get(data, "log_level", "INFO"))
	c.take(err)
	s.LogLevel = strings.ToUpper(level)
	if err == nil && !slices.Contains(logLevels, s.LogLevel) {
		c.add("log_level", "log_level must be one of %s", strings.Join(logLevels, ", "))
	}
	if err != nil {
		s.LogLevel = "INFO"
	}
	s.IntervalMinutes, err = toInt("interval_minutes", get(data, "interval_minutes", int64(60)), 5, 1440)
	c.take(err)
	s.StateDir, err = toStr("state_dir", get(data, "state_dir", "/state"))
	c.take(err)
	s.DryRun, err = toBool("dry_run", get(data, "dry_run", false))
	c.take(err)
	s.Web, err = parseWeb(get(data, "web", map[string]any{}))
	c.take(err)
	if s.Instances == nil {
		s.Instances = []Instance{}
	}
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
		data[i.Kind] = append(list, InstanceData(i))
	}
	return data
}

func strList(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func webData(w Web) map[string]any {
	d := DefaultWeb()
	out := map[string]any{}
	if w.Enabled != d.Enabled {
		out["enabled"] = w.Enabled
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
		out["allowed_networks"] = strList(w.AllowedNetworks)
	}
	if len(w.TrustedProxies) > 0 {
		out["trusted_proxies"] = strList(w.TrustedProxies)
	}
	return out
}

// InstanceData is one [[kind]] table, with only the non-default values (name and url always).
func InstanceData(i Instance) map[string]any {
	m := map[string]any{"name": i.Name, "url": i.URL}
	switch {
	case i.APIKeyFrom != "":
		m["api_key_from"] = i.APIKeyFrom
	case i.APIKeyFile != "":
		m["api_key_file"] = i.APIKeyFile
	case i.APIKeyEnv != "":
		m["api_key_env"] = i.APIKeyEnv
	}
	d := NewInstance(i.Kind, "", "")
	for _, f := range []struct {
		key        string
		value, def int
	}{
		{"missing", i.Missing, d.Missing}, {"cutoff", i.Cutoff, d.Cutoff}, {"daily_cap", i.DailyCap, d.DailyCap},
		{"max_queue", i.MaxQueue, d.MaxQueue}, {"timeout_seconds", i.TimeoutSeconds, d.TimeoutSeconds},
	} {
		if f.value != f.def {
			m[f.key] = int64(f.value)
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
		entry := map[string]any{"url": env.get(p + "_URL")}
		for key, def := range map[string]int64{"MISSING": 5, "CUTOFF": 0, "DAILY_CAP": 0, "MAX_QUEUE": 0} {
			n, err := num(p+"_"+key, def)
			if err != nil {
				return nil, err
			}
			entry[strings.ToLower(key)] = n
		}
		ids, err := envIDList(p+"_EXCLUDE", env.get(p+"_EXCLUDE"))
		if err != nil {
			return nil, err
		}
		entry["exclude"] = ids
		if f := env.get(p + "_API_KEY_FILE"); f != "" {
			entry["api_key_file"] = f
		} else if c := env.get(p + "_CONFIG"); c != "" {
			entry["api_key_from"] = c
		} else {
			entry["api_key_from"] = "/arr/" + kind + "/config.xml"
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

func envIDList(name, raw string) ([]any, error) {
	ids := []any{}
	for _, t := range regexp.MustCompile(`[,\s]+`).Split(raw, -1) {
		if strings.TrimSpace(t) == "" {
			continue
		}
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return nil, Errorf(name, "%s must be a comma-separated list of ids", name)
		}
		ids = append(ids, n)
	}
	return ids, nil
}

// Path is the settings file to use: the flag, else $ARR_TRICKLE_CONFIG, else the default.
func Path(flag string, env Environ) string {
	if flag != "" {
		return flag
	}
	if v := env.get("ARR_TRICKLE_CONFIG"); v != "" {
		return v
	}
	return DefaultConfigPath
}

// Decode parses TOML text into the generic structure Parse validates.
func Decode(text string) (map[string]any, error) {
	data := map[string]any{}
	if _, err := toml.Decode(text, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// Load reads the TOML file if it exists (or was named explicitly), else falls back to the environment.
func Load(flag string, env Environ, allowEmpty bool) (Settings, error) {
	explicit := flag != "" || env.get("ARR_TRICKLE_CONFIG") != ""
	path := Path(flag, env)
	info, err := os.Stat(path)
	if err == nil && info.Mode().IsRegular() {
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
	if explicit && !allowEmpty {
		return Settings{}, Errorf("", "config file %s not found", path)
	}
	data, err := FromEnv(env)
	if err != nil {
		return Settings{}, err
	}
	return Parse(data, allowEmpty)
}

func strerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		s := errno.Error()
		return strings.ToUpper(s[:1]) + s[1:]
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
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
	info, err := os.Stat(path)
	if err == nil {
		var raw []byte
		raw, err = os.ReadFile(path)
		if err == nil {
			if info.Mode().Perm()&0o004 != 0 {
				slog.Warn(fmt.Sprintf("%s is world-readable; consider chmod 600", path))
			}
			return strings.TrimSpace(string(raw)), nil
		}
	}
	return "", Errorf("", "cannot read %s: %s", path, strerror(err))
}

// ListenAddr is the host:port the web UI listens on.
func (w Web) ListenAddr() string { return net.JoinHostPort(w.Listen, strconv.Itoa(w.Port)) }
