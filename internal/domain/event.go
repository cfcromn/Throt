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
)

// Event is the single structured record that feeds logs, diagnostics, and
// the TUI timeline.
type Event struct {
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
}

// NewEventLog returns a log retaining at most capacity events (minimum 1).
func NewEventLog(capacity int) *EventLog {
	return &EventLog{buf: make([]Event, max(capacity, 1))}
}

// Append records e, evicting the oldest event when full.
func (l *EventLog) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
