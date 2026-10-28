package app

import (
	"fmt"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// The capacity verdict lives here rather than in package capacity because
// throtd's acquire method grants admission leases with exactly the verdict
// clients see; package capacity re-exports it for the MCP server and
// `throt run`.

// Workload states, matching `throt status`.
const (
	StateRunning = "running"
	StatePaused  = "PAUSED"
	StateQueued  = "QUEUED"
)

// Verdict is the result of Assess. Its JSON form is the structured content
// of the throt_capacity MCP tool.
type Verdict struct {
	OKToStart      bool   `json:"ok_to_start"`
	Reason         string `json:"reason"`
	Pressure       string `json:"pressure"`
	PressureReason string `json:"pressure_reason,omitempty"`
	SchedulerMode  string `json:"scheduler_mode"`
	RunningHeavy   int    `json:"running_heavy"`
	// Reserved counts heavy slots promised to `throt run` commands that have
	// not started their heavy work yet (admission leases). They count as
	// running against the limits.
	Reserved   int    `json:"reserved"`
	HeavyLimit int    `json:"heavy_limit"`
	Agent      string `json:"agent,omitempty"`
	// AgentRunningHeavy is set only when an agent was requested.
	AgentRunningHeavy *int   `json:"agent_running_heavy,omitempty"`
	Suggestion        string `json:"suggestion"`
}

// WorkloadStates maps every heavy workload to running, PAUSED, or QUEUED.
// Scheduler queueing wins over a plain pause, as in `throt status`.
func WorkloadStates(st StatusResult) map[domain.ProcessKey]string {
	states := make(map[domain.ProcessKey]string, len(st.Heavy))
	for _, h := range st.Heavy {
		states[h.Key] = StateRunning
	}
	for _, p := range st.Paused {
		if _, ok := states[p.Key]; ok {
			states[p.Key] = StatePaused
		}
	}
	for _, k := range st.Scheduler.Queued {
		if _, ok := states[k]; ok {
			states[k] = StateQueued
		}
	}
	return states
}

// Assess answers "should I start a heavy job now?" for agent (an instance
// label such as "claude-1", or "" for none). Admission leases in
// st.Scheduler.Leases count as running heavy work, overall and for their
// agent. It fails open: missing data never blocks work.
func Assess(st StatusResult, agent string) Verdict {
	states := WorkloadStates(st)
	running, agentRunning := 0, 0
	for _, h := range st.Heavy {
		if states[h.Key] != StateRunning {
			continue
		}
		running++
		if agent != "" && h.Owner == agent {
			agentRunning++
		}
	}
	reserved, agentReserved := len(st.Scheduler.Leases), 0
	for _, l := range st.Scheduler.Leases {
		if agent != "" && l.Agent == agent {
			agentReserved++
		}
	}
	busy, agentBusy := running+reserved, agentRunning+agentReserved
	mode := st.Scheduler.Mode
	if mode == "" {
		mode = scheduler.ModeOff
	}
	c := Verdict{
		Pressure:       st.Pressure.String(),
		PressureReason: st.Reason,
		SchedulerMode:  string(mode),
		RunningHeavy:   running,
		Reserved:       reserved,
		HeavyLimit:     st.Scheduler.HeavyLimit,
		Agent:          agent,
	}
	if agent != "" {
		c.AgentRunningHeavy = &agentRunning
	}
	pressure := c.Pressure
	if st.Reason != "" {
		pressure += " (" + st.Reason + ")"
	}
	switch {
	case st.Pressure >= domain.PressureHigh:
		c.Reason = "pressure is " + pressure
		c.Suggestion = "pressure is " + pressure + "; wait or run a lighter task"
	case mode != scheduler.ModeOff && agent != "" && st.Scheduler.PerAgentLimit > 0 && agentBusy >= st.Scheduler.PerAgentLimit:
		c.Reason = fmt.Sprintf("%s already runs %d heavy jobs (per-agent limit %d)", agent, agentBusy, st.Scheduler.PerAgentLimit)
		if agentReserved > 0 {
			c.Reason = fmt.Sprintf("%s already runs %d heavy jobs, %d of them reserved (per-agent limit %d)", agent, agentBusy, agentReserved, st.Scheduler.PerAgentLimit)
		}
		c.Suggestion = "wait for one of your heavy jobs to finish; a new one would wait for your budget"
	case mode != scheduler.ModeOff && len(st.Scheduler.Queued) > 0:
		c.Reason = fmt.Sprintf("%d heavy jobs are already queued", len(st.Scheduler.Queued))
		c.Suggestion = "new heavy jobs queue behind waiting ones; wait or run a lighter task"
	case mode != scheduler.ModeOff && busy >= st.Scheduler.HeavyLimit:
		c.Reason = fmt.Sprintf("all heavy slots are in use (%d running, limit %d)", running, st.Scheduler.HeavyLimit)
		if reserved > 0 {
			c.Reason = fmt.Sprintf("all heavy slots are in use (%d running, %d reserved, limit %d)", running, reserved, st.Scheduler.HeavyLimit)
		}
		if mode == scheduler.ModeEnforce {
			c.Suggestion = "a new heavy job would be queued (paused) by throtd until a slot frees; wait for a running heavy job to finish or run a lighter task"
		} else {
			c.Suggestion = "wait for a running heavy job to finish or run a lighter task"
		}
	case st.Pressure == domain.PressureUnknown:
		c.OKToStart = true
		c.Reason = "pressure is unknown (no stable sample yet); allowed, but capacity is unverified"
		c.Suggestion = "ok to start; check again shortly if the job is large"
	case st.Pressure == domain.PressureElevated:
		c.OKToStart = true
		c.Reason = "pressure is " + pressure + " but below high"
		c.Suggestion = "ok to start one heavy job; avoid starting several at once"
	default:
		c.OKToStart = true
		c.Reason = "pressure is " + pressure
		if mode == scheduler.ModeOff {
			c.Reason += "; scheduler is off, so no heavy limit applies"
		} else {
			c.Reason += fmt.Sprintf("; %d of %d heavy slots in use", busy, st.Scheduler.HeavyLimit)
			if reserved > 0 {
				c.Reason += fmt.Sprintf(" (%d reserved)", reserved)
			}
		}
		c.Suggestion = "ok to start"
	}
	if agent != "" && !hasAgent(st, agent) {
		c.Suggestion += fmt.Sprintf(" (note: agent %q is not currently detected; see throt_status for labels)", agent)
	}
	return c
}

func hasAgent(st StatusResult, label string) bool {
	for _, a := range st.Agents {
		if a.Agent == label {
			return true
		}
	}
	return false
}
