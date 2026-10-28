// Command throt is the Throt client.
package main

import (
	"context"
	"encoding/json"
	"errors"
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
  stats      show per-agent resource usage since throtd started; --json for JSON
  ping       check that throtd is reachable
  events     print recent daemon events as JSON lines; -f keeps printing new ones
  explain PID
             explain why a heavy workload is running, paused, or queued
  pause PID  pause a heavy workload (SIGSTOP) shown in 'status'
  resume [--force] PID
             resume a paused workload (SIGCONT). Also resumes a stopped
             process throtd did not pause (listed by 'doctor', e.g. one an
             earlier throtd left paused) when it is a heavy workload of an
             agent; --force resumes any other such process of yours too,
             such as a shell job suspended with Ctrl-Z (prefer fg there)
  run [--timeout D] [--no-wait] -- CMD [ARGS...]
             wait for heavy-job capacity, then exec CMD (advisory; runs
             immediately if throtd is down)
  reload     make throtd re-read its config file
  doctor     print a sanitized diagnostic report (safe to share)
  mcp        run a read-only MCP server on stdio for coding agents
  completion bash|zsh|fish
             print a shell completion script
  version    print the client version

Flags:
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "throt:", err)
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
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
	// 'run' takes its own flags and passes everything else to the command.
	if cmd == "run" {
		return runCommand(fs.Args()[1:], *cfgPath)
	}
	// 'events' takes its own flags after the command: throt events -f.
	followEvents, statsJSON, resumeForce := false, false, false
	pidArg := fs.Arg(1)
	switch {
	case cmd == "resume":
		var err error
		if pidArg, resumeForce, err = parseResumeArgs(fs.Args()[1:]); err != nil {
			return err
		}
	case cmd == "stats":
		sfs := flag.NewFlagSet("throt stats", flag.ContinueOnError)
		sfs.BoolVar(&statsJSON, "json", false, "print the stats as JSON")
		if err := sfs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
		if sfs.NArg() > 0 {
			return fmt.Errorf("usage: throt stats [--json]")
		}
	case cmd == "events":
		efs := flag.NewFlagSet("throt events", flag.ContinueOnError)
		efs.BoolVar(&followEvents, "f", false, "keep printing new events until interrupted")
		efs.IntVar(limit, "n", *limit, "number of recent events to print first")
		if err := efs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
		if efs.NArg() > 0 {
			return fmt.Errorf("usage: throt events [-f] [-n N]")
		}
	case fs.NArg() > 2:
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
	if cmd == "completion" {
		return printCompletion(os.Stdout, fs.Arg(1))
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
	case "stats":
		var r app.StatsResult
		if err := transport.Call(ctx, cfg.SocketPath, "stats", nil, &r); err != nil {
			return err
		}
		if statsJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(r)
		}
		printStats(os.Stdout, r, time.Now())
	case "pause", "resume":
		pid, err := strconv.Atoi(pidArg)
		if err != nil {
			return fmt.Errorf("usage: throt %s PID", cmd)
		}
		if err := transport.Call(ctx, cfg.SocketPath, cmd, app.ControlParams{PID: pid, Force: resumeForce}, nil); err != nil {
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

// parseResumeArgs accepts "PID" with an optional --force before or after it.
func parseResumeArgs(args []string) (pid string, force bool, err error) {
	for _, a := range args {
		switch {
		case a == "--force" || a == "-force":
			force = true
		case pid == "" && !strings.HasPrefix(a, "-"):
			pid = a
		default:
			return "", false, fmt.Errorf("usage: throt resume [--force] PID")
		}
	}
	if pid == "" {
		return "", false, fmt.Errorf("usage: throt resume [--force] PID")
	}
	return pid, force, nil
}

// daemonClient implements tui.Client and mcp.Client over the local protocol.
type daemonClient struct{ socket string }

func (c daemonClient) Status(ctx context.Context) (app.StatusResult, error) {
	var st app.StatusResult
	err := transport.Call(ctx, c.socket, "status", nil, &st)
	return st, err
}

// Acquire asks for an admission lease for the process p names.
func (c daemonClient) Acquire(ctx context.Context, p app.AcquireParams) (app.AcquireResult, error) {
	var r app.AcquireResult
	err := transport.Call(ctx, c.socket, "acquire", p, &r)
	return r, err
}

func (c daemonClient) Events(ctx context.Context, limit int) ([]domain.Event, error) {
	var events []domain.Event
	err := transport.Call(ctx, c.socket, "events", app.EventsParams{Limit: limit}, &events)
	return events, err
}

func (c daemonClient) Stats(ctx context.Context) (app.StatsResult, error) {
	var r app.StatsResult
	err := transport.Call(ctx, c.socket, "stats", nil, &r)
	return r, err
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
	if n := st.OrphanCount; n > 0 {
		noun := "processes"
		if n == 1 {
			noun = "process"
		}
		fmt.Fprintf(w, "STOPPED   %d %s not paused by throtd (see throt doctor)\n", n, noun)
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
	mem := na(domain.MetricMemory, fmt.Sprintf("%s used of %s", gib(h.MemTotal-min(h.MemAvailable, h.MemTotal)), gib(h.MemTotal)))
	if h.HasMemoryPressure() {
		mem += fmt.Sprintf(" (kernel: %s)", h.MemoryPressure)
	}
	fmt.Fprintf(w, "MEM       %s\n", mem)
	fmt.Fprintf(w, "LATENCY   %s\n", na(domain.MetricLatency, h.Latency.Round(time.Millisecond).String()))
	fmt.Fprintf(w, "SWAP      %s\n", na(domain.MetricSwap, fmt.Sprintf("%s used of %s", gib(h.SwapUsed), gib(h.SwapTotal))))
	if psi := psiDetail(*h); psi != "" {
		fmt.Fprintf(w, "PSI       %s\n", psi)
	}

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
	fmt.Fprintf(w, "SCHEDULER %s (heavy limit %d, %d queued, %d reserved)\n", st.Scheduler.Mode, st.Scheduler.HeavyLimit, len(st.Scheduler.Queued), len(st.Scheduler.Leases))
	const maxLeases = 5
	for i, l := range st.Scheduler.Leases {
		if i == maxLeases {
			fmt.Fprintf(w, "RESERVED  ... and %d more\n", len(st.Scheduler.Leases)-maxLeases)
			break
		}
		agent := l.Agent
		if agent == "" {
			agent = "-"
		}
		fmt.Fprintf(w, "RESERVED  pid %d (%s) since %s\n", l.Process.PID, agent, l.Granted.Format(time.TimeOnly))
	}
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

// psiDetail renders the Linux PSI readings the sample carries, as
// "mem 3% (full 1%)  io 0% (full 0%)  cpu 12%", or "" when it has none.
// The pressure policy uses some and full for memory, full for I/O and some
// for CPU.
func psiDetail(h domain.HostSample) string {
	var parts []string
	pct := func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
	if st, ok := h.StallFor(domain.MetricMemoryStall); ok {
		parts = append(parts, "mem "+pct(st.Some)+" (full "+pct(st.Full)+")")
	}
	if st, ok := h.StallFor(domain.MetricIOStall); ok {
		parts = append(parts, "io "+pct(st.Some)+" (full "+pct(st.Full)+")")
	}
	if st, ok := h.StallFor(domain.MetricCPUStall); ok {
		parts = append(parts, "cpu "+pct(st.Some))
	}
	return strings.Join(parts, "  ")
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
		Adapters:  app.Adapters(cfg.Agents),
		UID:       uint32(os.Getuid()),
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
