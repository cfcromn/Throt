package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

const (
	followInterval = time.Second
	// followTimeout bounds one poll so a stuck daemon cannot hang -f.
	followTimeout = 5 * time.Second
)

// fetchEvents returns up to limit events with seq > since, or the newest
// limit events when since is 0 (the "events" method).
type fetchEvents func(ctx context.Context, since uint64, limit int) ([]domain.Event, error)

// follow prints the newest limit events, then every new event, as JSON lines
// until ctx is cancelled. Notices (restarts, missed events, connection
// errors) go to errw so out stays machine-readable. Only a failure of the
// first call is returned; later ones are reported once and retried.
func follow(ctx context.Context, fetch fetchEvents, out, errw io.Writer, interval time.Duration, limit int) error {
	enc := json.NewEncoder(out)
	var last uint64
	var instance string
	first, failing := true, false
	for {
		n := limit
		if !first {
			n = app.MaxEventsPerCall
		}
		callCtx, cancel := context.WithTimeout(ctx, followTimeout)
		events, err := fetch(callCtx, last, n)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err != nil && first:
			return err
		case err != nil:
			if !failing {
				fmt.Fprintf(errw, "throt: %v; retrying every %s\n", err, interval)
				failing = true
			}
		default:
			if failing {
				fmt.Fprintln(errw, "throt: reconnected to throtd")
				failing = false
			}
			if len(events) > 0 {
				if events[0].Seq == 0 {
					return errors.New("throtd does not number its events; upgrade throtd to use -f")
				}
				if instance != "" && events[0].Instance != instance {
					// The new daemon may already be past last; start over from
					// its newest events instead of skipping its first ones.
					fmt.Fprintln(errw, "throt: throtd restarted; following the new daemon")
					instance, last, first = events[0].Instance, 0, true
					continue
				}
				instance = events[0].Instance
				switch {
				case last != 0 && events[0].Seq <= last:
					fmt.Fprintf(errw, "throt: event sequence went back from %d to %d: throtd restarted; following the new daemon\n", last, events[0].Seq)
				case last != 0 && events[0].Seq > last+1:
					fmt.Fprintf(errw, "throt: missed %d events dropped from throtd's history\n", events[0].Seq-last-1)
				}
				for _, e := range events {
					enc.Encode(e)
				}
				last = events[len(events)-1].Seq
			}
		}
		first = false
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
