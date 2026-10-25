package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func minimal(over map[string]any) map[string]any {
	entry := map[string]any{"url": "http://sonarr:8989", "api_key_env": "K"}
	maps.Copy(entry, over)
	return map[string]any{"sonarr": []any{entry}}
}

func with(data map[string]any, k string, v any) map[string]any {
	data[k] = v
	return data
}

func mustParse(t *testing.T, data map[string]any) Settings {
	t.Helper()
	s, err := Parse(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return s
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("want error containing %q, got %v", substr, err)
	}
}

func TestDefaults(t *testing.T) {
	s := mustParse(t, minimal(nil))
	i := s.Instances[0]
	if i.Name != "sonarr" || i.Missing != 5 || i.Cutoff != 0 || i.DailyCap != 0 || s.IntervalMinutes != 60 {
		t.Fatalf("defaults: %+v %+v", i, s)
	}
}

func TestMultipleInstancesGetUniqueDefaultNames(t *testing.T) {
	data := map[string]any{"sonarr": []any{
		map[string]any{"url": "http://a:1", "api_key_env": "A"}, map[string]any{"url": "http://b:1", "api_key_env": "B"},
	}}
	s := mustParse(t, data)
	if s.Instances[0].Name != "sonarr" || s.Instances[1].Name != "sonarr-2" {
		t.Fatal(s.Instances)
	}
}

func TestSingleTableFormAccepted(t *testing.T) {
	s := mustParse(t, map[string]any{"radarr": map[string]any{"url": "http://radarr:7878", "api_key_env": "K"}})
	if s.Instances[0].Kind != "radarr" {
		t.Fatal(s.Instances)
	}
}

func TestRejectsBadSettings(t *testing.T) {
	cases := []struct {
		data map[string]any
		msg  string
	}{
		{map[string]any{}, "no apps configured"},
		{map[string]any{"sonar": []any{}}, "unknown setting"},
		{minimal(map[string]any{"missin": int64(3)}), "unknown setting"},
		{minimal(map[string]any{"missing": "3"}), "must be an integer"},
		{minimal(map[string]any{"missing": true}), "must be an integer"},
		{minimal(map[string]any{"missing": 1.5}), "must be an integer"},
		{minimal(map[string]any{"missing": int64(-1)}), "between"},
		{minimal(map[string]any{"exclude": []any{int64(1), "2"}}), "exclude"},
		{minimal(map[string]any{"exclude": []any{int64(0)}}), "exclude"},
		{minimal(map[string]any{"exclude": "1"}), "exclude"},
		{minimal(map[string]any{"url": "ftp://x"}), "http"},
		{minimal(map[string]any{"url": "http://user:pw@sonarr:8989"}), "credentials"},
		{minimal(map[string]any{"url": "http://sonarr:8989/?x=1"}), "query"},
		{minimal(map[string]any{"url": int64(3)}), "non-empty string"},
		{minimal(map[string]any{"api_key_file": "/x"}), "exactly one"},
		{map[string]any{"sonarr": []any{map[string]any{"url": "http://sonarr:8989"}}}, "exactly one"},
		{map[string]any{"sonarr": []any{"nope"}}, "must be a table"},
		{with(minimal(nil), "interval_minutes", int64(1)), "between"},
		{with(minimal(nil), "log_level", "LOUD"), "log_level"},
		{with(minimal(nil), "log_level", int64(3)), "log_level"},
		{with(minimal(nil), "dry_run", "yes"), "dry_run must be true or false"},
		{with(minimal(nil), "state_dir", ""), "state_dir"},
		{map[string]any{"sonarr": "nope"}, "table"},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			_, err := Parse(c.data, false)
			wantErr(t, err, c.msg)
		})
	}
}

func TestDuplicateNamesRejected(t *testing.T) {
	data := map[string]any{
		"sonarr": []any{map[string]any{"url": "http://a:1", "api_key_env": "A", "name": "x"}},
		"radarr": []any{map[string]any{"url": "http://b:1", "api_key_env": "B", "name": "x"}},
	}
	_, err := Parse(data, false)
	wantErr(t, err, "duplicate")
}

func TestPrivateAndPublicHosts(t *testing.T) {
	for _, h := range []string{"localhost", "sonarr", "127.0.0.1", "192.168.0.10", "10.0.0.2", "nas.local", "::1"} {
		if !isPrivateHost(h) {
			t.Errorf("%s should be private", h)
		}
	}
	// IPv6 literals have no dots: they must be judged as addresses, not as Docker service names.
	for _, h := range []string{"example.com", "8.8.8.8", "sonarr.mydomain.net", "2606:4700::1111"} {
		if isPrivateHost(h) {
			t.Errorf("%s should be public", h)
		}
	}
}

