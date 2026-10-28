package domain

import (
	"strconv"
	"testing"
)

func TestEventLogBounded(t *testing.T) {
	log := NewEventLog(3, "run-1")
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

func TestEventLogSince(t *testing.T) {
	log := NewEventLog(3, "run-1")
	if got := log.Since(0, 0); len(got) != 0 {
		t.Fatalf("empty log returned %d events", len(got))
	}
	for i := range 5 {
		// Append owns Seq; a caller's value is replaced.
		log.Append(Event{Seq: 99, Message: strconv.Itoa(i)})
	}
	// Seqs 1 and 2 were evicted; 3, 4 and 5 are retained.
	tests := []struct {
		seq   uint64
		limit int
		want  []uint64
	}{
		{0, 0, []uint64{3, 4, 5}},
		{0, 2, []uint64{3, 4}},
		{1, 0, []uint64{3, 4, 5}},
		{3, 0, []uint64{4, 5}},
		{3, 1, []uint64{4}},
		{4, 10, []uint64{5}},
		{5, 0, nil},
		{50, 0, nil},
	}
	for _, tt := range tests {
		got := log.Since(tt.seq, tt.limit)
		if len(got) != len(tt.want) {
			t.Fatalf("Since(%d, %d) len = %d, want %d", tt.seq, tt.limit, len(got), len(tt.want))
		}
		for i, e := range got {
			if e.Seq != tt.want[i] || e.Message != strconv.Itoa(int(e.Seq)-1) {
				t.Errorf("Since(%d, %d)[%d] = seq %d %q, want seq %d", tt.seq, tt.limit, i, e.Seq, e.Message, tt.want[i])
			}
		}
	}
	if got := log.Recent(1); got[0].Seq != 5 {
		t.Errorf("Recent(1) seq = %d, want 5", got[0].Seq)
	}
}
