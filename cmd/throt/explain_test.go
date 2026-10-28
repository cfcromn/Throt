package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

var (
	explainStart = time.Date(2026, 10, 28, 14, 0, 0, 0, time.Local)
	makeKey      = domain.ProcessKey{PID: 4312, Start: explainStart}
	gradleKey    = domain.ProcessKey{PID: 4400, Start: explainStart}
)

func explainStatus(mode scheduler.Mode) app.StatusResult {
	return app.StatusResult{
		Pressure: domain.PressureElevated,
		Heavy: []process.WorkloadView{
			{Key: makeKey, Name: "make", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 1000},
			{Key: gradleKey, Name: "gradle", Class: domain.ClassBuild, Owner: "codex-1", OwnerPID: 1001},
		},
		Scheduler: app.SchedulerStatus{Mode: mode, HeavyLimit: 2},
	}
}

func decision(at time.Time, pid int, action, reason, observed string) domain.Event {
	return domain.Event{Time: at, Kind: domain.EventSchedulerDecision, Attrs: map[string]string{
		"decision": action, "pid": fmt.Sprint(pid), "reason": reason,
		"pressure": "elevated", "running": "2", "limit": "2", "observed": observed,
	}}
}

func TestExplain(t *testing.T) {
	queuedAt := explainStart.Add(2*time.Minute + 11*time.Second)
	queue := decision(queuedAt, 4312, "queue", scheduler.ReasonSlotsExhausted, "false")
	tests := []struct {
		name   string
		status func() app.StatusResult
		events []domain.Event
		want   []string
		absent []string
	}{
		{
			name: "queued by scheduler",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Paused = []control.PausedWorkload{{Key: makeKey, By: control.OriginScheduler, State: control.StatePaused}}
				st.Scheduler.Queued = []domain.ProcessKey{makeKey}
				return st
			},
			events: []domain.Event{
				// Same PID before this process started: a different process.
				{Time: explainStart.Add(-time.Hour), Kind: domain.EventProcessDiscovered, Attrs: map[string]string{"pid": "4312"}},
				decision(queuedAt, 4400, "queue", scheduler.ReasonSlotsExhausted, "false"),
				queue,
			},
			want: []string{
				"make (pid 4312, build)", "QUEUED by scheduler", "claude-1 (pid 1000)",
				"enforce, heavy limit 2, 1/2 heavy running", "pressure   elevated",
				"14:02:11  scheduler_decision  queue   heavy_slots_exhausted  elevated  2/2",
				"Queued at 14:02:11 because 2/2 heavy slots were in use under elevated pressure; it resumes when a slot frees (cooldown 10s).",
			},
			absent: []string{"13:00:00"},
		},
		{
			name: "queued behind waiting work",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Scheduler.Queued = []domain.ProcessKey{makeKey}
				return st
			},
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonQueueWaiting, "false")},
			want:   []string{"because other work was already waiting for a heavy slot", "in queue order"},
		},
		{
			name: "preempted under critical pressure",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Scheduler.Queued = []domain.ProcessKey{makeKey}
				return st
			},
			events: []domain.Event{decision(queuedAt, 4312, "preempt", scheduler.ReasonPressureCritical, "false")},
			want:   []string{"Preempted at 14:02:11 because pressure was critical; it resumes when pressure drops and a slot frees"},
		},
		{
			name: "queued, decision evicted",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Scheduler.Queued = []domain.ProcessKey{makeKey}
				return st
			},
			want: []string{"(none in the daemon's event history)", "the decision is no longer in the event history"},
		},
		{
			name: "paused by you",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Paused = []control.PausedWorkload{{Key: makeKey, By: control.OriginManual, State: control.StatePaused, Since: queuedAt}}
				return st
			},
			events: []domain.Event{{Time: queuedAt, Kind: domain.EventControlPaused, Attrs: map[string]string{"pid": "4312", "owner": "claude-1"}}},
			want: []string{
				"PAUSED by you", "14:02:11  control_paused  -       -       -         -",
				"Paused by you at 14:02:11; the scheduler never resumes manual pauses, so it stays paused until `throt resume 4312`.",
			},
		},
		{
			name:   "running, scheduler off",
			status: func() app.StatusResult { return explainStatus(scheduler.ModeOff) },
			want:   []string{"state      running", "off, heavy limit 2, 2/2", "the scheduler is off, so only `throt pause 4312` would stop it."},
		},
		{
			name:   "running, observe would queue",
			status: func() app.StatusResult { return explainStatus(scheduler.ModeObserve) },
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonSlotsExhausted, "true")},
			want:   []string{"at 14:02:11 the scheduler would have queued it (heavy slots exhausted), but observe mode never acts."},
		},
		{
			name:   "running after a resume",
			status: func() app.StatusResult { return explainStatus(scheduler.ModeEnforce) },
			events: []domain.Event{queue, decision(queuedAt.Add(time.Minute), 4312, "resume", scheduler.ReasonSlotAvailable, "false")},
			want:   []string{"Running since the scheduler resumed it at 14:03:11 (slot available)."},
		},
		{
			name: "running in a slot, per-agent limit",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Scheduler.PerAgentLimit = 1
				return st
			},
			want: []string{"enforce, heavy limit 2, per-agent limit 1", "Running in one of 2/2 heavy slots under elevated pressure"},
		},
		{
			name: "unattributed",
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Heavy[0].Owner, st.Heavy[0].OwnerPID = "", 0
				return st
			},
			want: []string{"unattributed (observe-only)", "not attributed to an agent, so throt never pauses it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := explain(&b, 4312, tt.status(), tt.events, 10*time.Second); err != nil {
				t.Fatal(err)
			}
			out := b.String()
			for _, s := range tt.want {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q:\n%s", s, out)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(out, s) {
					t.Errorf("unexpected %q:\n%s", s, out)
				}
			}
			if strings.Contains(out, "4400") {
				t.Errorf("events of another workload shown:\n%s", out)
			}
		})
	}
}

func TestExplainLimitsEvents(t *testing.T) {
	var events []domain.Event
	for i := range 15 {
		events = append(events, decision(explainStart.Add(time.Duration(i)*time.Second), 4312, "queue", fmt.Sprint("reason_", i), "true"))
	}
	var b strings.Builder
	if err := explain(&b, 4312, explainStatus(scheduler.ModeObserve), events, time.Second); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if n := strings.Count(out, "scheduler_decision"); n != explainEvents {
		t.Errorf("%d events shown, want %d:\n%s", n, explainEvents, out)
	}
	if strings.Contains(out, "reason_4 ") || !strings.Contains(out, "reason_5 ") || !strings.Contains(out, "reason_14") {
		t.Errorf("want the newest %d events (reason_5..reason_14):\n%s", explainEvents, out)
	}
}

func TestExplainUnknownPID(t *testing.T) {
	err := explain(&strings.Builder{}, 999, explainStatus(scheduler.ModeEnforce), nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "pid 999 is not a heavy workload") || !strings.Contains(err.Error(), "4312 (make), 4400 (gradle)") {
		t.Errorf("err = %v, want not-heavy error listing 4312 and 4400", err)
	}
	err = explain(&strings.Builder{}, 999, app.StatusResult{}, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "no heavy workloads right now") {
		t.Errorf("err = %v, want no-heavy-workloads error", err)
	}
}
