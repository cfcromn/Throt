package mcp

import (
	"encoding/json"
	"errors"
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
	explainStart = time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
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

func TestExplainTool(t *testing.T) {
	queuedAt := explainStart.Add(2*time.Minute + 11*time.Second)
	queue := decision(queuedAt, 4312, "queue", scheduler.ReasonSlotsExhausted, "false")
	queuedBy := func(mode scheduler.Mode) func() app.StatusResult {
		return func() app.StatusResult {
			st := explainStatus(mode)
			st.Scheduler.Queued = []domain.ProcessKey{makeKey}
			return st
		}
	}
	tests := []struct {
		name      string
		status    func() app.StatusResult
		events    []domain.Event
		wantState string
		want      []string
		absent    []string
	}{
		{
			name: "queued by scheduler",
			status: func() app.StatusResult {
				st := queuedBy(scheduler.ModeEnforce)()
				st.Paused = []control.PausedWorkload{{Key: makeKey, By: control.OriginScheduler, State: control.StatePaused}}
				return st
			},
			events: []domain.Event{
				// Same PID before this process started: a different process.
				{Time: explainStart.Add(-time.Hour), Kind: domain.EventProcessDiscovered, Attrs: map[string]string{"pid": "4312"}},
				decision(queuedAt, 4400, "queue", scheduler.ReasonSlotsExhausted, "false"),
				queue,
			},
			wantState: explainQueued,
			want: []string{
				"make (pid 4312, build)", "state: QUEUED by scheduler", "owner: claude-1 (pid 1000)",
				"scheduler: enforce, heavy limit 2, 1/2 heavy running", "pressure: elevated",
				"2026-10-28T14:02:11Z  scheduler_decision  queue  heavy_slots_exhausted  elevated  2/2",
				"Queued at 2026-10-28T14:02:11Z because 2/2 heavy slots were in use under elevated pressure; it resumes when a slot frees (after the resume cooldown).",
			},
			absent: []string{"13:00:00"},
		},
		{
			name: "queued behind waiting work", status: queuedBy(scheduler.ModeEnforce), wantState: explainQueued,
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonQueueWaiting, "false")},
			want:   []string{"because other work was already waiting for a heavy slot", "in queue order"},
		},
		{
			name: "queued over the per-agent budget", wantState: explainQueued,
			status: func() app.StatusResult {
				st := queuedBy(scheduler.ModeEnforce)()
				st.Scheduler.PerAgentLimit = 1
				return st
			},
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonOwnerBudget, "false")},
			want:   []string{"claude-1 already ran its per-agent limit of 1 heavy workloads", "when one of them finishes"},
		},
		{
			name: "queued under high pressure", status: queuedBy(scheduler.ModeEnforce), wantState: explainQueued,
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonPressureHigh, "false")},
			want:   []string{"high pressure blocks new heavy work", "max_queue_wait"},
		},
		{
			name: "preempted under critical pressure", status: queuedBy(scheduler.ModeEnforce), wantState: explainQueued,
			events: []domain.Event{decision(queuedAt, 4312, "preempt", scheduler.ReasonPressureCritical, "false")},
			want:   []string{"Preempted at 2026-10-28T14:02:11Z because pressure was critical; it resumes when pressure drops and a slot frees"},
		},
		{
			name: "queued, decision evicted", status: queuedBy(scheduler.ModeEnforce), wantState: explainQueued,
			want: []string{"(none in the daemon's event history)", "the decision is no longer in the event history"},
		},
		{
			name: "paused by the user", wantState: explainPaused,
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Paused = []control.PausedWorkload{{Key: makeKey, By: control.OriginManual, State: control.StatePaused, Since: queuedAt}}
				return st
			},
			events: []domain.Event{{Time: queuedAt, Kind: domain.EventControlPaused, Attrs: map[string]string{"pid": "4312", "owner": "claude-1"}}},
			want: []string{
				"state: PAUSED by the user", "2026-10-28T14:02:11Z  control_paused  -  -  -  -",
				"Paused by the user at 2026-10-28T14:02:11Z; the scheduler never resumes manual pauses, so it stays paused until the user runs `throt resume 4312`.",
			},
		},
		{
			name: "resuming", wantState: explainResuming,
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Paused = []control.PausedWorkload{{Key: makeKey, State: control.StateResuming}}
				return st
			},
			want: []string{"Being resumed", "retries it every sample"},
		},
		{
			name: "running, scheduler off", wantState: explainRunning,
			status: func() app.StatusResult { return explainStatus(scheduler.ModeOff) },
			want:   []string{"state: running", "off, heavy limit 2, 2/2", "the scheduler is off, so only `throt pause 4312` would stop it."},
		},
		{
			name: "running, empty mode means off", wantState: explainRunning,
			status: func() app.StatusResult { return explainStatus("") },
			want:   []string{"scheduler: off,"},
		},
		{
			name: "running, observe would queue", wantState: explainRunning,
			status: func() app.StatusResult { return explainStatus(scheduler.ModeObserve) },
			events: []domain.Event{decision(queuedAt, 4312, "queue", scheduler.ReasonSlotsExhausted, "true")},
			want:   []string{"at 2026-10-28T14:02:11Z the scheduler would have queued it (heavy slots exhausted), but observe mode never acts."},
		},
		{
			name: "running, observe", wantState: explainRunning,
			status: func() app.StatusResult { return explainStatus(scheduler.ModeObserve) },
			want:   []string{"observe mode, so it logs decisions but never pauses work"},
		},
		{
			name: "running after a resume", wantState: explainRunning,
			status: func() app.StatusResult { return explainStatus(scheduler.ModeEnforce) },
			events: []domain.Event{queue, decision(queuedAt.Add(time.Minute), 4312, "resume", scheduler.ReasonSlotAvailable, "false")},
			want:   []string{"Running since the scheduler resumed it at 2026-10-28T14:03:11Z (slot available)."},
		},
		{
			name: "running in a slot, per-agent limit", wantState: explainRunning,
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Scheduler.PerAgentLimit = 1
				return st
			},
			want: []string{"enforce, heavy limit 2, per-agent limit 1", "Running in one of 2/2 heavy slots under elevated pressure"},
		},
		{
			name: "unattributed", wantState: explainRunning,
			status: func() app.StatusResult {
				st := explainStatus(scheduler.ModeEnforce)
				st.Heavy[0].Owner, st.Heavy[0].OwnerPID = "", 0
				return st
			},
			want: []string{"owner: unattributed (observe-only)", "not attributed to an agent, so throt never pauses it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{status: tt.status(), events: tt.events}
			r, sc := call(t, c, "throt_explain", `{"pid":4312}`)
			if r.IsError {
				t.Fatalf("isError: %s", r.Content[0].Text)
			}
			if c.lastLimit != app.MaxEventsPerCall {
				t.Errorf("events limit = %d, want %d", c.lastLimit, app.MaxEventsPerCall)
			}
			out := r.Content[0].Text
			for _, s := range tt.want {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q:\n%s", s, out)
				}
			}
			for _, s := range append(tt.absent, "4400") {
				if strings.Contains(out, s) {
					t.Errorf("unexpected %q:\n%s", s, out)
				}
			}
			var v explainView
			if err := json.Unmarshal(sc, &v); err != nil {
				t.Fatal(err)
			}
			if v.PID != 4312 || v.Name != "make" || v.State != tt.wantState || v.Summary == "" || v.Events == nil ||
				!strings.HasSuffix(out, v.Summary) {
				t.Errorf("structured = %+v", v)
			}
		})
	}
}

