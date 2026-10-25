// Package keystore keeps API keys entered in the web UI in <state_dir>/secrets/<instance>.key (mode 0600),
// never in config.toml.
package keystore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/state"
)

// The instance name becomes a file name, so it must not be able to climb out of the secrets dir.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Dir is the secrets directory.
func Dir(stateDir string) string { return filepath.Join(stateDir, "secrets") }

// Path is where name's key goes, if name is usable as a file name.
func Path(stateDir, name string) (string, error) {
	if !safeName.MatchString(name) {
		return "", config.Errorf("name", "to store a key, the name may only use letters, digits, '.', '-' and '_' (max 64)")
	}
	return filepath.Join(Dir(stateDir), name+".key"), nil
}

// Store writes the key and returns the path to use as api_key_file. The key is validated, never echoed.
func Store(stateDir, name, key string) (string, error) {
	if !config.IsAPIKeyFormat(key) {
		return "", config.Errorf("api_key", "the API key has an unexpected format (expected 20-64 letters and digits)")
	}
	path, err := Path(stateDir, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(Dir(stateDir), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(Dir(stateDir), 0o700); err != nil {
		return "", err
	}
	return path, state.AtomicWrite(path, []byte(key+"\n"), 0o600)
}

// IsManaged reports whether path is a key file this package wrote (so removing the instance may delete it).
func IsManaged(stateDir, path string) bool {
	if path == "" || !strings.HasSuffix(path, ".key") {
		return false
	}
	dir, err1 := filepath.Abs(Dir(stateDir))
	file, err2 := filepath.Abs(path)
	return err1 == nil && err2 == nil && filepath.Dir(file) == dir
}

// Remove deletes path if it is a managed key file.
func Remove(stateDir, path string) {
	if IsManaged(stateDir, path) {
		_ = os.Remove(path)
	}
}
