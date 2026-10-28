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
	"syscall"

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

// parseFlags parses args; usage and errors go to out.
func parseFlags(args []string, out io.Writer) (options, error) {
	defPath, _ := config.DefaultPath()
	var o options
	var level string
	fs := flag.NewFlagSet("throtd", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { fmt.Fprint(out, usage); fs.PrintDefaults() }
	fs.StringVar(&o.configPath, "config", defPath, "path to config file")
	fs.BoolVar(&o.version, "version", false, "print version and exit")
	fs.StringVar(&o.logFormat, "log-format", "text", "log format: text or json")
	fs.StringVar(&level, "log-level", "info", "log level: debug, info, warn, or error")
	if err := fs.Parse(args); err != nil {
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
	o, err := parseFlags(args, os.Stderr)
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
	log := newLogger(os.Stderr, o)
	procs, ctl := platform.NewProcessTools()
	d := app.NewDaemon(cfg, platform.NewHostSampler(), procs, ctl, log)
	d.SetConfigPath(o.configPath)

	// SIGHUP re-reads the config; the outcome is an event either way.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				d.Reload()
			}
		}
	}()
	return d.Run(ctx)
}