func TestExplainToolLimitsEvents(t *testing.T) {
	var events []domain.Event
	for i := range 15 {
		events = append(events, decision(explainStart.Add(time.Duration(i)*time.Second), 4312, "queue", fmt.Sprint("reason_", i), "true"))
	}
	r, sc := call(t, &fakeClient{status: explainStatus(scheduler.ModeObserve), events: events}, "throt_explain", `{"pid":4312}`)
	out := r.Content[0].Text
	if n := strings.Count(out, "scheduler_decision"); n != explainEvents {
		t.Errorf("%d events shown, want %d:\n%s", n, explainEvents, out)
	}
	if strings.Contains(out, "reason_4 ") || !strings.Contains(out, "reason_5 ") || !strings.Contains(out, "reason_14") {
		t.Errorf("want the newest %d events (reason_5..reason_14):\n%s", explainEvents, out)
	}
	var v explainView
	if err := json.Unmarshal(sc, &v); err != nil || len(v.Events) != explainEvents || v.Events[0].Reason != "reason_5" {
		t.Errorf("structured events = %+v (%v)", v.Events, err)
	}
}

func TestExplainToolErrors(t *testing.T) {
	many := explainStatus(scheduler.ModeEnforce)
	for i := range maxTextRows + 3 {
		many.Heavy = append(many.Heavy, process.WorkloadView{Key: key(5000 + i), Name: "go"})
	}
	down := errors.New("connect to throtd (is it running?): refused")
	tests := []struct {
		name   string
		client *fakeClient
		args   string
		want   string
	}{
		{"not heavy", &fakeClient{status: explainStatus(scheduler.ModeEnforce)}, `{"pid":999}`,
			"pid 999 is not a heavy workload in the latest sample; heavy workloads: 4312 (make), 4400 (gradle)"},
		{"no heavy workloads", &fakeClient{}, `{"pid":999}`, "no heavy workloads right now"},
		{"long heavy list is bounded", &fakeClient{status: many}, `{"pid":999}`, "and 5 more"},
		{"missing pid", &fakeClient{}, `{}`, "pid must be a positive integer"},
		{"no arguments", &fakeClient{}, ``, "pid must be a positive integer"},
		{"zero pid", &fakeClient{}, `{"pid":0}`, "pid must be a positive integer"},
		{"negative pid", &fakeClient{}, `{"pid":-4}`, "pid must be a positive integer"},
		{"string pid", &fakeClient{}, `{"pid":"4312"}`, "invalid arguments"},
		{"fractional pid", &fakeClient{}, `{"pid":43.5}`, "invalid arguments"},
		{"unknown argument", &fakeClient{}, `{"pid":1,"verbose":true}`, "invalid arguments"},
		{"daemon down", &fakeClient{err: down}, `{"pid":4312}`, "throtd &"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sc := call(t, tt.client, "throt_explain", tt.args)
			if !r.IsError || !strings.Contains(r.Content[0].Text, tt.want) || len(sc) != 0 {
				t.Fatalf("got isError %v: %s", r.IsError, r.Content[0].Text)
			}
		})
	}
}
