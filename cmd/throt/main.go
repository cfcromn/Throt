// Command throt is the Throt client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

const usage = `Usage: throt [flags] <command>

Commands:
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
