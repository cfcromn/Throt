package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// SchedulerStatus is the scheduler part of StatusResult.
type SchedulerStatus struct {
	Mode scheduler.Mode `json:"mode"`
	// HeavyLimit is the effective limit; with Adaptive it moves between 1
	// and MaxLimit.
	HeavyLimit int `json:"heavy_limit"`
	// Adaptive, StartLimit and MaxLimit (the configured heavy_limit) are set
	// only for an adaptive scheduler.
	Adaptive   bool `json:"adaptive,omitempty"`
	StartLimit int  `json:"start_limit,omitempty"`
	MaxLimit   int  `json:"max_limit,omitempty"`
	// PerAgentLimit caps heavy workloads per agent; 0 means no cap.
	PerAgentLimit int `json:"per_agent_limit"`
	// Queued lists workloads the scheduler paused (queued or preempted).
	Queued []domain.ProcessKey `json:"queued"`
	// WouldQueue lists, in observe mode, the workloads the latest round
	// would have queued; they keep running. Empty in the other modes.
	WouldQueue []domain.ProcessKey `json:"would_queue"`
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
		Mode:       scheduler.Mode(c.Mode),
		HeavyLimit: c.HeavyLimit,
		StartLimit: c.StartLimit,
		// Without memory evidence the limit grows no further than one
		// workload per CPU; see scheduler.Policy.BlindLimit.
		BlindLimit:      max(2, runtime.NumCPU()),
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
// in carries everything but Paused and Held, which schedule fills from the
// controller's records. Orphans (stopped processes no record explains) are
// held like manual pauses: the scheduler must never queue one and later
// resume it, since Throt only reports orphans.
func (d *Daemon) schedule(parent context.Context, in scheduler.Input, orphans []domain.ProcessKey) {
	// The controller records who paused what, so a manual hold is never
	// mistaken for scheduler queueing, even across restarts.
	inSnapshot := make(map[domain.ProcessKey]bool, len(in.Heavy))
	for _, h := range in.Heavy {
		inSnapshot[h.Key] = true
	}
	in.Held = orphans
	for _, p := range d.ctl.Paused() {
		switch {
		case p.By != control.OriginScheduler:
			in.Held = append(in.Held, p.Key)
		case !inSnapshot[p.Key]:
			// The top process exited (e.g. killed while stopped); its
			// children would otherwise stay stopped with nobody to resume them.
			d.schedulerResume(parent, p.Key, p.Name, p.Owner, ReasonTopExited)
		default:
			in.Paused = append(in.Paused, p.Key)
		}
	}

	decisions := d.sched.Decide(in)
	d.wouldQueue = d.wouldQueue[:0]
	for _, c := range d.sched.Changes() {
		d.emit(domain.EventSchedulerLimit, fmt.Sprintf("heavy limit %d -> %d: %s", c.From, c.To, c.Reason),
			map[string]string{"from": strconv.Itoa(c.From), "to": strconv.Itoa(c.To), "reason": c.Reason})
	}
	for _, dec := range decisions {
		d.emitDecision(dec)
		if dec.Observed {
			if dec.Action == scheduler.ActionQueue {
				d.wouldQueue = append(d.wouldQueue, dec.Key)
			}
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

// schedulerPause queues or preempts the workload of a decision.
func (d *Daemon) schedulerPause(parent context.Context, dec scheduler.Decision) {
	target, ok := d.workload(dec.Key)
	if !ok {
		return // exited since the snapshot
	}
	if len(target.Members) > maxQueueMembers {
		d.emit(domain.EventControlRefused, fmt.Sprintf("scheduler %s %s refused: %d processes exceeds %d", dec.Action, dec.Name, len(target.Members), maxQueueMembers),
			map[string]string{"pid": strconv.Itoa(dec.Key.PID), "action": string(dec.Action), "reason": control.ReasonTooManyMembers})
		return
	}
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	err := d.ctl.PauseAs(ctx, target, control.OriginScheduler)
	if err == nil {
		return
	}
	if errors.Is(err, domain.ErrProcessGone) && !d.recorded(dec.Key) {
		// The top process exited between the snapshot and the pause, and
		// the rollback was complete: like a workload that exited before
		// the round, there is nothing to queue and nothing failed.
		return
	}
	d.emit(domain.EventControlFailed, fmt.Sprintf("scheduler %s %s failed: %v", dec.Action, dec.Name, err),
		map[string]string{"pid": strconv.Itoa(dec.Key.PID), "action": string(dec.Action)})
}

// recorded reports whether the controller holds a record for key.
func (d *Daemon) recorded(key domain.ProcessKey) bool {
	return slices.ContainsFunc(d.ctl.Paused(), func(w control.PausedWorkload) bool { return w.Key == key })
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

// emitDecision emits a scheduler decision, prefixed with [observe] when
// it was only observed.
func (d *Daemon) emitDecision(dec scheduler.Decision) {
	prefix := ""
	if dec.Observed {
		prefix = "[observe] "
	}
	msg := fmt.Sprintf("%s%s %s (%s): %s, %d/%d heavy running, pressure %s",
		prefix, dec.Action, dec.Name, dec.Owner, dec.Reason, dec.RunningHeavy, dec.HeavyLimit, dec.Pressure)
	d.emit(domain.EventSchedulerDecision, msg, map[string]string{
		"decision": string(dec.Action),
		"pid":      strconv.Itoa(dec.Key.PID),
		"name":     dec.Name,
		"owner":    dec.Owner,
		"reason":   dec.Reason,
		"pressure": dec.Pressure.String(),
		"running":  strconv.Itoa(dec.RunningHeavy),
		"limit":    strconv.Itoa(dec.HeavyLimit),
		"observed": strconv.FormatBool(dec.Observed),
	})
}

// schedulerQueued returns the workloads the scheduler paused, never nil.
func (d *Daemon) schedulerQueued() []domain.ProcessKey {
	queued := []domain.ProcessKey{}
	for _, p := range d.ctl.Paused() {
		if p.By == control.OriginScheduler {
			queued = append(queued, p.Key)
		}
	}
	return queued
}

// schedulerStatus returns the scheduler part of the status, with Leases
// empty: the status handlers fill them in under d.mu.
func (d *Daemon) schedulerStatus() SchedulerStatus {
	p := d.sched.Policy()
	st := SchedulerStatus{
		Mode: p.Mode, HeavyLimit: d.sched.Limit(), PerAgentLimit: p.PerOwnerLimit,
		Queued: d.schedulerQueued(), WouldQueue: []domain.ProcessKey{}, Leases: []Lease{},
	}
	if p.Mode == scheduler.ModeObserve {
		st.WouldQueue = append(st.WouldQueue, d.wouldQueue...)
	}
	if p.Adaptive {
		st.Adaptive, st.StartLimit, st.MaxLimit = true, p.StartLimit, p.HeavyLimit
	}
	return st
}
