package capacity

import (
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

func key(pid int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)}
}

// sampleStatus has two running heavy workloads (one per agent), one paused
// and one queued.
func sampleStatus(level domain.PressureLevel, mode scheduler.Mode, limit int) app.StatusResult {
	return app.StatusResult{
		Pressure: level,
		Reason:   "memory_used>=0.9",
		Agents: []process.AgentView{
			{Key: key(100), Agent: "claude-1", Name: "claude"},
			{Key: key(200), Agent: "codex-1", Name: "codex"},
		},
		Heavy: []process.WorkloadView{
			{Key: key(101), Name: "java", Class: domain.ClassBuild, Owner: "claude-1"},
			{Key: key(201), Name: "go", Class: domain.ClassTest, Owner: "codex-1"},
			{Key: key(102), Name: "buildkitd", Class: domain.ClassContainerBuild, Owner: "claude-1"},
			{Key: key(300), Name: "make", Class: domain.ClassBuild},
		},
		Paused:    []control.PausedWorkload{{Key: key(102), Name: "buildkitd", Owner: "claude-1"}, {Key: key(300)}},
		Scheduler: app.SchedulerStatus{Mode: mode, HeavyLimit: limit, Queued: []domain.ProcessKey{key(300)}},
	}
}

func ptr(i int) *int { return &i }

func TestWorkloadStates(t *testing.T) {
	got := WorkloadStates(sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3))
	want := map[int]string{101: StateRunning, 201: StateRunning, 102: StatePaused, 300: StateQueued}
	for pid, s := range want {
		if got[key(pid)] != s {
			t.Errorf("pid %d = %q, want %q", pid, got[key(pid)], s)
		}
	}
}

func TestAssess(t *testing.T) {
	perAgent := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3)
	perAgent.Scheduler.PerAgentLimit = 1
	// Admission leases count as running heavy work, overall and per agent.
	leased := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3)
	leased.Scheduler.Leases = []app.Lease{{ID: "1", Process: key(400), Agent: "codex-1"}}
	leasedAgent := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 4)
	leasedAgent.Scheduler.PerAgentLimit = 2
	leasedAgent.Scheduler.Leases = leased.Scheduler.Leases
	leasedFree := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 4)
	leasedFree.Scheduler.Leases = leased.Scheduler.Leases
	tests := []struct {
		name           string
		status         app.StatusResult
		agent          string
		keepQueued     bool
		wantOK         bool
		wantAgent      *int
		reason, suggst string
	}{
		{name: "normal, slot free", status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3),
			wantOK: true, reason: "2 of 3 heavy slots in use", suggst: "ok to start"},
		{name: "elevated is ok with caveat", status: sampleStatus(domain.PressureElevated, scheduler.ModeObserve, 3),
			wantOK: true, reason: "elevated", suggst: "avoid starting several"},
		{name: "high pressure", status: sampleStatus(domain.PressureHigh, scheduler.ModeEnforce, 3),
			reason: "pressure is high (memory_used>=0.9)", suggst: "pressure is high (memory_used>=0.9); wait or run a lighter task"},
		{name: "critical with scheduler off", status: sampleStatus(domain.PressureCritical, scheduler.ModeOff, 1),
			reason: "pressure is critical", suggst: "wait or run a lighter task"},
		{name: "at limit, enforce", status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 2),
			reason: "all heavy slots are in use (2 running, limit 2)", suggst: "queued (paused) by throtd"},
		{name: "above limit, observe", status: sampleStatus(domain.PressureNormal, scheduler.ModeObserve, 1),
			reason: "all heavy slots are in use", suggst: "wait for a running heavy job"},
		{name: "scheduler off ignores limit", status: sampleStatus(domain.PressureNormal, scheduler.ModeOff, 1),
			wantOK: true, reason: "scheduler is off", suggst: "ok to start"},
		{name: "empty mode means off", status: sampleStatus(domain.PressureNormal, "", 1),
			wantOK: true, reason: "scheduler is off"},
		{name: "unknown pressure fails open", status: sampleStatus(domain.PressureUnknown, scheduler.ModeEnforce, 3),
			wantOK: true, reason: "unknown", suggst: "check again"},
		{name: "agent count", status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3), agent: "claude-1",
			wantOK: true, wantAgent: ptr(1), suggst: "ok to start"},
		{name: "unknown agent", status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3), agent: "ghost",
			wantOK: true, wantAgent: ptr(0), suggst: `agent "ghost" is not currently detected`},
		{name: "queued work waits first", status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3), keepQueued: true,
			reason: "1 heavy jobs are already queued", suggst: "queue behind waiting ones"},
		{name: "agent at its budget", status: perAgent, agent: "claude-1",
			wantAgent: ptr(1), reason: "per-agent limit 1", suggst: "budget"},
		{name: "leases fill the last slot", status: leased,
			reason: "all heavy slots are in use (2 running, 1 reserved, limit 3)", suggst: "queued (paused) by throtd"},
		{name: "leases count toward the agent budget", status: leasedAgent, agent: "codex-1",
			wantAgent: ptr(1), reason: "codex-1 already runs 2 heavy jobs, 1 of them reserved (per-agent limit 2)", suggst: "budget"},
		{name: "slot free beside a lease", status: leasedFree,
			wantOK: true, reason: "3 of 4 heavy slots in use (1 reserved)", suggst: "ok to start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.keepQueued {
				tt.status.Scheduler.Queued = nil
			}
			v := Assess(tt.status, tt.agent)
			if v.OKToStart != tt.wantOK || v.RunningHeavy != 2 || v.HeavyLimit != tt.status.Scheduler.HeavyLimit ||
				v.Reserved != len(tt.status.Scheduler.Leases) {
				t.Errorf("got %+v", v)
			}
			if !strings.Contains(v.Reason, tt.reason) || !strings.Contains(v.Suggestion, tt.suggst) {
				t.Errorf("reason %q / suggestion %q", v.Reason, v.Suggestion)
			}
			switch {
			case tt.wantAgent == nil && v.AgentRunningHeavy != nil:
				t.Errorf("unexpected agent count %d", *v.AgentRunningHeavy)
			case tt.wantAgent != nil && (v.AgentRunningHeavy == nil || *v.AgentRunningHeavy != *tt.wantAgent):
				t.Errorf("agent count = %v, want %d", v.AgentRunningHeavy, *tt.wantAgent)
			}
		})
	}
}
