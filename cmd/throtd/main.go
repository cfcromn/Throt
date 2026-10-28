// Command throtd is the Throt daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/platform"
)

const usage = `Usage: throtd [flags]

throtd samples host and process metrics, attributes heavy workloads to
coding agents, and serves throt clients on a local Unix socket. It logs to
stderr; the event log ('throt events') is the record of what it did.

Log levels: info logs daemon lifecycle, agents appearing and leaving,
pressure changes, scheduler decisions and control actions; debug adds every
heavy workload start and exit and every admission lease; warn and error
keep only failures and processes left stopped.

Flags:
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		// -h printed the usage to stdout; asking for help is not a failure.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "throtd:", err)
		os.Exit(1)
	}
}

// options are throtd's command-line settings.
type options struct {
	configPath string
	version    bool
	logFormat  string
	logLevel   slog.Level
}

// parseFlags parses args. -h prints the usage to stdout and returns
// flag.ErrHelp, which main treats as success; errors, followed by the
// usage, go to stderr.
func parseFlags(args []string, stdout, stderr io.Writer) (options, error) {
	defPath, _ := config.DefaultPath()
	var o options
	var level string
	fs := flag.NewFlagSet("throtd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	usageTo := func(w io.Writer) {
		fs.SetOutput(w)
		fmt.Fprint(w, usage)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
	}
	// Parse reports its own error first; the usage follows, below.
	fs.Usage = func() {}
	fs.StringVar(&o.configPath, "config", defPath, "path to config file")
	fs.BoolVar(&o.version, "version", false, "print version and exit")
	fs.StringVar(&o.logFormat, "log-format", "text", "log format: text or json")
	fs.StringVar(&level, "log-level", "info", "log level: debug, info, warn, or error")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		usageTo(stdout)
		return o, err
	} else if err != nil {
		usageTo(stderr)
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	switch o.logFormat {
	case "text", "json":
	default:
		return o, fmt.Errorf("-log-format must be text or json, not %q", o.logFormat)
	}
	switch level {
	case "debug":
		o.logLevel = slog.LevelDebug
	case "info":
		o.logLevel = slog.LevelInfo
	case "warn":
		o.logLevel = slog.LevelWarn
	case "error":
		o.logLevel = slog.LevelError
	default:
		return o, fmt.Errorf("-log-level must be debug, info, warn, or error, not %q", level)
	}
	return o, nil
}

// newLogger returns the logger o asks for, writing to w.
func newLogger(w io.Writer, o options) *slog.Logger {
	opts := &slog.HandlerOptions{Level: o.logLevel}
	if o.logFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func run(args []string) error {
	o, err := parseFlags(args, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if o.version {
		fmt.Println(app.Version)
		return nil
	}

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// THROT_DEBUG_GOROUTINES (hidden, for soak tests) logs the goroutine
	// count at debug level, so it also forces the debug level.
	every := goroutineLogInterval(os.Getenv("THROT_DEBUG_GOROUTINES"))
	if every > 0 {
		o.logLevel = slog.LevelDebug
	}
	log := newLogger(os.Stderr, o)
	if every > 0 {
		go logGoroutines(ctx, log, every)
	}
	procs, ctl := platform.NewProcessTools()
	d := app.NewDaemon(cfg, platform.NewHostSampler(), procs, ctl, log)
	d.SetConfigPath(o.configPath)

	// SIGHUP re-reads the config; the outcome is an event either way.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go reloadOnHangup(ctx, hup, func() { d.Reload() })
	return d.Run(ctx)
}

// reloadOnHangup calls reload for each signal on hup until ctx is
// cancelled.
func reloadOnHangup(ctx context.Context, hup <-chan os.Signal, reload func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			reload()
		}
	}
}

// goroutineLogInterval parses THROT_DEBUG_GOROUTINES, a hidden debugging
// aid the soak test (internal/e2e) uses to watch for goroutine leaks: a Go
// duration such as "30s", or any other non-empty value for one minute.
// Empty or non-positive means off. It is deliberately documented only here:
// it is not a supported interface.
func goroutineLogInterval(v string) time.Duration {
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Minute
	}
	if d <= 0 {
		return 0
	}
	return max(d, time.Second) // a tiny interval would flood the log
}

// logGoroutines logs runtime.NumGoroutine() at debug level every interval
// until ctx is cancelled.
func logGoroutines(ctx context.Context, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		log.Debug("goroutines", "n", runtime.NumGoroutine())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
