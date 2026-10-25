package web

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/service"
	"github.com/cstupi/arr-trickle/internal/state"
)

// Form values are only converted to the right type (a numeric string becomes an int); every rule is
// config.Parse's, so the UI cannot accept anything the file would reject.

var globalFields = []string{"interval_minutes", "log_level", "dry_run", "allow_insecure_http"}

type keySource struct{ Field, Label string }

var keySources = []keySource{
	{"api_key_from", "the app's config.xml (path)"},
	{"api_key_file", "a key file (path)"},
	{"api_key_env", "an environment variable (name)"},
}

func isKeySource(f string) bool {
	return slices.ContainsFunc(keySources, func(k keySource) bool { return k.Field == f })
}

var intRE = regexp.MustCompile(`^[+-]?\d{1,18}$`)

// number converts a numeric string; anything else goes to Parse as-is, which rejects it.
func number(raw string) any {
	if raw = strings.TrimSpace(raw); intRE.MatchString(raw) {
		n, _ := strconv.ParseInt(raw, 10, 64)
		return n
	}
	return raw
}

type postForm map[string]string

func applyGlobal(data map[string]any, f postForm) {
	data["interval_minutes"] = number(f["interval_minutes"])
	data["log_level"] = f["log_level"]
	data["dry_run"] = f["dry_run"] == "on"
	data["allow_insecure_http"] = f["allow_insecure_http"] == "on"
}

// instanceValues is the form prefill. There is deliberately no field for the key itself: it is write-only.
func instanceValues(inst *config.Instance) postForm {
	if inst == nil {
		return postForm{"key_source": "api_key_from", "missing": "5", "cutoff": "0", "timeout_seconds": "60"}
	}
	ids := make([]string, len(inst.Exclude))
	for i, id := range inst.Exclude {
		ids[i] = strconv.Itoa(id)
	}
	v := postForm{
		"name": inst.Name, "url": inst.URL, "missing": strconv.Itoa(inst.Missing), "cutoff": strconv.Itoa(inst.Cutoff),
		"daily_cap": strconv.Itoa(inst.DailyCap), "max_queue": strconv.Itoa(inst.MaxQueue),
		"timeout_seconds": strconv.Itoa(inst.TimeoutSeconds), "exclude": strings.Join(ids, ", "),
		"key_source": "api_key_env", "key_ref": inst.APIKeyEnv,
	}
	if inst.APIKeyFrom != "" {
		v["key_source"], v["key_ref"] = "api_key_from", inst.APIKeyFrom
	} else if inst.APIKeyFile != "" {
		v["key_source"], v["key_ref"] = "api_key_file", inst.APIKeyFile
	}
	return v
}

// instanceEntry is the [[kind]] table for a submitted instance form (without the write-only key).
func instanceEntry(f postForm) map[string]any {
	ids := []any{}
	for t := range config.SplitIDs(f["exclude"]) {
		ids = append(ids, number(t))
	}
	e := map[string]any{"url": f["url"], "exclude": ids}
	if strings.TrimSpace(f["name"]) != "" { // blank: Parse picks the default name
		e["name"] = f["name"]
	}
	for _, k := range []string{"missing", "cutoff", "daily_cap", "max_queue", "timeout_seconds"} {
		if strings.TrimSpace(f[k]) != "" { // blank: the default
			e[k] = number(f[k])
		}
	}
	source := f["key_source"]
	if !isKeySource(source) {
		source = "api_key_source" // rejected by Parse
	}
	e[source] = f["key_ref"]
	return e
}

type fieldErrors map[string][]string

// splitErrors sorts Parse errors into per-field messages for the form at prefix (e.g. "sonarr[1]") and the rest.
func splitErrors(err error, prefix string) (fieldErrors, []string) {
	byField := fieldErrors{}
	var general []string
	cerr, ok := errors.AsType[*config.Error](err)
	if !ok {
		return byField, []string{err.Error()}
	}
	for _, fe := range cerr.Errors {
		name, isField := strings.CutPrefix(fe.Where, prefix+".")
		switch {
		case slices.Contains(globalFields, fe.Where) || fe.Where == "name": // "name": duplicate names
			name = fe.Where
		case prefix == "" || !isField:
			general = append(general, fe.Msg)
			continue
		case isKeySource(name) || name == "api_key":
			name = "key_ref"
		}
		byField[name] = append(byField[name], fe.Msg)
	}
	return byField, general
}

// editor validates and writes config.toml atomically (keeping one .bak), then hot-reloads the scheduler.
// Lock it around every read-modify-save of the settings.
type editor struct {
	sync.Mutex
	path      string
	scheduler *service.Scheduler
}

// problem is why saving can't work, if it can't (e.g. config.toml mounted read-only).
func (e *editor) problem() string {
	dir := filepath.Dir(e.path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return dir + " does not exist"
	}
	f, err := os.CreateTemp(dir, ".writetest-*")
	if err != nil {
		return dir + " is not writable; mount the config directory read-write to save from the UI"
	}
	f.Close()
	os.Remove(f.Name())
	if f, err := os.OpenFile(e.path, os.O_WRONLY, 0); err == nil {
		f.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return e.path + " is not writable"
	}
	return ""
}

// save validates data, writes it, and applies it. The caller holds the lock.
func (e *editor) save(data map[string]any) (config.Settings, error) {
	s, err := config.Parse(data, true)
	if err != nil {
		return s, err
	}
	text := config.Dump(s)
	// Belt and braces: what we write must read back as exactly what we validated.
	back, err := config.Decode(text)
	again, perr := config.Parse(back, true)
	if err != nil || perr != nil || !reflect.DeepEqual(again, s) {
		return s, config.Errorf("", "internal error: settings did not round-trip; nothing was saved")
	}
	if p := e.problem(); p != "" {
		return s, config.Errorf("", "cannot save: %s", p)
	}
	perm := os.FileMode(0o644)
	if old, err := os.ReadFile(e.path); err == nil {
		info, _ := os.Stat(e.path)
		perm = info.Mode().Perm()
		err = state.AtomicWrite(e.path+".bak", old, perm)
		if err != nil {
			return s, config.Errorf("", "cannot save: %s", err)
		}
	}
	if err := state.AtomicWrite(e.path, []byte(text), perm); err != nil {
		return s, config.Errorf("", "cannot save: %s", err)
	}
	e.scheduler.UpdateSettings(s)
	return s, nil
}
