package cli

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tu "github.com/cstupi/arr-trickle/internal/testutil"
	"github.com/cstupi/arr-trickle/internal/web"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n[web]\nenabled = false\n[[radarr]]\nurl = \"http://127.0.0.1:9\"\n"+
		"api_key_from = %q\ntimeout_seconds = 5\n", dir, tu.ArrConfigXML(t)), 0o600)
	return path, dir
}

func TestVersionAndHelp(t *testing.T) {
	code, out, _ := run("--version")
	if code != 0 || !strings.HasPrefix(out, "arr-trickle ") {
		t.Fatal(out)
	}
	if code, _, errs := run("--help"); code != 0 || !strings.Contains(errs, "reset-password") {
		t.Fatal(errs)
	}
	if code, _, _ := run("--bogus"); code != 2 {
		t.Fatal(code)
	}
	if code, _, errs := run("frobnicate"); code != 2 || !strings.Contains(errs, "unknown command") {
		t.Fatal(errs)
	}
	if code, _, _ := run("run", "--bogus"); code != 2 {
		t.Fatal(code)
	}
}

func TestValidate(t *testing.T) {
	path, _ := writeConfig(t)
	if code, out, _ := run("--config", path, "validate"); code != 0 || !strings.Contains(out, "radarr") {
		t.Fatal(out)
	}
}

func TestValidateBadKeySource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	os.WriteFile(path, fmt.Appendf(nil, "[[radarr]]\nurl = \"http://r:1\"\napi_key_from = %q\n", filepath.Join(dir, "missing.xml")), 0o600)
	if code, _, errs := run("--config", path, "validate"); code != 2 || !strings.Contains(errs, "cannot read") {
		t.Fatal(errs)
	}
}

func TestConfigErrorExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte("[[radarr]]\nurl = 3\n"), 0o600)
	if code, _, errs := run("--config", path, "validate"); code != 2 || !strings.Contains(errs, "configuration error") {
		t.Fatal(errs)
	}
}

func TestCheckUnreachableFailsWithoutLeakingKey(t *testing.T) {
	path, _ := writeConfig(t)
	code, out, _ := run("--config", path, "check")
	if code != 1 || !strings.Contains(out, "FAIL") || strings.Contains(out, tu.Key) {
		t.Fatal(out)
	}
}

func TestRunOnceWritesHeartbeatAndHealthcheck(t *testing.T) {
	path, dir := writeConfig(t)
	if code, _, _ := run("--config", path, "healthcheck"); code != 1 {
		t.Fatal("no heartbeat yet")
	}
	code, out, _ := run("--config", path, "run", "--once") // unreachable app -> nonzero
	if code != 1 || strings.Contains(out, tu.Key) {
		t.Fatal(code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "heartbeat")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("--config", path, "healthcheck"); code != 0 || !strings.Contains(out, "healthy") {
		t.Fatal(out)
	}
}

func TestHealthcheckStale(t *testing.T) {
	path, dir := writeConfig(t)
	os.WriteFile(filepath.Join(dir, "heartbeat"), []byte(strconv.FormatInt(time.Now().Add(-240*time.Hour).Unix(), 10)), 0o644)
	if code, out, _ := run("--config", path, "healthcheck"); code != 1 || !strings.Contains(out, "ago") {
		t.Fatal(out)
	}
}

func TestResetPasswordPrintsANewSetupCode(t *testing.T) {
	path, dir := writeConfig(t)
	store := web.NewAuthStore(dir)
	code, _ := store.NewSetupCode()
	store.CreateAdmin(code, "admin", "a long password", "a long password")
	rc, out, _ := run("--config", path, "reset-password")
	fields := strings.Fields(out)
	if rc != 0 || store.HasAdmin() || !store.CheckSetupCode(fields[len(fields)-1]) {
		t.Fatal(out)
	}
}

func TestRunNeedsInstancesUnlessTheWebUICanAddThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n[web]\nenabled = false\n", dir), 0o600)
	if code, out, _ := run("--config", path, "run"); code != 2 || !strings.Contains(out, "no apps configured") {
		t.Fatal(out)
	}
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n", dir), 0o600)
	if code, _, _ := run("--config", path, "run", "--once"); code != 2 {
		t.Fatal("run --once needs instances")
	}
	if code, _, _ := run("--config", path, "validate"); code != 2 {
		t.Fatal("validate needs instances")
	}
}

func TestStateDirMustBeCreatable(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	path := filepath.Join(dir, "c.toml")
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n[[radarr]]\nurl = \"http://r:1\"\napi_key_env = \"K\"\n", filepath.Join(blocker, "state")), 0o600)
	if code, _, _ := run("--config", path, "run", "--once"); code != 1 {
		t.Fatal(code)
	}
	if code, _, _ := run("--config", path, "reset-password"); code != 1 {
		t.Fatal(code)
	}
}

func TestLogLevelFromSettings(t *testing.T) {
	path, _ := writeConfig(t)
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, append([]byte("log_level = \"WARNING\"\n"), raw...), 0o600)
	_, out, _ := run("--config", path, "run", "--once")
	if strings.Contains(out, "level=INFO") || !strings.Contains(out, "level=WARN") {
		t.Fatal(out)
	}
}

// serve runs `run` until the process gets SIGTERM, like `docker stop`.
func serve(t *testing.T, toml string, ready func(stateDir string) bool) (int, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n%s", dir, toml), 0o600)
	var out tu.Logs
	done := make(chan int, 1)
	go func() { done <- Main([]string{"--config", path, "run"}, &out, &out) }()
	deadline := time.Now().Add(10 * time.Second)
	for !ready(dir) {
		if time.Now().After(deadline) {
			t.Fatalf("never became ready:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		return code, out.String()
	case <-time.After(15 * time.Second):
		t.Fatal("did not stop on SIGTERM")
	}
	return 0, ""
}

func TestRunServesTheWebUIUntilSIGTERM(t *testing.T) {
	port := tu.FreePort(t)
	healthz := func(string) bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body) == "ok"
	}
	code, out := serve(t, fmt.Sprintf("[web]\nlisten = \"127.0.0.1\"\nport = %d\n", port), healthz)
	if code != 0 || !strings.Contains(out, "Setup code: ") || !strings.Contains(out, `msg="web UI listening" addr=127.0.0.1:`) {
		t.Fatal(code, out)
	}
}

func TestRunWithoutTheWebUI(t *testing.T) {
	path, _ := writeConfig(t)
	raw, _ := os.ReadFile(path)
	beat := func(stateDir string) bool { _, err := os.Stat(filepath.Join(stateDir, "heartbeat")); return err == nil }
	code, out := serve(t, strings.SplitN(string(raw), "\n", 2)[1], beat)
	if code != 0 || strings.Contains(out, "Setup code") {
		t.Fatal(code, out)
	}
}

func TestRunReportsAPortInUse(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, fmt.Appendf(nil, "state_dir = %q\n[web]\nlisten = \"127.0.0.1\"\nport = %d\n", dir, l.Addr().(*net.TCPAddr).Port), 0o600)
	if code, out, _ := run("--config", path, "run"); code != 1 || !strings.Contains(out, "web UI stopped") {
		t.Fatal(code, out)
	}
}
