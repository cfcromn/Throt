package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

type fetchResult struct {
	events []domain.Event
	err    error
}

// scriptedFetch replays results in order and cancels ctx after the last.
type scriptedFetch struct {
	results []fetchResult
	cancel  context.CancelFunc
	sinces  []uint64
	limits  []int
}

func (s *scriptedFetch) fetch(_ context.Context, since uint64, limit int) ([]domain.Event, error) {
	s.sinces = append(s.sinces, since)
	s.limits = append(s.limits, limit)
	r := s.results[0]
	s.results = s.results[1:]
	if len(s.results) == 0 {
		s.cancel()
	}
	return r.events, r.err
}

func seqs(from, to uint64) []domain.Event {
	var out []domain.Event
	for s := from; s <= to; s++ {
		out = append(out, domain.Event{Seq: s, Kind: domain.EventAgentDiscovered})
	}
	return out
}

func TestFollow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scriptedFetch{cancel: cancel, results: []fetchResult{
		{events: seqs(1, 3)},
		{},                        // nothing new
		{events: seqs(4, 4)},      // one new event
		{err: errors.New("down")}, // daemon stopped
		{err: errors.New("down")},
		{events: seqs(1, 2)}, // a new daemon: seq went back
		{events: seqs(6, 6)}, // 3..5 were evicted before this poll
		{},                   // interrupted during this poll
	}}
	var out, errw strings.Builder
	if err := follow(ctx, s.fetch, &out, &errw, time.Millisecond, 50); err != nil {
		t.Fatal(err)
	}
	var got []uint64
	dec := json.NewDecoder(strings.NewReader(out.String()))
	for dec.More() {
		var e domain.Event
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e.Seq)
	}
	if want := "[1 2 3 4 1 2 6]"; jsonSeqs(got) != want {
		t.Errorf("printed seqs %s, want %s", jsonSeqs(got), want)
	}
	if want := "[0 3 3 4 4 4 2 6]"; jsonSeqs(s.sinces) != want {
		t.Errorf("since values %s, want %s", jsonSeqs(s.sinces), want)
	}
	if s.limits[0] != 50 || s.limits[1] != app.MaxEventsPerCall {
		t.Errorf("limits %v, want 50 first, then %d", s.limits, app.MaxEventsPerCall)
	}
	notices := errw.String()
	for _, want := range []string{"down; retrying", "reconnected", "went back from 4 to 1: throtd restarted", "missed 3 events"} {
		if !strings.Contains(notices, want) {
			t.Errorf("stderr missing %q:\n%s", want, notices)
		}
	}
	if n := strings.Count(notices, "retrying"); n != 1 {
		t.Errorf("connection error reported %d times, want once:\n%s", n, notices)
	}
}

func jsonSeqs(v []uint64) string {
	b, _ := json.Marshal(v)
	return strings.ReplaceAll(string(b), ",", " ")
}

func TestFollowErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &scriptedFetch{cancel: func() {}, results: []fetchResult{{err: errors.New("connect: no such file")}, {}}}
	if err := follow(ctx, s.fetch, &strings.Builder{}, &strings.Builder{}, time.Millisecond, 50); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("first-call error = %v, want it returned", err)
	}
	// An old daemon that does not number events cannot be followed.
	s = &scriptedFetch{cancel: func() {}, results: []fetchResult{{events: []domain.Event{{Kind: domain.EventDaemonStarted}}}, {}}}
	if err := follow(ctx, s.fetch, &strings.Builder{}, &strings.Builder{}, time.Millisecond, 50); err == nil || !strings.Contains(err.Error(), "upgrade throtd") {
		t.Errorf("unnumbered events error = %v, want upgrade hint", err)
	}
}

func TestFollowDetectsRestartPastLastSeq(t *testing.T) {
	run := func(inst string, from, to uint64) []domain.Event {
		es := seqs(from, to)
		for i := range es {
			es[i].Instance = inst
		}
		return es
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &scriptedFetch{cancel: cancel, results: []fetchResult{
		{events: run("a", 1, 3)},
		// A new daemon already logged 5 events, so since=3 returns 4..5:
		// only the instance shows the restart.
		{events: run("b", 4, 5)},
		{events: run("b", 1, 5)},
		{}, // interrupted during this poll
	}}
	var out, errw strings.Builder
	if err := follow(ctx, s.fetch, &out, &errw, time.Millisecond, 50); err != nil {
		t.Fatal(err)
	}
	if want := "[0 3 0 5]"; jsonSeqs(s.sinces) != want {
		t.Errorf("since values %s, want %s", jsonSeqs(s.sinces), want)
	}
	if !strings.Contains(errw.String(), "throtd restarted") {
		t.Errorf("no restart notice:\n%s", errw.String())
	}
	if n := strings.Count(out.String(), `"instance":"b"`); n != 5 {
		t.Errorf("printed %d events of the new daemon, want all 5:\n%s", n, out.String())
	}
}
