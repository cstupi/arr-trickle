package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRequestsDefaultToOff(t *testing.T) {
	s, err := Parse(map[string]any{}, true)
	if err != nil || !reflect.DeepEqual(s.Requests, DefaultRequests()) || s.Requests.Enabled {
		t.Fatal(s.Requests, err)
	}
	if _, ok := ToData(s)["requests"]; ok {
		t.Fatal("a default [requests] table is not written")
	}
	if strings.Contains(Dump(s), "[requests]") {
		t.Fatal(Dump(s))
	}
}

func TestRequestsParse(t *testing.T) {
	text := `
[requests]
enabled = true
interval_minutes = 120
seerr_url = "http://seerr:5055/"
seerr_key_file = "/state/secrets/seerr.key"
ombi_url = "http://ombi:3579/ombi"
ombi_key_env = "OMBI_KEY"
`
	data, err := Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data, true)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Requests
	if !r.Enabled || r.IntervalMinutes != 120 || r.LookbackDays != 14 || r.GraceMinutes != 10 ||
		r.Apps["seerr"] != (RequestApp{URL: "http://seerr:5055", KeyFile: "/state/secrets/seerr.key"}) ||
		r.Apps["ombi"] != (RequestApp{URL: "http://ombi:3579/ombi", KeyEnv: "OMBI_KEY"}) {
		t.Fatalf("%+v", r)
	}
	if !slices.Equal(r.ConfiguredRequestApps(), []string{"seerr", "ombi"}) {
		t.Fatal(r.ConfiguredRequestApps())
	}
	if !strings.Contains(Dump(s), "[requests]\nenabled = true\ninterval_minutes = 120\nseerr_url") {
		t.Fatal(Dump(s))
	}
}

func TestRequestsValidation(t *testing.T) {
	cases := []struct {
		table map[string]any
		where string
		want  string
	}{
		{map[string]any{"enabled": true}, "requests.enabled", "set seerr_url or ombi_url"},
		{map[string]any{"seerr_url": "http://seerr:5055"}, "requests.seerr_key", "exactly one of seerr_key_file, seerr_key_env"},
		{map[string]any{"ombi_url": "http://o:1", "ombi_key_file": "/a", "ombi_key_env": "B"}, "requests.ombi_key", "exactly one"},
		{map[string]any{"ombi_key_env": "B"}, "requests.ombi_url", "ombi_url is required"},
		{map[string]any{"seerr_url": "http://seerr.example.com", "seerr_key_env": "K"}, "requests.seerr_url", "plain http"},
		{map[string]any{"interval_minutes": int64(1)}, "requests.interval_minutes", "between 5 and 1440"},
		{map[string]any{"lookback_days": int64(0)}, "requests.lookback_days", "between 1 and 365"},
		{map[string]any{"grace_minutes": int64(-1)}, "requests.grace_minutes", "between 0 and 1440"},
		{map[string]any{"enabled": "yes"}, "requests.enabled", "true or false"},
		{map[string]any{"sonarr_url": "x"}, "requests", "unknown setting(s) sonarr_url"},
		{map[string]any{"seerr_url": "http://s:1", "seerr_key_file": ""}, "requests.seerr_key_file", "non-empty"},
	}
	for _, c := range cases {
		_, err := Parse(map[string]any{"requests": c.table}, true)
		e, ok := err.(*Error)
		if !ok || !slices.ContainsFunc(e.Errors, func(fe FieldError) bool { return fe.Where == c.where && strings.Contains(fe.Msg, c.want) }) {
			t.Errorf("%v: %v", c.table, err)
		}
	}
	if _, err := Parse(map[string]any{"requests": "on"}, true); err == nil || !strings.Contains(err.Error(), "must be a table") {
		t.Fatal(err)
	}
}

func TestRequestsKey(t *testing.T) {
	dir := t.TempDir()
	good, short := filepath.Join(dir, "seerr.key"), filepath.Join(dir, "short.key")
	seerrKey := "fakeseerrfakeseerrfakeseerrfakeseerr=="
	os.WriteFile(good, []byte(seerrKey+"\n"), 0o600)
	os.WriteFile(short, []byte("abc"), 0o600)
	r := DefaultRequests()
	r.Apps = map[string]RequestApp{
		"seerr": {URL: "http://s", KeyFile: good}, "ombi": {URL: "http://o", KeyEnv: "OMBI"},
	}
	if k, err := r.Key("seerr", nil); err != nil || k != seerrKey {
		t.Fatal(k, err)
	}
	if k, err := r.Key("ombi", Environ{"OMBI": "fakeombifakeombifakeombifakeombi"}); err != nil || k == "" {
		t.Fatal(k, err)
	}
	if _, err := r.Key("ombi", Environ{}); err == nil || !strings.Contains(err.Error(), "OMBI is empty") {
		t.Fatal(err)
	}
	r.Apps["seerr"] = RequestApp{URL: "http://s", KeyFile: short}
	if _, err := r.Key("seerr", nil); err == nil || !strings.Contains(err.Error(), "unexpected format") {
		t.Fatal(err)
	}
	r.Apps["seerr"] = RequestApp{URL: "http://s", KeyFile: filepath.Join(dir, "missing")}
	if _, err := r.Key("seerr", nil); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatal(err)
	}
}
