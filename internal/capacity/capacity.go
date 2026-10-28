// Package capacity answers "should a heavy job start now?" from a throtd
// status snapshot. It is shared by the MCP server (throt_capacity) and
// `throt run`. The verdict itself is advisory and controls nothing; slots
// are only reserved through throtd's acquire method, which grants an
// admission lease with this same verdict. The implementation lives in
// package app so throtd can use it; this package keeps the client-facing
// names.
package capacity

import (
	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// Workload states, matching `throt status`.
const (
	StateRunning = app.StateRunning
	StatePaused  = app.StatePaused
	StateQueued  = app.StateQueued
)

// Verdict is the result of Assess. Its JSON form is the structured content
// of the throt_capacity MCP tool.
type Verdict = app.Verdict

// WorkloadStates maps every heavy workload to running, PAUSED, or QUEUED.
// Scheduler queueing wins over a plain pause, as in `throt status`.
func WorkloadStates(st app.StatusResult) map[domain.ProcessKey]string {
	return app.WorkloadStates(st)
}

// Assess answers "should I start a heavy job now?" for agent (an instance
// label such as "claude-1", or "" for none). Admission leases count as
// running heavy work. It fails open: missing data never blocks work.
func Assess(st app.StatusResult, agent string) Verdict { return app.Assess(st, agent) }
