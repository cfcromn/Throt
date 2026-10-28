package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdEvents is 'throt events [-f] [-n N]'.
func cmdEvents(ctx context.Context, e env, args []string) error {
	limit := e.limit
	followEvents, err := parseEventsArgs(args, &limit, e.stdout, e.stderr)
	if err != nil {
		return err
	}
	cfg := e.clientConfig()
	if followEvents {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		c := daemonClient{socket: cfg.SocketPath}
		return follow(ctx, c.EventsSince, e.stdout, e.stderr, followInterval, limit)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var events []domain.Event
	if err := transport.Call(ctx, cfg.SocketPath, "events", app.EventsParams{Limit: limit}, &events); err != nil {
		return err
	}
	enc := json.NewEncoder(e.stdout)
	for _, ev := range events {
		enc.Encode(ev)
	}
	return nil
}

// parseEventsArgs parses the arguments after 'events'; -n overrides limit.
func parseEventsArgs(args []string, limit *int, stdout, stderr io.Writer) (follow bool, err error) {
	efs := flag.NewFlagSet("throt events", flag.ContinueOnError)
	efs.SetOutput(stderr)
	efs.Usage = func() { fmt.Fprintln(efs.Output(), "usage: throt events [-f] [-n N]"); efs.PrintDefaults() }
	efs.BoolVar(&follow, "f", false, "keep printing new events until interrupted")
	efs.IntVar(limit, "n", *limit, "number of recent events to print first")
	if err := parseFlags(efs, args, stdout); err != nil {
		return false, err
	}
	if efs.NArg() > 0 {
		return false, usageLine("usage: throt events [-f] [-n N]")
	}
	return follow, nil
}
