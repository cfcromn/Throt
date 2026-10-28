package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
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
	d.emit(domain.EventProcessDiscovered, "discovered heavy workload go (pid 1)", map[string]string{"pid": "1"})
	if b.Len() != 0 {
		t.Errorf("debug event logged at info: %s", b.String())
	}
	d.emit(domain.EventAgentDiscovered, "agent claude-1 started (pid 7)", map[string]string{"pid": "7", "agent": "claude-1"})
	if got := b.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "agent=claude-1 pid=7") {
		t.Errorf("log = %q, want an info line with sorted attrs", got)
	}
	// Every event still reaches the event log.
	if n := len(d.events.Recent(10)); n != 2 {
		t.Errorf("event log has %d events, want 2", n)
	}
}

// TestChangeMessages checks that process graph events read as sentences
// rather than repeating their kind, which the kind field already carries.
func TestChangeMessages(t *testing.T) {
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.DiscardHandler))
	k := domain.ProcessKey{PID: 123}
	for _, tc := range []struct {
		c    process.Change
		want string
	}{
		{process.Change{Kind: domain.EventProcessDiscovered, Process: k, Name: "make"}, "discovered heavy workload make (pid 123)"},
		{process.Change{Kind: domain.EventProcessLost, Process: k, Name: "make"}, "heavy workload make (pid 123) exited"},
		{process.Change{Kind: domain.EventAgentDiscovered, Process: k, Name: "claude", Agent: "claude-1"}, "agent claude-1 started (pid 123)"},
		{process.Change{Kind: domain.EventAgentLost, Process: k, Name: "claude", Agent: "claude-1"}, "agent claude-1 exited (pid 123)"},
	} {
		d.emitChange(tc.c)
		ev := d.events.Recent(1)[0]
		if ev.Kind != tc.c.Kind || ev.Message != tc.want {
			t.Errorf("%s: message %q, want %q", tc.c.Kind, ev.Message, tc.want)
		}
		if ev.Attrs["pid"] != "123" || ev.Attrs["name"] != tc.c.Name || ev.Attrs["agent"] != tc.c.Agent {
			t.Errorf("%s: attrs %v", tc.c.Kind, ev.Attrs)
		}
	}
}
