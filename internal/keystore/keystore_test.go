package keystore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
)

func TestStoreWrites0600FileIn0700Dir(t *testing.T) {
	d := t.TempDir()
	path, err := Store(d, "sonarr-4k", tu.Key)
	if err != nil || path != filepath.Join(d, "secrets", "sonarr-4k.key") {
		t.Fatal(path, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != tu.Key {
		t.Fatal(string(raw))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if info, _ := os.Stat(dir(d)); info.Mode().Perm() != 0o700 {
		t.Fatal(info.Mode())
	}
	if !IsManaged(d, path) {
		t.Fatal("managed")
	}
	Remove(d, path)
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
	d := t.TempDir()
	other := filepath.Join(d, "other.key")
	os.WriteFile(other, []byte(tu.Key), 0o600)
	Remove(d, other)
	Remove(d, "")
	if _, err := os.Stat(other); err != nil || IsManaged(d, "") || IsManaged(d, filepath.Join(dir(d), "x.txt")) {
		t.Fatal("removed an unmanaged file")
	}
}

func TestUnwritableStateDir(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "secrets"), nil, 0o600) // a file where the directory should be
	if _, err := Store(d, "sonarr", tu.Key); err == nil {
		t.Fatal("expected an error")
	}
}
