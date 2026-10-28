// Package domain holds Throt's core types. It has no OS, UI, or transport
// dependencies so policy code built on it stays deterministic and testable.
package domain

import (
	"sync"
	"time"
)

// EventKind identifies what happened. Values are stable strings because they
// appear in logs, diagnostics, and the local protocol.
type EventKind string

const (
	EventDaemonStarted EventKind = "daemon_started"
	EventDaemonStopped EventKind = "daemon_stopped"
	EventDegraded      EventKind = "degraded"
	EventRecovered     EventKind = "recovered"
	// EventConfigReloaded records an applied config reload and the keys it
	// changed; EventConfigReloadFailed records one that changed nothing.
	EventConfigReloaded     EventKind = "config_reloaded"
	EventConfigReloadFailed EventKind = "config_reload_failed"
)

// Event is the single structured record that feeds logs, diagnostics, and
// the TUI timeline.
type Event struct {
	// Seq is assigned by EventLog.Append: 1 for a daemon's first event, then
	// one more for each event. It starts over when the daemon restarts.
	Seq     uint64            `json:"seq"`
	Time    time.Time         `json:"time"`
	Kind    EventKind         `json:"kind"`
	Message string            `json:"message,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// EventLog is a fixed-capacity ring of recent events, safe for concurrent use.
// Old events are dropped so history stays bounded.
type EventLog struct {
	mu    sync.Mutex
	buf   []Event
	next  int
	count int
	seq   uint64 // Seq of the newest event
}

// NewEventLog returns a log retaining at most capacity events (minimum 1).
func NewEventLog(capacity int) *EventLog {
	return &EventLog{buf: make([]Event, max(capacity, 1))}
}

// Append records e under the next sequence number, replacing any Seq it
// carries, and evicts the oldest event when full.
func (l *EventLog) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	l.count = min(l.count+1, len(l.buf))
}

// Recent returns up to n of the newest events, oldest first. n <= 0 means all.
func (l *EventLog) Recent(n int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > l.count {
		n = l.count
	}
	out := make([]Event, n)
	start := (l.next - n + len(l.buf)) % len(l.buf)
	for i := range n {
		out[i] = l.buf[(start+i)%len(l.buf)]
	}
	return out
}

// Since returns up to limit events with Seq > seq, oldest first, so a client
// that polls with the last Seq it saw never skips a retained event; limit <= 0
// means all. Evicted events are gone: a gap between seq and the first
// returned Seq is how many were missed.
func (l *EventLog) Since(seq uint64, limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Retained events have consecutive Seqs ending at l.seq.
	newer := 0
	if seq < l.seq {
		newer = int(min(l.seq-seq, uint64(l.count)))
	}
	n := newer
	if limit > 0 {
		n = min(n, limit)
	}
	out := make([]Event, n)
	start := (l.next - newer + len(l.buf)) % len(l.buf)
	for i := range n {
		out[i] = l.buf[(start+i)%len(l.buf)]
	}
	return out
}
