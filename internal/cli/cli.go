// Package cli is the command line: run (default), check, validate, healthcheck, reset-password.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

var level = new(slog.LevelVar)

func setLevel(name string) {
	level.Set(map[string]slog.Level{"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARNING": slog.LevelWarn, "ERROR": slog.LevelError}[name])
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
	setLevel(settings.LogLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: level})))
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
	age, ok := state.HeartbeatAge(filepath.Join(s.StateDir, "heartbeat"))
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
	slog.Info("arr-trickle "+version.Version, "instances", strings.Join(names, ", "), "interval_minutes", s.IntervalMinutes)
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		slog.Error("cannot create the state directory", "path", s.StateDir, "err", err)
		return 1
	}
	sched := service.New(s, nil, func(n config.Settings) { setLevel(n.LogLevel) })
	if once {
		for _, r := range sched.RunPass() {
			if !r.OK() {
				return 1
			}
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer sched.Stop(10 * time.Second)
	if !s.Web.Enabled {
		sched.Start()
		<-ctx.Done()
		return 0
	}
	if err := web.Announce(s, stdout); err != nil {
		slog.Error("cannot write the setup code", "err", err)
		return 1
	}
	srv := web.New(s, sched, path).HTTPServer(s.Web.ListenAddr())
	sched.Start()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("web UI listening", "addr", s.Web.ListenAddr())
	select {
	case <-ctx.Done():
	case err := <-errc:
		slog.Error("web UI stopped", "err", err)
		return 1
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		slog.Warn("web UI shutdown", "err", err)
	}
	return 0
}
