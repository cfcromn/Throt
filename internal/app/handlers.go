package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// PingResult is returned by the "ping" method.
type PingResult struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	Uptime   string `json:"uptime"`
}

func (d *Daemon) ping(context.Context, json.RawMessage) (any, error) {
	return PingResult{
		Version:  Version,
		Protocol: transport.ProtocolVersion,
		PID:      os.Getpid(),
		Uptime:   time.Since(d.started).Round(time.Second).String(),
	}, nil
}

// MaxEventsPerCall caps one "events" response so it fits the protocol's
// message size limit.
const MaxEventsPerCall = 1000

// EventsParams selects which events the "events" method returns.
type EventsParams struct {
	Limit int `json:"limit"`
	// Since, when > 0, returns only events with a greater seq, oldest first,
	// up to Limit; poll again with the last seq to get the rest. A Since
	// beyond the newest event means the daemon restarted, so the newest
	// events are returned instead and the client sees seq go down.
	Since uint64 `json:"since,omitempty"`
}

func (d *Daemon) recentEvents(_ context.Context, raw json.RawMessage) (any, error) {
	var p EventsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if p.Limit <= 0 || p.Limit > MaxEventsPerCall {
		p.Limit = MaxEventsPerCall
	}
	if p.Since == 0 {
		return d.events.Recent(p.Limit), nil
	}
	if evs := d.events.Since(p.Since, p.Limit); len(evs) > 0 {
		return evs, nil
	}
	if newest := d.events.Recent(1); len(newest) == 1 && newest[0].Seq < p.Since {
		return d.events.Recent(p.Limit), nil
	}
	return []domain.Event{}, nil
}

func (d *Daemon) currentStatus(context.Context, json.RawMessage) (any, error) {
	d.mu.Lock()
	st := d.status
	st.Scheduler.Leases = d.leases.views()
	d.mu.Unlock()
	st.Paused = d.ctl.Paused()
	return st, nil
}

func (d *Daemon) currentStats(context.Context, json.RawMessage) (any, error) {
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	return d.acct.Report(), nil
}