func TestPlainHTTPToPublicHostRefusedUnlessAllowed(t *testing.T) {
	_, err := Parse(minimal(map[string]any{"url": "http://sonarr.example.com"}), false)
	wantErr(t, err, "plain http")
	mustParse(t, with(minimal(map[string]any{"url": "http://sonarr.example.com"}), "allow_insecure_http", true))
	mustParse(t, minimal(map[string]any{"url": "https://sonarr.example.com"}))
	_, err = Parse(minimal(map[string]any{"url": "http://[2606:4700::1111]:8989"}), false)
	wantErr(t, err, "plain http")
	mustParse(t, minimal(map[string]any{"url": "http://[fd00::5]:8989"}))
}

func TestKeyFromArrConfig(t *testing.T) {
	s := mustParse(t, map[string]any{"sonarr": []any{map[string]any{"url": "http://s:1", "api_key_from": tu.ArrConfigXML(t)}}})
	if k, err := s.Instances[0].APIKey(nil); err != nil || k != tu.Key {
		t.Fatal(k, err)
	}
}

func TestKeyFromSecretFileWarnsIfWorldReadable(t *testing.T) {
	logs := tu.CaptureLogs(t)
	secret := filepath.Join(t.TempDir(), "key")
	os.WriteFile(secret, []byte(tu.Key+"\n"), 0o644)
	os.Chmod(secret, 0o644)
	s := mustParse(t, map[string]any{"sonarr": []any{map[string]any{"url": "http://s:1", "api_key_file": secret}}})
	if k, err := s.Instances[0].APIKey(nil); err != nil || k != tu.Key {
		t.Fatal(k, err)
	}
	if !strings.Contains(logs.String(), "world-readable") || strings.Contains(logs.String(), tu.Key) {
		t.Fatal(logs.String())
	}
	_, err := Instance{Name: "x", APIKeyFile: secret + "nope"}.APIKey(nil)
	wantErr(t, err, "cannot read")
}

func TestKeyFromEnvAndFormatCheck(t *testing.T) {
	inst := mustParse(t, minimal(nil)).Instances[0]
	if k, err := inst.APIKey(Environ{"K": tu.Key}); err != nil || k != tu.Key {
		t.Fatal(k, err)
	}
	_, err := inst.APIKey(Environ{})
	wantErr(t, err, "empty")
	_, err = inst.APIKey(Environ{"K": "short; rm -rf"})
	wantErr(t, err, "unexpected format")
	if strings.Contains(err.Error(), "rm -rf") {
		t.Fatal("the bad key must not be echoed")
	}
}

func TestMissingArrConfig(t *testing.T) {
	dir := t.TempDir()
	_, err := Instance{Name: "l", APIKeyFrom: filepath.Join(dir, "nope.xml")}.APIKey(nil)
	wantErr(t, err, "cannot read")
	os.WriteFile(filepath.Join(dir, "empty.xml"), []byte("<Config></Config>"), 0o600)
	_, err = Instance{Name: "l", APIKeyFrom: filepath.Join(dir, "empty.xml")}.APIKey(nil)
	wantErr(t, err, "no <ApiKey>")
}

func TestLoadTomlFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("interval_minutes = 30\n[[lidarr]]\nurl = \"http://lidarr:8686\"\napi_key_from = \""+
		tu.ArrConfigXML(t)+"\"\nmissing = 6\nexclude = [4, 3, 4]\n"), 0o600)
	s, err := Load(path, Environ{}, false)
	if err != nil || s.IntervalMinutes != 30 || !slices.Equal(s.Instances[0].Exclude, []int{3, 4}) {
		t.Fatal(s, err)
	}
}

func TestLoadBadToml(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("missing = = 3"), 0o600)
	_, err := Load(path, Environ{}, false)
	wantErr(t, err, "config.toml")
}

func TestExplicitMissingFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"), Environ{}, false)
	wantErr(t, err, "not found")
	_, err = Load("", Environ{"ARR_TRICKLE_CONFIG": filepath.Join(t.TempDir(), "nope.toml")}, false)
	wantErr(t, err, "not found")
}

func TestEnvFallback(t *testing.T) {
	dir := t.TempDir()
	env := Environ{
		"SONARR_URL": "http://sonarr:8989", "SONARR_MISSING": "4", "SONARR_EXCLUDE": "134, 135", "SONARR_CONFIG": "/c.xml",
		"INTERVAL_MINUTES": "15", "DRY_RUN": "yes", "STATE_DIR": dir, "LOG_LEVEL": "debug",
		"RADARR_URL": "http://radarr:7878", "RADARR_API_KEY_FILE": "/run/secrets/radarr",
		"LIDARR_URL": "http://lidarr:8686",
	}
	data, err := FromEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	s := mustParse(t, data)
	sonarr, radarr, lidarr := s.Instances[0], s.Instances[1], s.Instances[2]
	if sonarr.Missing != 4 || !slices.Equal(sonarr.Exclude, []int{134, 135}) || sonarr.APIKeyFrom != "/c.xml" {
		t.Fatal(sonarr)
	}
	if radarr.APIKeyFile != "/run/secrets/radarr" || lidarr.APIKeyFrom != "/arr/lidarr/config.xml" {
		t.Fatal(radarr, lidarr)
	}
	if s.IntervalMinutes != 15 || !s.DryRun || s.LogLevel != "DEBUG" || s.StateDir != dir {
		t.Fatal(s)
	}
	loaded, err := Load("", env, false) // no file at the default path: the environment is used
	if err != nil || len(loaded.Instances) != 3 {
		t.Fatal(loaded, err)
	}
}

