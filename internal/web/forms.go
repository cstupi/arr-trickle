package web

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/service"
	"github.com/cstupi/arr-trickle/internal/state"
)

// Form values are only converted to the right type (a numeric string becomes an int); every rule is
// config.Parse's, so the UI cannot accept anything the file would reject.

// GlobalFields are the top-level settings the UI edits.
var GlobalFields = []string{"interval_minutes", "log_level", "dry_run", "allow_insecure_http"}

// KeySource is one way an instance can get its key.
type KeySource struct{ Field, Label string }

// KeySources are the choices in the instance form.
var KeySources = []KeySource{
	{"api_key_from", "the app's config.xml (path)"},
	{"api_key_file", "a key file (path)"},
	{"api_key_env", "an environment variable (name)"},
}

func isKeySource(f string) bool {
	for _, k := range KeySources {
		if k.Field == f {
			return true
		}
	}
	return false
}

var (
	intRE    = regexp.MustCompile(`^[+-]?\d{1,18}$`)
	splitIDs = regexp.MustCompile(`[,\s]+`)
	numeric  = map[string]bool{"missing": true, "cutoff": true, "daily_cap": true, "max_queue": true, "timeout_seconds": true}
)

// InstanceFields are the instance form's settings.
var InstanceFields = []string{"name", "url", "missing", "cutoff", "daily_cap", "max_queue", "exclude", "timeout_seconds"}

// number converts a numeric string; anything else goes to Parse as-is, which rejects it.
func number(raw string) any {
	raw = strings.TrimSpace(raw)
	if intRE.MatchString(raw) {
		n, _ := strconv.ParseInt(raw, 10, 64)
		return n
	}
	return raw
}

// Form is a submitted form's values.
type Form map[string]string

// ApplyGlobal puts the general settings form into data.
func ApplyGlobal(data map[string]any, f Form) {
	data["interval_minutes"] = number(f["interval_minutes"])
	data["log_level"] = f["log_level"]
	data["dry_run"] = f["dry_run"] == "on"
	data["allow_insecure_http"] = f["allow_insecure_http"] == "on"
}

// InstanceValues is the form prefill. There is deliberately no field for the key itself: it is write-only.
func InstanceValues(inst *config.Instance) Form {
	if inst == nil {
		return Form{"key_source": "api_key_from", "key_ref": "", "missing": "5", "cutoff": "0", "timeout_seconds": "60"}
	}
	v := Form{
		"name": inst.Name, "url": inst.URL, "missing": strconv.Itoa(inst.Missing), "cutoff": strconv.Itoa(inst.Cutoff),
		"daily_cap": strconv.Itoa(inst.DailyCap), "max_queue": strconv.Itoa(inst.MaxQueue),
		"timeout_seconds": strconv.Itoa(inst.TimeoutSeconds),
	}
	ids := make([]string, len(inst.Exclude))
	for i, id := range inst.Exclude {
		ids[i] = strconv.Itoa(id)
	}
	v["exclude"] = strings.Join(ids, ", ")
	switch {
	case inst.APIKeyFrom != "":
		v["key_source"], v["key_ref"] = "api_key_from", inst.APIKeyFrom
	case inst.APIKeyFile != "":
		v["key_source"], v["key_ref"] = "api_key_file", inst.APIKeyFile
	default:
		v["key_source"], v["key_ref"] = "api_key_env", inst.APIKeyEnv
	}
	return v
}

// InstanceEntry is the [[kind]] table for a submitted instance form (without the write-only key).
func InstanceEntry(f Form) map[string]any {
	e := map[string]any{}
	for _, field := range InstanceFields {
		raw := f[field]
		switch {
		case field == "exclude":
			ids := []any{}
			for _, t := range splitIDs.Split(raw, -1) {
				if strings.TrimSpace(t) != "" {
					ids = append(ids, number(t))
				}
			}
			e[field] = ids
		case numeric[field]:
			if strings.TrimSpace(raw) != "" { // blank: the default
				e[field] = number(raw)
			}
		case field == "name" && strings.TrimSpace(raw) == "":
			// blank: Parse picks the default name
		default:
			e[field] = raw
		}
	}
	source := f["key_source"]
	if !isKeySource(source) {
		source = "api_key_source"
	}
	e[source] = f["key_ref"]
	return e
}

