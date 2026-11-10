package app

import (
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// TestIdleCountsAtHighPressure checks that an idle workload stops counting
// as running only below high pressure, as in the scheduler.
func TestIdleCountsAtHighPressure(t *testing.T) {
	key := domain.ProcessKey{PID: 7, Start: time.Unix(1, 0)}
	for level, want := range map[domain.PressureLevel]int{
		domain.PressureNormal: 0, domain.PressureElevated: 0, domain.PressureHigh: 1, domain.PressureCritical: 1,
	} {
		st := StatusResult{Pressure: level, Heavy: []process.WorkloadView{{Key: key, Owner: "claude-1", Idle: true}}}
		if v := Assess(st, "claude-1"); v.RunningHeavy != want || *v.AgentRunningHeavy != want {
			t.Errorf("%v: running %d, agent running %d, want %d", level, v.RunningHeavy, *v.AgentRunningHeavy, want)
		}
		if got := busyByOwner(st)["claude-1"]; got != want {
			t.Errorf("%v: busyByOwner = %d, want %d", level, got, want)
		}
	}
}
