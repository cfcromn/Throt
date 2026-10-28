// Command throt is the Throt client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/doctor"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/mcp"
	"github.com/cfcromn/Throt/internal/platform"
	"github.com/cfcromn/Throt/internal/transport"
	"github.com/cfcromn/Throt/internal/tui"
)

const usage = `Usage: throt [flags] [command]

Commands:
  top        open the terminal UI (the default with no command)
  status     show host resources and pressure
  ping       check that throtd is reachable
  events     print recent daemon events as JSON lines; -f keeps printing new ones
  explain PID
             explain why a heavy workload is running, paused, or queued
  pause PID  pause a heavy workload (SIGSTOP) shown in 'status'
  resume PID resume a paused workload (SIGCONT)
  reload     make throtd re-read its config file
  doctor     print a sanitized diagnostic report (safe to share)
  mcp        run a read-only MCP server on stdio for coding agents
  version    print the client version

Flags:
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "throt:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("throt", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage); fs.PrintDefaults() }
	defPath, _ := config.DefaultPath()
	cfgPath := fs.String("config", defPath, "path to config file")
	limit := fs.Int("n", 50, "number of events for 'events'")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := "top"
	if fs.NArg() >= 1 {
		cmd = fs.Arg(0)
	}
	// 'events' takes its own flags after the command: throt events -f.
	followEvents := false
	if cmd == "events" {
		efs := flag.NewFlagSet("throt events", flag.ContinueOnError)
		efs.BoolVar(&followEvents, "f", false, "keep printing new events until interrupted")
		efs.IntVar(limit, "n", *limit, "number of recent events to print first")
		if err := efs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
		if efs.NArg() > 0 {
			return fmt.Errorf("usage: throt events [-f] [-n N]")
		}
	} else if fs.NArg() > 2 {
		fs.Usage()
		return fmt.Errorf("too many arguments")
	}
	if cmd == "version" {
		fmt.Println(app.Version)
		return nil
	}

	if cmd == "doctor" {
		return runDoctor(*cfgPath)
	}

	cfg, err := config.Load(*cfgPath)
	switch {
	case err != nil && cmd == "reload":
		// throtd reports the invalid file itself and keeps its config, so
		// reach it at the best socket path the file allows.
		if cfg.SocketPath == "" {
			cfg.SocketPath = config.DefaultSocketPath()
		}
	case err != nil:
		return err
	}
	if cmd == "top" {
		// Quitting the TUI only ends this client; throtd keeps running.
		_, err := tea.NewProgram(tui.New(daemonClient{socket: cfg.SocketPath}, cfg.SocketPath)).Run()
		return err
	}
	if cmd == "mcp" {
		// stdout carries MCP messages only; logs go to stderr.
		log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		return mcp.New(daemonClient{socket: cfg.SocketPath}, app.Version, log).Serve(context.Background(), os.Stdin, os.Stdout)
	}
	if cmd == "events" && followEvents {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		c := daemonClient{socket: cfg.SocketPath}
		return follow(ctx, c.EventsSince, os.Stdout, os.Stderr, followInterval, *limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch cmd {
	case "ping":
		var r app.PingResult
		if err := transport.Call(ctx, cfg.SocketPath, "ping", nil, &r); err != nil {
			return err
		}
		fmt.Printf("throtd %s (protocol v%d, pid %d, up %s)\n", r.Version, r.Protocol, r.PID, r.Uptime)
	case "status":
		var st app.StatusResult
		if err := transport.Call(ctx, cfg.SocketPath, "status", nil, &st); err != nil {
			return err
		}
		printStatus(os.Stdout, st)
	case "pause", "resume":
		pid, err := strconv.Atoi(fs.Arg(1))
		if err != nil {
			return fmt.Errorf("usage: throt %s PID", cmd)
		}
		if err := transport.Call(ctx, cfg.SocketPath, cmd, app.ControlParams{PID: pid}, nil); err != nil {
			return err
		}
		fmt.Printf("%sd pid %d\n", cmd, pid)
	case "reload":
		var r app.ReloadResult
		if err := transport.Call(ctx, cfg.SocketPath, "reload", nil, &r); err != nil {
			return err
		}
		printReload(os.Stdout, r)
	case "explain":
		pid, err := strconv.Atoi(fs.Arg(1))
		if err != nil {
			return fmt.Errorf("usage: throt explain PID")
		}
		var st app.StatusResult
		if err := transport.Call(ctx, cfg.SocketPath, "status", nil, &st); err != nil {
			return err
		}
		var events []domain.Event
		if err := transport.Call(ctx, cfg.SocketPath, "events", app.EventsParams{Limit: app.MaxEventsPerCall}, &events); err != nil {
			return err
		}
		return explain(os.Stdout, pid, st, events, time.Duration(cfg.Scheduler.ResumeCooldown))
	case "events":
		var events []domain.Event
		if err := transport.Call(ctx, cfg.SocketPath, "events", app.EventsParams{Limit: *limit}, &events); err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		for _, e := range events {
			enc.Encode(e)
		}
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

// daemonClient implements tui.Client and mcp.Client over the local protocol.
type daemonClient struct{ socket string }

func (c daemonClient) Status(ctx context.Context) (app.StatusResult, error) {
	var st app.StatusResult
	err := transport.Call(ctx, c.socket, "status", nil, &st)
	return st, err
}

func (c daemonClient) Events(ctx context.Context, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := transport.Call(ctx, c.socket, "events", app.EventsParams{Limit: limit}, &events)
	return events, err
}

func printReload(w io.Writer, r app.ReloadResult) {
	if len(r.Changed) == 0 {
		fmt.Fprintln(w, "reloaded: no changes applied")
	}
	for _, c := range r.Changed {
		fmt.Fprintln(w, "applied: "+c)
	}
	for _, c := range r.RequiresRestart {
		fmt.Fprintln(w, "requires restart: "+c)
	}
}

// EventsSince returns up to limit events newer than seq, or the newest limit
// events when seq is 0.
func (c daemonClient) EventsSince(ctx context.Context, seq uint64, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := transport.Call(ctx, c.socket, "events", app.EventsParams{Limit: limit, Since: seq}, &events)
	return events, err
}

func (c daemonClient) Pause(ctx context.Context, k domain.ProcessKey) error {
	return transport.Call(ctx, c.socket, "pause", app.ControlParams{PID: k.PID, Start: k.Start}, nil)
}

func (c daemonClient) Resume(ctx context.Context, k domain.ProcessKey) error {
	return transport.Call(ctx, c.socket, "resume", app.ControlParams{PID: k.PID, Start: k.Start}, nil)
}

func printStatus(w io.Writer, st app.StatusResult) {
	fmt.Fprintf(w, "PRESSURE  %s", strings.ToUpper(st.Pressure.String()))
	if st.Reason != "" {
		fmt.Fprintf(w, " (%s)", st.Reason)
	}
	fmt.Fprintln(w)
	if st.Degraded != "" {
		fmt.Fprintln(w, "DEGRADED  "+st.Degraded)
	}
	h := st.Host
	if h == nil {
		fmt.Fprintln(w, "no host sample yet")
		return
	}
	na := func(m domain.Metric, v string) string {
		if !h.Has(m) {
			return "n/a"
		}
		return v
	}
	fmt.Fprintf(w, "CPU       %s\n", na(domain.MetricCPU, fmt.Sprintf("%.0f%%", h.CPUUsage*100)))
	fmt.Fprintf(w, "LOAD      %s\n", na(domain.MetricLoad, fmt.Sprintf("%.2f per CPU", h.Load1)))
	fmt.Fprintf(w, "MEM       %s\n", na(domain.MetricMemory, fmt.Sprintf("%s used of %s", gib(h.MemTotal-min(h.MemAvailable, h.MemTotal)), gib(h.MemTotal))))
	fmt.Fprintf(w, "LATENCY   %s\n", na(domain.MetricLatency, h.Latency.Round(time.Millisecond).String()))
	fmt.Fprintf(w, "SWAP      %s\n", na(domain.MetricSwap, fmt.Sprintf("%s used of %s", gib(h.SwapUsed), gib(h.SwapTotal))))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nAGENTS\nAGENT\tPID\tCPU\tMEM\tCHILDREN")
	if len(st.Agents) == 0 {
		fmt.Fprintln(tw, "(none detected)")
	}
	const maxAgents = 15
	for i, a := range st.Agents {
		if i == maxAgents {
			fmt.Fprintf(tw, "... and %d more\n", len(st.Agents)-maxAgents)
			break
		}
		fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%s\t%d\n", a.Agent, a.Key.PID, a.CPU*100, gib(a.RSS), a.Children)
	}
	fmt.Fprintf(w, "SCHEDULER %s (heavy limit %d, %d queued)\n", st.Scheduler.Mode, st.Scheduler.HeavyLimit, len(st.Scheduler.Queued))
	paused := make(map[int]bool, len(st.Paused))
	for _, p := range st.Paused {
		paused[p.Key.PID] = true
	}
	queued := make(map[int]bool, len(st.Scheduler.Queued))
	for _, k := range st.Scheduler.Queued {
		queued[k.PID] = true
	}
	fmt.Fprintln(tw, "\nHEAVY WORKLOADS\nOWNER\tPID\tPROCESS\tCLASS\tCPU\tMEM\tSTATE")
	if len(st.Heavy) == 0 {
		fmt.Fprintln(tw, "(none)")
	}
	for _, h := range st.Heavy {
		owner := h.Owner
		if owner == "" {
			owner = "-"
		}
		state := "running"
		switch {
		case queued[h.Key.PID]:
			state = "QUEUED"
		case paused[h.Key.PID]:
			state = "PAUSED"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.0f%%\t%s\t%s\n", owner, h.Key.PID, h.Name, h.Class, h.CPU*100, gib(h.RSS), state)
	}
	tw.Flush()
}

func gib(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

// runDoctor reports even when the config is invalid or the daemon is down.
func runDoctor(cfgPath string) error {
	cfg, cfgErr := config.Load(cfgPath)
	if cfgErr != nil {
		cfg = config.Default()
	}
	_, statErr := os.Stat(cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := daemonClient{socket: cfg.SocketPath}
	r := doctor.Collect(ctx, doctor.Inputs{
		ConfigPath: cfgPath,
		SocketPath: cfg.SocketPath,
		ConfigErr:  cfgErr,
		ConfigFile: statErr == nil,
		Ping: func(ctx context.Context) (app.PingResult, error) {
			var p app.PingResult
			err := transport.Call(ctx, cfg.SocketPath, "ping", nil, &p)
			return p, err
		},
		Status:    c.Status,
		Host:      platform.NewHostSampler(),
		Processes: platform.NewProcessSampler(),
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
