package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// cmdEvents is 'throt events [-f] [-n N]'.
func cmdEvents(ctx context.Context, e env, args []string) error {
	limit := e.limit
	followEvents, asJSON, err := parseEventsArgs(args, &limit, e.stdout, e.stderr)
	if err != nil {
		return err
	}
	cfg := e.clientConfig()
	if followEvents {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		c := daemonClient{socket: cfg.SocketPath}
		return follow(ctx, c.EventsSince, eventPrinter(e.stdout, asJSON, time.Now()), e.stderr, followInterval, limit)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var events []domain.Event
	if err := transport.Call(ctx, cfg.SocketPath, "events", app.EventsParams{Limit: limit}, &events); err != nil {
		return err
	}
	print := eventPrinter(e.stdout, asJSON, time.Now())
	for _, ev := range events {
		print(ev)
	}
	return nil
}

// parseEventsArgs parses the arguments after 'events'; -n overrides limit.
func parseEventsArgs(args []string, limit *int, stdout, stderr io.Writer) (follow, asJSON bool, err error) {
	efs := flag.NewFlagSet("throt events", flag.ContinueOnError)
	efs.SetOutput(stderr)
	efs.Usage = func() { fmt.Fprintln(efs.Output(), "usage: throt events [-f] [-n N] [--json]"); efs.PrintDefaults() }
	efs.BoolVar(&follow, "f", false, "keep printing new events until interrupted")
	efs.IntVar(limit, "n", *limit, "number of recent events to print first")
	efs.BoolVar(&asJSON, "json", false, "print JSON lines even on a terminal (the default elsewhere)")
	if err := parseFlags(efs, args, stdout); err != nil {
		return false, false, err
	}
	if efs.NArg() > 0 {
		return false, false, usageLine("usage: throt events [-f] [-n N] [--json]")
	}
	return follow, asJSON, nil
}