func TestEnvFallbackBadNumbers(t *testing.T) {
	for _, env := range []Environ{
		{"SONARR_URL": "http://s:1", "SONARR_MISSING": "x"},
		{"SONARR_URL": "http://s:1", "SONARR_EXCLUDE": "1,a"},
		{"SONARR_URL": "http://s:1", "INTERVAL_MINUTES": "soon"},
	} {
		if _, err := FromEnv(env); err == nil {
			t.Errorf("%v should fail", env)
		}
	}
}

func repoFile(name string) string { return filepath.Join("..", "..", name) }

func TestExampleConfigIsValid(t *testing.T) {
	s, err := Load(repoFile("examples/config.toml"), Environ{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, i := range s.Instances {
		kinds = append(kinds, i.Kind)
	}
	if !slices.Equal(kinds, []string{"sonarr", "radarr", "lidarr"}) {
		t.Fatal(kinds)
	}
}

func TestUnreadableConfigIsAConfigError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[[sonarr]]\n"), 0o600)
	os.Chmod(path, 0)
	defer os.Chmod(path, 0o600)
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("running as root: permissions don't apply")
	}
	_, err := Load(path, Environ{}, false)
	wantErr(t, err, "permissions")
}

func TestAllErrorsAreCollectedWithTheirSetting(t *testing.T) {
	data := minimal(map[string]any{"missing": int64(-1), "url": "ftp://x", "exclude": []any{"a"}})
	data["interval_minutes"] = int64(1)
	data["log_level"] = "LOUD"
	_, err := Parse(data, false)
	where := map[string]bool{}
	for _, fe := range err.(*Error).Errors {
		where[fe.Where] = true
	}
	for _, w := range []string{"interval_minutes", "log_level", "sonarr[0].missing", "sonarr[0].url", "sonarr[0].exclude"} {
		if !where[w] {
			t.Errorf("no error for %s in %v", w, err)
		}
	}
}

func TestAllowEmpty(t *testing.T) {
	s, err := Parse(map[string]any{}, true)
	if err != nil || len(s.Instances) != 0 {
		t.Fatal(s, err)
	}
	s, err = Load(filepath.Join(t.TempDir(), "config.toml"), Environ{}, true) // the web UI may start without a file
	if err != nil || len(s.Instances) != 0 || !s.Web.Enabled {
		t.Fatal(s, err)
	}
}

func TestWebDefaults(t *testing.T) {
	w := mustParse(t, minimal(nil)).Web
	if !w.Enabled || w.Listen != "0.0.0.0" || w.Port != 8790 || w.CookieSecure || w.SessionHours != 12 || len(w.TrustedProxies) != 0 {
		t.Fatal(w)
	}
	if !slices.Contains(w.AllowedNetworks, "192.168.0.0/16") || !slices.Contains(w.AllowedNetworks, "fc00::/7") {
		t.Fatal(w.AllowedNetworks)
	}
	if w.ListenAddr() != "0.0.0.0:8790" {
		t.Fatal(w.ListenAddr())
	}
}

func TestWebSettingsAreNormalized(t *testing.T) {
	data := with(minimal(nil), "web", map[string]any{"allowed_networks": []any{"192.168.1.7/24", " ::1 "}, "trusted_proxies": []any{"10.0.0.2"}, "listen": "::"})
	w := mustParse(t, data).Web
	if !slices.Equal(w.AllowedNetworks, []string{"192.168.1.0/24", "::1/128"}) || !slices.Equal(w.TrustedProxies, []string{"10.0.0.2/32"}) || w.Listen != "::" {
		t.Fatal(w)
	}
}

