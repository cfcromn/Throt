package domain

import (
	"strconv"
	"testing"
)

func TestEventLogBounded(t *testing.T) {
	log := NewEventLog(3)
	if got := log.Recent(0); len(got) != 0 {
		t.Fatalf("empty log returned %d events", len(got))
	}
	for i := range 5 {
		log.Append(Event{Message: strconv.Itoa(i)})
	}
	tests := []struct {
		n    int
		want []string
	}{
		{0, []string{"2", "3", "4"}},
		{2, []string{"3", "4"}},
		{10, []string{"2", "3", "4"}},
	}
	for _, tt := range tests {
		got := log.Recent(tt.n)
		if len(got) != len(tt.want) {
			t.Fatalf("Recent(%d) len = %d, want %d", tt.n, len(got), len(tt.want))
		}
		for i, e := range got {
			if e.Message != tt.want[i] {
				t.Errorf("Recent(%d)[%d] = %q, want %q", tt.n, i, e.Message, tt.want[i])
			}
		}
	}
}
