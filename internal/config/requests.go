package config

import (
	"regexp"
	"slices"
	"strings"
)

// The [requests] table: request sync re-adds approved Seerr/Ombi requests that never reached Radarr/Sonarr.

var (
	requestsKeys = []string{
		"enabled", "interval_minutes", "lookback_days", "grace_minutes", "timeout_seconds",
		"seerr_url", "seerr_key_file", "seerr_key_env", "ombi_url", "ombi_key_file", "ombi_key_env",
	}
	// RequestApps are the request apps request sync reads, in the order it reads them.
	RequestApps = []string{"seerr", "ombi"}
	// Seerr keys are base64 (68 characters, may end in '='); Ombi keys are 32 hex digits.
	requestKeyRE = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{20,128}$`)
)

// Requests is the [requests] table.
type Requests struct {
	Enabled         bool
	IntervalMinutes int
	LookbackDays    int // only requests made this recently are checked
	GraceMinutes    int // a request this new is left to its app, which may still be sending it
	TimeoutSeconds  int
	Apps            map[string]RequestApp // by name in RequestApps; only configured ones
}

// RequestApp is one request app's connection.
type RequestApp struct {
	URL, KeyFile, KeyEnv string
}

// DefaultRequests is the [requests] table when nothing is set: off.
func DefaultRequests() Requests {
	return Requests{IntervalMinutes: 60, LookbackDays: 14, GraceMinutes: 10, TimeoutSeconds: 60, Apps: map[string]RequestApp{}}
}

// Key reads the named request app's API key.
func (r Requests) Key(app string, env Environ) (string, error) {
	a := r.Apps[app]
	var key string
	var err error
	if a.KeyFile != "" {
		key, err = readSecretFile(app, a.KeyFile)
	} else if key = strings.TrimSpace(env.get(a.KeyEnv)); key == "" {
		err = Errorf("", "%s: environment variable %s is empty", app, a.KeyEnv)
	}
	if err == nil && !requestKeyRE.MatchString(key) {
		err = Errorf("", "%s: API key has an unexpected format", app)
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

func parseRequests(raw any) (Requests, error) {
	r := DefaultRequests()
	m, ok := raw.(map[string]any)
	if !ok {
		return r, Errorf("requests", "requests must be a table ([requests])")
	}
	var c collector
	if u := unknown(m, requestsKeys...); u != "" {
		c.add("requests", "requests: unknown setting(s) %s", u)
	}
	at := func(k string) string { return "requests." + k }
	r.Enabled = c.bool(at("enabled"), get(m, "enabled", false))
	r.IntervalMinutes = c.int(at("interval_minutes"), get(m, "interval_minutes", int64(r.IntervalMinutes)), 5, 1440)
	r.LookbackDays = c.int(at("lookback_days"), get(m, "lookback_days", int64(r.LookbackDays)), 1, 365)
	r.GraceMinutes = c.int(at("grace_minutes"), get(m, "grace_minutes", int64(r.GraceMinutes)), 0, 1440)
	r.TimeoutSeconds = c.int(at("timeout_seconds"), get(m, "timeout_seconds", int64(r.TimeoutSeconds)), 5, 600)
	for _, app := range RequestApps {
		rawURL, hasURL := m[app+"_url"]
		file, hasFile := m[app+"_key_file"]
		env, hasEnv := m[app+"_key_env"]
		if !hasURL {
			if hasFile || hasEnv {
				c.add(at(app+"_url"), "requests: %s_url is required when a %s key is set", app, app)
			}
			continue
		}
		a := RequestApp{URL: c.url(at(app+"_url"), rawURL)}
		if hasFile {
			a.KeyFile = c.str(at(app+"_key_file"), file)
		}
		if hasEnv {
			a.KeyEnv = c.str(at(app+"_key_env"), env)
		}
		if hasFile == hasEnv {
			c.add(at(app+"_key"), "requests: set exactly one of %s_key_file, %s_key_env", app, app)
		}
		r.Apps[app] = a
	}
	if r.Enabled && len(r.Apps) == 0 {
		c.add(at("enabled"), "requests: set seerr_url or ombi_url to enable request sync")
	}
	return r, c.err()
}

// requestsData is the [requests] table for r, with only the non-default values.
func requestsData(r Requests) map[string]any {
	d, out := DefaultRequests(), map[string]any{}
	if r.Enabled {
		out["enabled"] = true
	}
	for k, v := range map[string][2]int{
		"interval_minutes": {r.IntervalMinutes, d.IntervalMinutes}, "lookback_days": {r.LookbackDays, d.LookbackDays},
		"grace_minutes": {r.GraceMinutes, d.GraceMinutes}, "timeout_seconds": {r.TimeoutSeconds, d.TimeoutSeconds},
	} {
		if v[0] != v[1] {
			out[k] = int64(v[0])
		}
	}
	for _, app := range RequestApps {
		a, ok := r.Apps[app]
		if !ok {
			continue
		}
		out[app+"_url"] = a.URL
		if a.KeyFile != "" {
			out[app+"_key_file"] = a.KeyFile
		}
		if a.KeyEnv != "" {
			out[app+"_key_env"] = a.KeyEnv
		}
	}
	return out
}

// ConfiguredRequestApps is the names of the request apps r reads, in RequestApps order.
func (r Requests) ConfiguredRequestApps() []string {
	return slices.DeleteFunc(slices.Clone(RequestApps), func(app string) bool { _, ok := r.Apps[app]; return !ok })
}
