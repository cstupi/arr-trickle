// Package cli is the command line: run (default), check, validate, healthcheck, reset-password.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cstupi/arr-trickle/internal/config"
	"github.com/cstupi/arr-trickle/internal/runner"
	"github.com/cstupi/arr-trickle/internal/service"
	"github.com/cstupi/arr-trickle/internal/state"
	"github.com/cstupi/arr-trickle/internal/version"
	"github.com/cstupi/arr-trickle/internal/web"
)

const usage = `usage: arr-trickle [--config FILE] [--version] [command]

Work through the Sonarr/Radarr/Lidarr missing and upgrade backlog a few searches at a time.

commands:
  run [--once]     search on a schedule and serve the web UI (default); --once does a single pass and exits
  check            read-only connectivity and API check; sends no searches
  validate         validate the settings and exit
  healthcheck      exit 0 if the scheduler is alive and passes are running (for Docker HEALTHCHECK)
  reset-password   remove the web UI admin account and print a new setup code

options:
  --config FILE    TOML settings file (default: $ARR_TRICKLE_CONFIG or /config/config.toml)
  --version        print the version
`

// Level is the log level, changed on hot reload.
var Level = new(slog.LevelVar)

// lineHandler writes "2006-01-02 15:04:05 LEVEL message", like the 0.2 log format.
type lineHandler struct {
	mu *sync.Mutex
	w  io.Writer
}

func (h lineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= Level.Level() }
func (h lineHandler) WithAttrs([]slog.Attr) slog.Handler           { return h }
func (h lineHandler) WithGroup(string) slog.Handler                { return h }
func (h lineHandler) Handle(_ context.Context, r slog.Record) error {
	name := map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING", slog.LevelError: "ERROR"}[r.Level]
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintf(h.w, "%s %s %s\n", r.Time.Format("2006-01-02 15:04:05"), name, r.Message)
	return err
}

// SetLevel applies a configured log level name.
func SetLevel(name string) {
	Level.Set(map[string]slog.Level{"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARNING": slog.LevelWarn, "ERROR": slog.LevelError}[name])
}

// SetupLogging sends logs to w.
func SetupLogging(w io.Writer, level string) {
	SetLevel(level)
	slog.SetDefault(slog.New(lineHandler{&sync.Mutex{}, w}))
}

// Main runs the command line and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("arr-trickle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	configFlag := fs.String("config", "", "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "arr-trickle "+version.Version)
		return 0
	}
	command, rest := "run", fs.Args()
	if len(rest) > 0 {
		command, rest = rest[0], rest[1:]
	}
	once := false
	switch command {
	case "run":
		sub := flag.NewFlagSet("run", flag.ContinueOnError)
		sub.SetOutput(stderr)
		sub.BoolVar(&once, "once", false, "do a single pass and exit")
		if sub.Parse(rest) != nil {
			return 2
		}
	case "check", "validate", "healthcheck", "reset-password":
	default:
		fmt.Fprintf(stderr, "arr-trickle: unknown command %q\n\n%s", command, usage)
		return 2
	}

	// The web UI can start with no instances (they are added from Settings); validate/check cannot.
	allowEmpty := command == "run" || command == "healthcheck" || command == "reset-password"
	settings, err := config.Load(*configFlag, nil, allowEmpty)
	if err != nil {
		fmt.Fprintf(stderr, "configuration error: %s\n", err)
		return 2
	}
	SetupLogging(stdout, settings.LogLevel)
	switch command {
	case "validate":
		return cmdValidate(settings, stdout, stderr)
	case "check":
		return cmdCheck(settings, stdout)
	case "healthcheck":
		return cmdHealthcheck(settings, stdout)
	case "reset-password":
		return cmdResetPassword(settings, stdout, stderr)
	}
	return cmdRun(settings, once, config.Path(*configFlag, nil), stdout)
}