// Errors maps form fields to messages.
type Errors map[string][]string

// FieldErrors splits Parse errors into per-field messages for the form at prefix (e.g. "sonarr[1]") and the rest.
func FieldErrors(err error, prefix string) (Errors, []string) {
	byField := Errors{}
	var general []string
	cerr, ok := err.(*config.Error)
	if !ok {
		return byField, []string{err.Error()}
	}
	for _, fe := range cerr.Errors {
		name := ""
		switch {
		case contains(GlobalFields, fe.Where):
			name = fe.Where
		case fe.Where == "name":
			name = "name" // duplicate names
		case prefix != "" && strings.HasPrefix(fe.Where, prefix+"."):
			name = strings.TrimPrefix(fe.Where, prefix+".")
			if isKeySource(name) || name == "api_key" {
				name = "key_ref"
			}
		}
		if name != "" {
			byField[name] = append(byField[name], fe.Msg)
		} else {
			general = append(general, fe.Msg)
		}
	}
	return byField, general
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Editor validates and writes config.toml atomically (keeping one .bak), then hot-reloads the scheduler.
type Editor struct {
	Path      string
	Scheduler *service.Scheduler
	mu        sync.Mutex
}

// Lock serializes read-modify-write cycles on the settings.
func (e *Editor) Lock() { e.mu.Lock() }

// Unlock ends a Lock.
func (e *Editor) Unlock() { e.mu.Unlock() }

func writable(path string) bool {
	f, err := os.CreateTemp(path, ".writetest-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// Problem is why saving can't work, if it can't (e.g. config.toml mounted read-only).
func (e *Editor) Problem() string {
	dir := filepath.Dir(e.Path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return dir + " does not exist"
	}
	if !writable(dir) {
		return dir + " is not writable; mount the config directory read-write to save from the UI"
	}
	if f, err := os.OpenFile(e.Path, os.O_WRONLY, 0); err == nil {
		f.Close()
	} else if !os.IsNotExist(err) {
		return e.Path + " is not writable"
	}
	return ""
}

// Save validates data, writes it, and applies it. The caller holds Lock.
func (e *Editor) Save(data map[string]any) (config.Settings, error) {
	s, err := config.Parse(data, true)
	if err != nil {
		return s, err
	}
	text := config.Dump(s)
	// Belt and braces: what we write must read back as exactly what we validated.
	back, derr := config.Decode(text)
	if derr == nil {
		var again config.Settings
		again, derr = config.Parse(back, true)
		if derr == nil && !reflect.DeepEqual(again, s) {
			derr = fmt.Errorf("mismatch")
		}
	}
	if derr != nil {
		return s, config.Errorf("", "internal error: settings did not round-trip; nothing was saved")
	}
	if p := e.Problem(); p != "" {
		return s, config.Errorf("", "cannot save: %s", p)
	}
	perm := os.FileMode(0o644)
	if info, err := os.Stat(e.Path); err == nil {
		perm = info.Mode().Perm()
		old, err := os.ReadFile(e.Path)
		if err != nil {
			return s, config.Errorf("", "cannot save: %s", err)
		}
		if err := state.AtomicWrite(e.Path+".bak", old, perm); err != nil {
			return s, config.Errorf("", "cannot save: %s", err)
		}
	}
	if err := state.AtomicWrite(e.Path, []byte(text), perm); err != nil {
		return s, config.Errorf("", "cannot save: %s", err)
	}
	e.Scheduler.UpdateSettings(s)
	return s, nil
}
