package app

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

func (d *Daemon) emit(kind domain.EventKind, msg string, attrs map[string]string) {
	d.events.Append(domain.Event{Time: time.Now(), Kind: kind, Message: msg, Attrs: attrs})
	if !d.log.Enabled(context.Background(), eventLevel(kind)) {
		return
	}
	args := make([]any, 0, 2+2*len(attrs))
	args = append(args, "event", kind)
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		args = append(args, k, attrs[k])
	}
	d.log.Log(context.Background(), eventLevel(kind), msg, args...)
}

// eventLevel is the log level of an event. Every event is in the event log;
// info stays quiet in steady state, so heavy workloads starting and exiting
// and admission leases, which come and go with every build, are debug.
func eventLevel(kind domain.EventKind) slog.Level {
	switch kind {
	case domain.EventProcessDiscovered, domain.EventProcessLost,
		domain.EventLeaseGranted, domain.EventLeaseReleased:
		return slog.LevelDebug
	case domain.EventDegraded, domain.EventControlFailed, domain.EventControlRefused,
		domain.EventConfigReloadFailed, domain.EventOrphanStopped:
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func (d *Daemon) emitChange(c process.Change) {
	attrs := map[string]string{"pid": strconv.Itoa(c.Process.PID), "name": c.Name}
	msg := string(c.Kind) + " " + c.Name
	if c.Agent != "" {
		attrs["agent"] = c.Agent
		msg = string(c.Kind) + " " + c.Agent
	}
	d.emit(c.Kind, msg, attrs)
}
