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
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

const usage = `Usage: throt [flags] <command>

Commands:
  status     show host resources and pressure
  ping       check that throtd is reachable
  events     print recent daemon events as JSON lines
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
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("expected one command")
	}
	cmd := fs.Arg(0)
	if cmd == "version" {
		fmt.Println(app.Version)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
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
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
