// Command throt is the Throt client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/doctor"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/platform"
	"github.com/cfcromn/Throt/internal/transport"
	"github.com/cfcromn/Throt/internal/tui"
)

const usage = `Usage: throt [flags] [command]

Commands:
  top        open the terminal UI (the default with no command)
  status     show host resources and pressure
  ping       check that throtd is reachable
  events     print recent daemon events as JSON lines
  doctor     print a sanitized diagnostic report (safe to share)
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
	if fs.NArg() > 1 {
		fs.Usage()
		return fmt.Errorf("expected one command")
	}
	cmd := "top"
	if fs.NArg() == 1 {
		cmd = fs.Arg(0)
	}
	if cmd == "version" {
		fmt.Println(app.Version)
		return nil
	}

	if cmd == "doctor" {
		return runDoctor(*cfgPath)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cmd == "top" {
		// Quitting the TUI only ends this client; throtd keeps running.
		_, err := tea.NewProgram(tui.New(daemonClient{socket: cfg.SocketPath}, cfg.SocketPath)).Run()
		return err
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

// daemonClient implements tui.Client over the local protocol.
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
	fmt.Fprintln(tw, "\nHEAVY WORKLOADS\nOWNER\tPID\tPROCESS\tCLASS\tCPU\tMEM")
	if len(st.Heavy) == 0 {
		fmt.Fprintln(tw, "(none)")
	}
	for _, h := range st.Heavy {
		owner := h.Owner
		if owner == "" {
			owner = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.0f%%\t%s\n", owner, h.Key.PID, h.Name, h.Class, h.CPU*100, gib(h.RSS))
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
