package web

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cstupi/arr-trickle/internal/config"
	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func newInstance(over ...string) url.Values {
	f := form("kind", "sonarr", "name", "sonarr", "url", "http://sonarr:8989", "key_source", "api_key_env",
		"key_ref", "ARR_TEST_KEY", "missing", "4", "cutoff", "1", "daily_cap", "", "max_queue", "", "exclude", "3, 4",
		"timeout_seconds", "60", "api_key", "")
	for i := 0; i+1 < len(over); i += 2 {
		f.Set(over[i], over[i+1])
	}
	return f
}

func instancesOf(e *env, kind string) []config.Instance {
	var out []config.Instance
	for _, i := range e.sched.Settings().Instances {
		if i.Kind == kind {
			out = append(out, i)
		}
	}
	return out
}

func fileText(t *testing.T, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// after returns the HTML between a field's input and the next label: where its errors are shown.
func after(html, field string) string {
	_, rest, ok := strings.Cut(html, `name="`+field+`"`)
	if !ok {
		return ""
	}
	block, _, _ := strings.Cut(rest, "<label")
	return block
}

func TestSettingsPage(t *testing.T) {
	e, b := web(t)
	page := b.get("/settings").text()
	want(t, strings.Contains(page, "radarr") && strings.Contains(page, e.config) && strings.Contains(page, "comments in that file are not preserved"), "page")
	want(t, !strings.Contains(page, "Saving is unavailable"), "writable")
}

func TestGlobalSaveIsAtomicKeepsABackupAndHotReloads(t *testing.T) {
	e, b := web(t)
	original := fileText(t, e.config)
	r := b.post("/settings", form("interval_minutes", "15", "log_level", "DEBUG", "dry_run", "on"))
	want(t, r.Code == 303 && r.Header().Get("Location") == "/settings", "%d", r.Code)
	want(t, strings.Contains(b.get("/settings").text(), "Settings saved"), "flash")
	s := e.sched.Settings()
	want(t, s.IntervalMinutes == 15 && s.LogLevel == "DEBUG" && s.DryRun && !s.AllowInsecureHTTP, "%+v", s)
	want(t, fileText(t, e.config+".bak") == original, "backup")
	loaded, err := config.Load(e.config, config.Environ{}, false)
	want(t, err == nil && loaded.IntervalMinutes == 15, "%v", err)
	want(t, strings.Contains(strings.SplitN(fileText(t, e.config), "\n", 2)[0], "web UI"), "header")
	entries, _ := os.ReadDir(filepath.Dir(e.config))
	for _, en := range entries {
		want(t, !strings.HasSuffix(en.Name(), ".tmp"), "temp file left: %s", en.Name())
	}
}

func TestGlobalValidationErrors(t *testing.T) {
	cases := []struct{ interval, level, msg, field string }{
		{"abc", "INFO", "interval_minutes must be an integer", "interval_minutes"},
		{"1", "INFO", "interval_minutes must be between 5 and 1440", "interval_minutes"},
		{"", "INFO", "interval_minutes must be an integer", "interval_minutes"},
		{"60", "LOUD", "log_level must be one of", "log_level"},
	}
	for _, c := range cases {
		e, b := web(t)
		before := fileText(t, e.config)
		r := b.post("/settings", form("interval_minutes", c.interval, "log_level", c.level))
		want(t, r.Code == 400 && strings.Contains(after(r.text(), c.field), c.msg), "%s: %s", c.msg, after(r.text(), c.field))
		want(t, fileText(t, e.config) == before && e.sched.Settings().IntervalMinutes == 60, "unchanged")
	}
}

func TestAddEditAndRemoveInstances(t *testing.T) {
	e, b := web(t)
	want(t, strings.Contains(b.get("/settings/instances/new?kind=sonarr").text(), "Add Sonarr instance"), "add page")
	r := b.post("/settings/instances/new", newInstance())
	want(t, r.Code == 303, "%d %s", r.Code, r.text())
	s := instancesOf(e, "sonarr")[0]
	want(t, s.Missing == 4 && s.Cutoff == 1 && slices.Equal(s.Exclude, []int{3, 4}) && s.APIKeyEnv == "ARR_TEST_KEY", "%+v", s)
	want(t, strings.Contains(fileText(t, e.config), "exclude = [3, 4]"), "written")
	want(t, b.post("/settings/instances/new", newInstance("name", "sonarr-4k", "url", "https://s4k.example.com")).Code == 303, "second sonarr")
	page := b.get("/settings/instances/sonarr/1").text()
	want(t, strings.Contains(page, `value="sonarr-4k"`) && strings.Contains(page, `value="https://s4k.example.com"`), "edit page")
	r = b.post("/settings/instances/sonarr/1", newInstance("name", "sonarr-uhd", "original_name", "sonarr-4k", "missing", "9"))
	want(t, r.Code == 303, "%d", r.Code)
	names := func() []string {
		var n []string
		for _, i := range e.sched.Settings().Instances {
			n = append(n, i.Name)
		}
		return n
	}
	want(t, slices.Equal(names(), []string{"sonarr", "sonarr-uhd", "radarr"}), "%v", names())
	want(t, b.post("/settings/instances/sonarr/0/delete", form("name", "sonarr")).Code == 303, "delete")
	want(t, slices.Equal(names(), []string{"sonarr-uhd", "radarr"}), "%v", names())
	b.post("/settings/instances/sonarr/0/delete", form("name", "sonarr-uhd"))
	want(t, !strings.Contains(fileText(t, e.config), "[[sonarr]]"), "all sonarr gone")
}

func TestBlankNameGetsTheDefault(t *testing.T) {
	e, b := web(t)
	b.post("/settings/instances/new", newInstance("name", ""))
	want(t, len(instancesOf(e, "sonarr")) == 1 && instancesOf(e, "sonarr")[0].Name == "sonarr", "default name")
}

func TestInstanceValidationErrorsAreShownNextToFields(t *testing.T) {
	cases := []struct{ field, value, at, msg string }{
		{"url", "ftp://sonarr", "url", "must be an http:// or https:// URL"},
		{"url", "http://user:pw@sonarr:8989", "url", "must not contain credentials"},
		{"url", "http://sonarr.example.com", "url", "refusing to send an API key over plain http"},
		{"url", "", "url", "must be a non-empty string"},
		{"missing", "lots", "missing", "missing must be an integer"},
		{"missing", "101", "missing", "missing must be between 0 and 100"},
		{"cutoff", "-1", "cutoff", "cutoff must be between 0 and 100"},
		{"daily_cap", "1.5", "daily_cap", "daily_cap must be an integer"},
		{"max_queue", "100001", "max_queue", "max_queue must be between"},
		{"timeout_seconds", "1", "timeout_seconds", "timeout_seconds must be between 5 and 600"},
		{"exclude", "1, x", "exclude", "exclude must be a list of positive integer ids"},
		{"exclude", "0", "exclude", "exclude must be a list of positive integer ids"},
		{"key_ref", " ", "key_ref", "must be a non-empty string"},
		{"key_source", "api_key_raw", "key_ref", "set exactly one of"},
		{"name", "radarr", "name", "duplicate instance name(s): radarr"},
		{"api_key", "tooshort", "api_key", "unexpected format"},
	}
	for _, c := range cases {
		e, b := web(t)
		before := fileText(t, e.config)
		r := b.post("/settings/instances/new", newInstance(c.field, c.value))
		want(t, r.Code == 400 && strings.Contains(after(r.text(), c.at), c.msg), "%s=%q: %d %s", c.field, c.value, r.Code, after(r.text(), c.at))
		want(t, fileText(t, e.config) == before && len(e.sched.Settings().Instances) == 1, "unchanged")
		want(t, !strings.Contains(r.text(), tu.Key), "key echoed")
	}
	e, b := web(t)
	r := b.post("/settings/instances/new", newInstance("api_key", tu.Key, "name", "has space"))
	want(t, r.Code == 400 && strings.Contains(after(r.text(), "name"), "name may only use") && !strings.Contains(r.text(), tu.Key), "unsafe name")
	_ = e
}

func TestSeveralErrorsAtOnce(t *testing.T) {
	_, b := web(t)
	r := b.post("/settings/instances/new", newInstance("missing", "-1", "cutoff", "x", "url", "ftp://x"))
	text := r.text()
	want(t, r.Code == 400 && strings.Contains(text, "missing must be between") && strings.Contains(text, "cutoff must be an integer") && strings.Contains(text, "http://"), "all shown")
}

func TestInvalidFormKeepsWhatWasTyped(t *testing.T) {
	_, b := web(t)
	r := b.post("/settings/instances/new", newInstance("missing", "lots", "url", "http://typed:1"))
	want(t, strings.Contains(r.text(), `value="lots"`) && strings.Contains(r.text(), `value="http://typed:1"`), "kept")
}

var keyRefRE = regexp.MustCompile(`name="key_ref" value="([^"]*)"`)

func TestWriteOnlyAPIKey(t *testing.T) {
	e, b := web(t)
	r := b.post("/settings/instances/new", newInstance("api_key", tu.Key, "key_source", "api_key_env", "key_ref", "IGNORED"))
	want(t, r.Code == 303, "%d %s", r.Code, r.text())
	keyFile := filepath.Join(e.state, "secrets", "sonarr.key")
	info, err := os.Stat(keyFile)
	want(t, err == nil && info.Mode().Perm() == 0o600 && strings.TrimSpace(fileText(t, keyFile)) == tu.Key, "stored 0600")
	s := instancesOf(e, "sonarr")[0]
	key, _ := s.APIKey(nil)
	want(t, s.APIKeyFile == keyFile && s.APIKeyEnv == "" && key == tu.Key, "%+v", s)
	want(t, !strings.Contains(fileText(t, e.config), tu.Key), "not in config.toml")
	edit := b.get("/settings/instances/sonarr/0").text()
	want(t, !strings.Contains(edit, tu.Key) && strings.Contains(edit, `type="password" value=""`) && strings.Contains(edit, "uses a key you entered here"), "edit page")
	want(t, strings.Contains(b.get("/settings").text(), "key stored from the UI"), "settings list")
	ref := keyRefRE.FindStringSubmatch(edit)[1]
	// saving again without a key keeps the stored one
	r = b.post("/settings/instances/sonarr/0", newInstance("original_name", "sonarr", "key_source", "api_key_file", "key_ref", ref, "missing", "7"))
	_, err = os.Stat(keyFile)
	want(t, r.Code == 303 && err == nil && instancesOf(e, "sonarr")[0].Missing == 7, "kept")
	// switching to another source deletes the stored key
	b.post("/settings/instances/sonarr/0", newInstance("original_name", "sonarr", "key_source", "api_key_env"))
	_, err = os.Stat(keyFile)
	want(t, os.IsNotExist(err), "deleted")
}

func TestRemovingAnInstanceDeletesItsStoredKey(t *testing.T) {
	e, b := web(t)
	b.post("/settings/instances/new", newInstance("api_key", tu.Key))
	b.post("/settings/instances/sonarr/0/delete", form("name", "sonarr"))
	_, err := os.Stat(filepath.Join(e.state, "secrets", "sonarr.key"))
	want(t, os.IsNotExist(err), "deleted")
}

func TestStaleFormsChangeNothing(t *testing.T) {
	e, b := web(t)
	before := fileText(t, e.config)
	r := b.post("/settings/instances/radarr/0", newInstance("kind", "radarr", "name", "x", "original_name", "not-radarr"))
	want(t, r.Code == 303 && strings.Contains(b.get("/settings").text(), "changed in the meantime"), "stale edit")
	b.post("/settings/instances/radarr/0/delete", form("name", "not-radarr"))
	want(t, fileText(t, e.config) == before, "unchanged")
}

func TestUnknownInstances404(t *testing.T) {
	_, b := web(t)
	for _, p := range []string{"/settings/instances/plex/0", "/settings/instances/radarr/5", "/settings/instances/radarr/x", "/settings/instances/new?kind=plex"} {
		want(t, b.get(p).Code == 404, "%s", p)
	}
	want(t, b.post("/settings/instances/new", newInstance("kind", "plex")).Code == 404, "post")
	want(t, b.post("/settings/instances/plex/0", newInstance()).Code == 404, "post edit")
	want(t, b.post("/settings/instances/plex/0/delete", form("name", "x")).Code == 404, "post delete")
}

func TestReadOnlyConfigIsExplained(t *testing.T) {
	e, b := web(t)
	dir := filepath.Dir(e.config)
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if writable(dir) {
		t.Skip("running as root: permissions don't apply")
	}
	want(t, strings.Contains(b.get("/settings").text(), "Saving is unavailable"), "shown")
	r := b.post("/settings", form("interval_minutes", "15", "log_level", "INFO"))
	want(t, r.Code == 400 && strings.Contains(r.text(), "not writable"), "global")
	r = b.post("/settings/instances/new", newInstance("api_key", tu.Key))
	want(t, r.Code == 400 && strings.Contains(r.text(), "not writable"), "instance")
	want(t, b.post("/control/dry-run", nil).Code == 303 && strings.Contains(b.get("/").text(), "not writable"), "dry run")
	b.post("/settings/instances/radarr/0/delete", form("name", "radarr"))
	want(t, strings.Contains(b.get("/settings").text(), "not writable") && e.sched.Settings().IntervalMinutes == 60, "delete")
}

func TestReadOnlyConfigFile(t *testing.T) {
	e, b := web(t)
	os.Chmod(e.config, 0o400)
	defer os.Chmod(e.config, 0o600)
	if f, err := os.OpenFile(e.config, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("running as root: permissions don't apply")
	}
	want(t, strings.Contains(b.get("/settings").text(), e.config+" is not writable"), "file")
}

func TestEnvConfiguredSetupSavesANewFile(t *testing.T) {
	t.Setenv("ARR_TEST_KEY", tu.Key)
	e := newEnv(t, opts{noFile: true})
	b := e.browser(lanIP).login()
	_, err := os.Stat(e.config)
	want(t, os.IsNotExist(err), "no file yet")
	want(t, b.post("/settings/instances/new", newInstance()).Code == 303, "saved")
	s, err := config.Load(e.config, config.Environ{}, false)
	want(t, err == nil && s.Instances[0].Name == "sonarr", "%v", err)
	_, err = os.Stat(e.config + ".bak")
	want(t, os.IsNotExist(err), "no backup of nothing")
}

func TestMissingConfigDirIsExplained(t *testing.T) {
	e, b := web(t)
	e.srv.Editor.Path = filepath.Join(t.TempDir(), "nope", "config.toml")
	want(t, strings.Contains(b.get("/settings").text(), "does not exist"), "explained")
}

func TestFieldErrorsMapping(t *testing.T) {
	errs, general := FieldErrors(os.ErrNotExist, "")
	want(t, len(errs) == 0 && len(general) == 1, "non-config errors are general")
	_, err := config.Parse(map[string]any{"sonarr": []any{map[string]any{"url": "x", "api_key_env": "K", "bogus": int64(1)}}}, true)
	errs, general = FieldErrors(err, "sonarr[0]")
	want(t, len(errs["url"]) == 1 && len(general) == 1 && strings.Contains(general[0], "unknown setting"), "%v %v", errs, general)
}