func cmdValidate(s config.Settings, stdout, stderr io.Writer) int {
	names := make([]string, 0, len(s.Instances))
	for _, i := range s.Instances {
		if _, err := i.APIKey(nil); err != nil {
			fmt.Fprintf(stderr, "configuration error: %s\n", err)
			return 2
		}
		names = append(names, i.Name)
	}
	fmt.Fprintf(stdout, "ok: %d instance(s): %s\n", len(s.Instances), strings.Join(names, ", "))
	return 0
}

func cmdCheck(s config.Settings, stdout io.Writer) int {
	code := 0
	for _, inst := range s.Instances {
		res := runner.CheckInstance(inst, nil)
		fmt.Fprintf(stdout, "[%s] %s\n", res.Name, inst.URL)
		for _, line := range res.Lines {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
		if !res.OK {
			code = 1
		}
	}
	return code
}

func cmdHealthcheck(s config.Settings, stdout io.Writer) int {
	age, ok := state.HeartbeatAge(s.StateDir + "/heartbeat")
	limit := time.Duration(s.IntervalMinutes)*2*time.Minute + 5*time.Minute
	if !ok || age > limit {
		last := "never"
		if ok {
			last = fmt.Sprintf("%ds ago", int(age.Seconds()))
		}
		fmt.Fprintf(stdout, "unhealthy: last run %s\n", last)
		return 1
	}
	fmt.Fprintf(stdout, "healthy: last run %ds ago\n", int(age.Seconds()))
	return 0
}

func cmdResetPassword(s config.Settings, stdout, stderr io.Writer) int {
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "cannot create %s: %s\n", s.StateDir, err)
		return 1
	}
	code, err := web.NewAuthStore(s.StateDir).Reset()
	if err != nil {
		fmt.Fprintf(stderr, "could not reset the password: %s\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "The admin account was removed and every web session ended.")
	fmt.Fprintf(stdout, "Open /setup in the web UI and create the account again with this setup code: %s\n", code)
	return 0
}

func cmdRun(s config.Settings, once bool, path string, stdout io.Writer) int {
	if len(s.Instances) == 0 && (once || !s.Web.Enabled) {
		fmt.Fprintln(stdout, "configuration error: no apps configured; add at least one [[sonarr]], [[radarr]] or [[lidarr]]")
		return 2
	}
	names := make([]string, 0, len(s.Instances))
	for _, i := range s.Instances {
		names = append(names, fmt.Sprintf("%s(%s)", i.Name, i.Kind))
	}
	shown := strings.Join(names, ", ")
	if shown == "" {
		shown = "no instances yet"
	}
	dry := ""
	if s.DryRun {
		dry = " [dry run]"
	}
	slog.Info(fmt.Sprintf("arr-trickle %s: %s every %d min%s", version.Version, shown, s.IntervalMinutes, dry))
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		slog.Error(fmt.Sprintf("cannot create the state directory %s: %s", s.StateDir, err))
		return 1
	}
	sched := service.New(s, nil, func(n config.Settings) { SetLevel(n.LogLevel) })
	if once {
		for _, r := range sched.RunPass() {
			if !r.OK() {
				return 1
			}
		}
		return 0
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	if !s.Web.Enabled {
		sched.Start()
		<-stop
		sched.Stop(10 * time.Second)
		return 0
	}
	if err := web.Announce(s); err != nil {
		slog.Error(fmt.Sprintf("cannot write the setup code: %s", err))
		return 1
	}
	srv := web.New(s, sched, path).HTTPServer(s.Web.ListenAddr())
	sched.Start()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("web UI listening on " + s.Web.ListenAddr())
	code := 0
	select {
	case <-stop:
	case err := <-errc:
		slog.Error(fmt.Sprintf("web UI stopped: %s", err))
		code = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn(fmt.Sprintf("web UI shutdown: %s", err))
	}
	sched.Stop(10 * time.Second)
	return code
}
