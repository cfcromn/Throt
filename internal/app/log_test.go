package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
)

func TestEventLevel(t *testing.T) {
	for kind, want := range map[domain.EventKind]slog.Level{
		domain.EventProcessDiscovered: slog.LevelDebug,
		domain.EventProcessLost:       slog.LevelDebug,
		domain.EventLeaseGranted:      slog.LevelDebug,
		domain.EventLeaseReleased:     slog.LevelDebug,
		domain.EventDaemonStarted:     slog.LevelInfo,
		domain.EventAgentDiscovered:   slog.LevelInfo,
		domain.EventPressureChanged:   slog.LevelInfo,
		domain.EventSchedulerDecision: slog.LevelInfo,
		domain.EventControlPaused:     slog.LevelInfo,
		domain.EventControlFailed:     slog.LevelWarn,
		domain.EventDegraded:          slog.LevelWarn,
		domain.EventOrphanStopped:     slog.LevelWarn,
	} {
		if got := eventLevel(kind); got != want {
			t.Errorf("eventLevel(%s) = %v, want %v", kind, got, want)
		}
	}
}

func TestEmitLogsAtEventLevel(t *testing.T) {
	var b bytes.Buffer
	log := slog.New(slog.NewTextHandler(&b, nil))
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, log)
	d.emit(domain.EventProcessDiscovered, "process_discovered go", map[string]string{"pid": "1"})
	if b.Len() != 0 {
		t.Errorf("debug event logged at info: %s", b.String())
	}
	d.emit(domain.EventAgentDiscovered, "agent_discovered claude-1", map[string]string{"pid": "7", "agent": "claude-1"})
	if got := b.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "agent=claude-1 pid=7") {
		t.Errorf("log = %q, want an info line with sorted attrs", got)
	}
	// Every event still reaches the event log.
	if n := len(d.events.Recent(10)); n != 2 {
		t.Errorf("event log has %d events, want 2", n)
	}
}
