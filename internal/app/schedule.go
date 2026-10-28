package app

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// SchedulerStatus is the scheduler part of StatusResult.
type SchedulerStatus struct {
	Mode       scheduler.Mode `json:"mode"`
	HeavyLimit int            `json:"heavy_limit"`
	// PerAgentLimit caps heavy workloads per agent; 0 means no cap.
	PerAgentLimit int `json:"per_agent_limit"`
	// Queued lists workloads the scheduler paused (queued or preempted).
	Queued []domain.ProcessKey `json:"queued"`
	// Leases lists the active admission leases: heavy slots reserved for
	// `throt run` commands that are not heavy workloads yet. They count as
	// running heavy work.
	Leases []Lease `json:"leases"`
}

func newScheduler(c config.Scheduler) *scheduler.Scheduler {
	return scheduler.New(schedulerPolicy(c))
}

func schedulerPolicy(c config.Scheduler) scheduler.Policy {
	return scheduler.Policy{
		Mode:            scheduler.Mode(c.Mode),
		HeavyLimit:      c.HeavyLimit,
		ResumeCooldown:  time.Duration(c.ResumeCooldown),
		PauseOnCritical: c.PauseOnCritical,
		MaxQueueWait:    time.Duration(c.MaxQueueWait),
		Adaptive:        c.Adaptive,
		PerOwnerLimit:   c.PerAgentLimit,
	}
}

// maxQueueMembers bounds the workloads the scheduler will queue, so one
// round's verified signals fit in its deadline.
const maxQueueMembers = 256

// schedule runs one scheduler round on the sampling goroutine and, in
// enforce mode, executes its decisions through the verified controller.
// Every decision is an event, so nothing the scheduler does is hidden.
// reserved holds the agent of each active admission lease, and admitted the
// workloads that took over a lease this round. Orphans (stopped processes
// no record explains) are held like manual pauses: the scheduler must never
// queue one and later resume it, since Throt only reports orphans.
func (d *Daemon) schedule(parent context.Context, now time.Time, level domain.PressureLevel, heavy []process.WorkloadView, reserved []string, admitted []domain.ProcessKey, waiters []scheduler.Waiter, orphans []domain.ProcessKey) {
	// The controller records who paused what, so a manual hold is never
	// mistaken for scheduler queueing, even across restarts.
	inSnapshot := make(map[domain.ProcessKey]bool, len(heavy))
	for _, h := range heavy {
		inSnapshot[h.Key] = true
	}
	var ours []domain.ProcessKey
	held := orphans
	for _, p := range d.ctl.Paused() {
		switch {
		case p.By != control.OriginScheduler:
			held = append(held, p.Key)
		case !inSnapshot[p.Key]:
			// The top process exited (e.g. killed while stopped); its
			// children would otherwise stay stopped with nobody to resume them.
			d.schedulerResume(parent, p.Key, p.Name, p.Owner, "top_exited")
		default:
			ours = append(ours, p.Key)
		}
	}

	decisions := d.sched.Decide(scheduler.Input{Now: now, Pressure: level, Heavy: heavy, Paused: ours, Held: held, Reserved: reserved, Admitted: admitted, Waiters: waiters})
	for _, c := range d.sched.Changes() {
		d.emit(domain.EventSchedulerLimit, fmt.Sprintf("heavy limit %d -> %d: %s", c.From, c.To, c.Reason),
			map[string]string{"from": strconv.Itoa(c.From), "to": strconv.Itoa(c.To), "reason": c.Reason})
	}
	for _, dec := range decisions {
		d.emitDecision(dec)
		if dec.Observed {
			continue
		}
		switch dec.Action {
		case scheduler.ActionQueue, scheduler.ActionPreempt:
			d.schedulerPause(parent, dec)
		case scheduler.ActionResume:
			d.schedulerResume(parent, dec.Key, dec.Name, dec.Owner, "")
		}
	}
}

func (d *Daemon) schedulerPause(parent context.Context, dec scheduler.Decision) {
	d.graphMu.Lock()
	target, ok := d.graph.Workload(dec.Key)
	d.graphMu.Unlock()
	if !ok {
		return // exited since the snapshot
	}
	if len(target.Members) > maxQueueMembers {
		d.emit(domain.EventControlRefused, fmt.Sprintf("scheduler %s %s refused: %d processes exceeds %d", dec.Action, dec.Name, len(target.Members), maxQueueMembers),
			map[string]string{"pid": strconv.Itoa(dec.Key.PID), "action": string(dec.Action), "reason": "too_many_members"})
		return
	}
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	if err := d.ctl.PauseAs(ctx, target, control.OriginScheduler); err != nil {
		d.emit(domain.EventControlFailed, fmt.Sprintf("scheduler %s %s failed: %v", dec.Action, dec.Name, err),
			map[string]string{"pid": strconv.Itoa(dec.Key.PID), "action": string(dec.Action)})
	}
}

// schedulerResume resumes a scheduler-paused workload. A non-empty reason
// means the daemon decided it outside Decide, so it gets its own event.
func (d *Daemon) schedulerResume(parent context.Context, key domain.ProcessKey, name, owner, reason string) {
	if reason != "" {
		d.emit(domain.EventSchedulerDecision, fmt.Sprintf("resume %s (%s): %s", name, owner, reason),
			map[string]string{"decision": "resume", "pid": strconv.Itoa(key.PID), "name": name, "owner": owner, "reason": reason, "observed": "false"})
	}
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	// A partial resume stays recorded and is retried by the controller.
	if err := d.ctl.Resume(ctx, key); err != nil {
		d.emit(domain.EventControlFailed, fmt.Sprintf("scheduler resume %s failed: %v", name, err),
			map[string]string{"pid": strconv.Itoa(key.PID), "action": "resume"})
	}
}

func (d *Daemon) emitDecision(dec scheduler.Decision) {
	prefix := ""
	if dec.Observed {
		prefix = "[observe] "
	}
	owner := dec.Owner
	msg := fmt.Sprintf("%s%s %s (%s): %s, %d/%d heavy running, pressure %s",
		prefix, dec.Action, dec.Name, owner, dec.Reason, dec.RunningHeavy, dec.HeavyLimit, dec.Pressure)
	d.emit(domain.EventSchedulerDecision, msg, map[string]string{
		"decision": string(dec.Action),
		"pid":      strconv.Itoa(dec.Key.PID),
		"name":     dec.Name,
		"owner":    owner,
		"reason":   dec.Reason,
		"pressure": dec.Pressure.String(),
		"running":  strconv.Itoa(dec.RunningHeavy),
		"limit":    strconv.Itoa(dec.HeavyLimit),
		"observed": strconv.FormatBool(dec.Observed),
	})
}

func (d *Daemon) schedulerStatus() SchedulerStatus {
	queued := []domain.ProcessKey{}
	for _, p := range d.ctl.Paused() {
		if p.By == control.OriginScheduler {
			queued = append(queued, p.Key)
		}
	}
	p := d.sched.Policy()
	return SchedulerStatus{Mode: p.Mode, HeavyLimit: d.sched.Limit(), PerAgentLimit: p.PerOwnerLimit, Queued: queued, Leases: []Lease{}}
}
