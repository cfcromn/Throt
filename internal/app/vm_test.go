package app

import (
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// TestBusyVMTakesOneSlot checks that busy VMs together take one heavy slot
// and idle ones none.
func TestBusyVMTakesOneSlot(t *testing.T) {
	running := process.WorkloadView{Key: domain.ProcessKey{PID: 11}, Name: "go", Owner: "claude-1", OwnerPID: 10}
	vm := func(pid int, cpu float64) process.VMView {
		return process.VMView{Key: domain.ProcessKey{PID: pid}, Name: "com.apple.Virtualization.VirtualMachine", CPU: cpu}
	}
	st := oneSlot(3, running)

	st.VMs = []process.VMView{vm(20, 0.2)}
	if v := Assess(st, "claude-1"); !v.OKToStart {
		t.Fatalf("idle VM refused: %+v", v)
	}
	st.VMs = []process.VMView{vm(20, 4), vm(21, 2)}
	if v := Assess(st, "claude-1"); !v.OKToStart || !strings.Contains(v.Reason, "2 of 3 heavy slots") {
		t.Fatalf("two busy VMs took more than one slot: %+v", v)
	}
	st.Scheduler.HeavyLimit = 2
	v := Assess(st, "claude-1")
	if v.OKToStart || !strings.Contains(v.Reason, "taken by a busy VM") {
		t.Fatalf("busy VM did not take a slot: %+v", v)
	}
	if got := vmSlots(st.VMs); len(got) != 1 || got[0] != "" {
		t.Fatalf("vmSlots = %q", got)
	}
}
