package app

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// SchedulerStatus is the scheduler part of StatusResult.
type SchedulerStatus struct {
	Mode       scheduler.Mode `json:"mode"`
	HeavyLimit int            `json:"heavy_limit"`
	// Queued lists workloads the scheduler paused (queued or preempted).
	Queued []domain.ProcessKey `json:"queued"`
}

func newScheduler(c config.Scheduler) *scheduler.Scheduler {
	return scheduler.New(scheduler.Policy{
		Mode:            scheduler.Mode(c.Mode),
		HeavyLimit:      c.HeavyLimit,
		ResumeCooldown:  time.Duration(c.ResumeCooldown),
		PauseOnCritical: c.PauseOnCritical,
		MaxQueueWait:    time.Duration(c.MaxQueueWait),
	})
}

// schedule runs one scheduler round on the sampling goroutine and, in
// enforce mode, executes its decisions through the verified controller.
// Every decision is an event, so nothing the scheduler does is hidden.
func (d *Daemon) schedule(parent context.Context, now time.Time, level domain.PressureLevel, heavy []process.WorkloadView) {
	// Split controller records into ours (queued) and manual pauses (held).
	// A queued workload the user resumed by hand simply drops out of ours.
	var ours, held []domain.ProcessKey
	still := make(map[domain.ProcessKey]bool, len(d.queued))
	for _, p := range d.ctl.Paused() {
		if d.queued[p.Key] {
			ours = append(ours, p.Key)
			still[p.Key] = true
		} else {
			held = append(held, p.Key)
		}
	}
	d.queued = still

	decisions := d.sched.Decide(scheduler.Input{Now: now, Pressure: level, Heavy: heavy, Paused: ours, Held: held})
	if len(decisions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	for _, dec := range decisions {
		d.emitDecision(dec)
		if dec.Observed {
			continue
		}
		var err error
		switch dec.Action {
		case scheduler.ActionQueue, scheduler.ActionPreempt:
			d.graphMu.Lock()
			target, ok := d.graph.Workload(dec.Key)
			d.graphMu.Unlock()
			if !ok {
				continue // exited since the snapshot
			}
			if err = d.ctl.Pause(ctx, target); err == nil {
				d.queued[dec.Key] = true
			}
		case scheduler.ActionResume:
			err = d.ctl.Resume(ctx, dec.Key)
			delete(d.queued, dec.Key) // a partial resume is retried by the controller
		}
		if err != nil {
			d.emit(domain.EventControlFailed, fmt.Sprintf("scheduler %s %s failed: %v", dec.Action, dec.Name, err),
				map[string]string{"pid": strconv.Itoa(dec.Key.PID), "action": string(dec.Action)})
		}
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
	queued := make([]domain.ProcessKey, 0, len(d.queued))
	for k := range d.queued {
		queued = append(queued, k)
	}
	p := d.sched.Policy()
	return SchedulerStatus{Mode: p.Mode, HeavyLimit: p.HeavyLimit, Queued: queued}
}
