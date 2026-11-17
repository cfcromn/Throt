package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestEventLine(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.Local)
	at := time.Date(2026, 11, 18, 8, 38, 0, 0, time.Local)
	for _, tt := range []struct {
		e    domain.Event
		want string
	}{
		{domain.Event{Time: at, Kind: domain.EventSchedulerDecision, Message: "queue java (claude-16): heavy_slots_exhausted", Attrs: map[string]string{"decision": "queue"}},
			"08:38:00  queue     java (claude-16): heavy_slots_exhausted"},
		{domain.Event{Time: at, Kind: domain.EventProcessLost, Message: "heavy workload java (pid 1) exited"},
			"08:38:00  exit      heavy workload java (pid 1) exited"},
		{domain.Event{Time: at.AddDate(0, 0, -1), Kind: "something_new", Message: "x"},
			"11-17 08:38:00  something_new  x"},
	} {
		if got := eventLine(tt.e, now, false); got != tt.want {
			t.Errorf("got  %q\nwant %q", got, tt.want)
		}
	}
	if got := eventLine(domain.Event{Time: at, Kind: domain.EventSchedulerDecision, Attrs: map[string]string{"decision": "resume"}}, now, true); !strings.Contains(got, "\x1b[32mresume") {
		t.Errorf("no color: %q", got)
	}
}
