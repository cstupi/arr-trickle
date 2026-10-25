package keystore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func TestStoreWrites0600FileIn0700Dir(t *testing.T) {
	dir := t.TempDir()
	path, err := Store(dir, "sonarr-4k", tu.Key)
	if err != nil || path != filepath.Join(dir, "secrets", "sonarr-4k.key") {
		t.Fatal(path, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != tu.Key {
		t.Fatal(string(raw))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if info, _ := os.Stat(Dir(dir)); info.Mode().Perm() != 0o700 {
		t.Fatal(info.Mode())
	}
	if !IsManaged(dir, path) {
		t.Fatal("managed")
	}
	Remove(dir, path)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("not removed")
	}
}

func TestUnsafeNamesRejected(t *testing.T) {
	for _, name := range []string{"../evil", "a/b", ".hidden", "", strings.Repeat("x", 65), "spa ce"} {
		if _, err := Store(t.TempDir(), name, tu.Key); err == nil || !strings.Contains(err.Error(), "name may only use") {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestBadKeyFormatRejectedWithoutEcho(t *testing.T) {
	_, err := Store(t.TempDir(), "sonarr", "not a key; rm -rf /")
	if err == nil || !strings.Contains(err.Error(), "unexpected format") || strings.Contains(err.Error(), "rm -rf") {
		t.Fatal(err)
	}
}

func TestOnlyManagedFilesAreRemoved(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.key")
	os.WriteFile(other, []byte(tu.Key), 0o600)
	Remove(dir, other)
	Remove(dir, "")
	if _, err := os.Stat(other); err != nil || IsManaged(dir, "") || IsManaged(dir, filepath.Join(Dir(dir), "x.txt")) {
		t.Fatal("removed an unmanaged file")
	}
}

func TestUnwritableStateDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secrets"), nil, 0o600) // a file where the directory should be
	if _, err := Store(dir, "sonarr", tu.Key); err == nil {
		t.Fatal("expected an error")
	}
}