func TestRejectsBadWebSettings(t *testing.T) {
	cases := []struct {
		web any
		msg string
	}{
		{"yes", "web must be a table"},
		{map[string]any{"enable": true}, "unknown setting"},
		{map[string]any{"enabled": "yes"}, "web.enabled must be true or false"},
		{map[string]any{"listen": "nas.local"}, "web.listen must be an IP address"},
		{map[string]any{"port": int64(0)}, "web.port must be between"},
		{map[string]any{"cookie_secure": int64(1)}, "web.cookie_secure"},
		{map[string]any{"session_hours": int64(0)}, "web.session_hours must be between"},
		{map[string]any{"allowed_networks": "192.168.0.0/16"}, "web.allowed_networks must be a list"},
		{map[string]any{"allowed_networks": []any{"lan"}}, "web.allowed_networks"},
		{map[string]any{"trusted_proxies": []any{int64(1)}}, "web.trusted_proxies must be a list"},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			_, err := Parse(with(minimal(nil), "web", c.web), false)
			wantErr(t, err, c.msg)
		})
	}
}

// --- the writer --------------------------------------------------------------------

const tricky = "quote \" backslash \\ tab \t newline \n bell \x07 del \x7f unicode é ✓ #notacomment"

func fullSettings() Settings {
	return Settings{
		Instances: []Instance{
			{Kind: "sonarr", Name: "sonarr", URL: "http://sonarr:8989", APIKeyFrom: "/arr/sonarr/config.xml", Missing: 4, Exclude: []int{}, TimeoutSeconds: 60},
			{Kind: "sonarr", Name: "sonarr 4k", URL: "https://sonarr4k.example.com", APIKeyFile: "/run/secrets/s", Missing: 5, Exclude: []int{1, 3}, TimeoutSeconds: 60},
			{Kind: "radarr", Name: tricky, URL: "http://radarr:7878", APIKeyEnv: "RADARR_KEY", Missing: 5, Cutoff: 2, DailyCap: 50, Exclude: []int{}, TimeoutSeconds: 60},
			{Kind: "lidarr", Name: "lidarr", URL: "http://10.0.0.5:8686", APIKeyFrom: "/x.xml", Missing: 5, MaxQueue: 9, Exclude: []int{}, TimeoutSeconds: 5},
		},
		IntervalMinutes: 15, StateDir: "/data/state", DryRun: true, LogLevel: "DEBUG", AllowInsecureHTTP: true,
		Web: Web{
			Enabled: false, Listen: "127.0.0.1", Port: 9000, CookieSecure: true, SessionHours: 2,
			AllowedNetworks: []string{"192.168.1.0/24", "fd00::/8"}, TrustedProxies: []string{"10.0.0.2/32"},
		},
	}
}

func roundTrip(t *testing.T, s Settings) Settings {
	t.Helper()
	data, err := Decode(Dump(s))
	if err != nil {
		t.Fatalf("written TOML does not parse: %v\n%s", err, Dump(s))
	}
	back, err := Parse(data, true)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestRoundTrip(t *testing.T) {
	empty, _ := Parse(map[string]any{}, true)
	one, _ := Parse(map[string]any{"radarr": []any{map[string]any{"url": "http://r:1", "api_key_env": "K"}}}, true)
	for _, s := range []Settings{fullSettings(), empty, one} {
		if back := roundTrip(t, s); !reflect.DeepEqual(back, s) {
			t.Fatalf("round trip changed settings:\n got %+v\nwant %+v", back, s)
		}
	}
}

func TestRoundTripThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(Dump(fullSettings())), 0o600)
	s, err := Load(path, Environ{}, false)
	if err != nil || !reflect.DeepEqual(s, fullSettings()) {
		t.Fatal(s, err)
	}
}

func TestExampleConfigRoundTrips(t *testing.T) {
	s, err := Load(repoFile("examples/config.toml"), Environ{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if back := roundTrip(t, s); !reflect.DeepEqual(back, s) {
		t.Fatal(back)
	}
}

func TestDefaultsAreNotWritten(t *testing.T) {
	s, _ := Parse(map[string]any{"radarr": []any{map[string]any{"url": "http://r:1", "api_key_env": "K"}}}, true)
	want := map[string]any{"interval_minutes": int64(60), "radarr": []any{map[string]any{"name": "radarr", "url": "http://r:1", "api_key_env": "K"}}}
	if got := ToData(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestHeaderWarnsCommentsAreLost(t *testing.T) {
	text := Dump(Settings{IntervalMinutes: 60, StateDir: "/state", LogLevel: "INFO", Web: DefaultWeb()})
	if !strings.HasPrefix(text, "# arr-trickle settings, written by the web UI.") || !strings.Contains(text, "not preserved") {
		t.Fatal(text)
	}
}

func TestStringsAreEscaped(t *testing.T) {
	data, err := Decode("s = " + tomlString(tricky) + "\n")
	if err != nil || data["s"] != tricky {
		t.Fatal(data, err)
	}
}

func TestUnsupportedTypesPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	tomlValue(1.5)
}

func TestErrorFormatting(t *testing.T) {
	e := &Error{Errors: []FieldError{{"a", "first"}, {"b", "second"}}}
	if e.Error() != "first; second" {
		t.Fatal(e.Error())
	}
}
