package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// Prioritizer is the platform's priority capability. Like
// control.Signaler, each call verifies that key still names the same
// process (PID and start time) owned by uid and acts on it in one call; it
// returns an error wrapping domain.ErrProcessGone, and changes nothing,
// when the process is gone or its identity does not match.
type Prioritizer interface {
	// Background lowers key's CPU and I/O priority and returns an opaque
	// token for Foreground.
	Background(ctx context.Context, key domain.ProcessKey, uid uint32) (token uint64, err error)
	// Foreground undoes Background, as far as the OS lets the user.
	Foreground(ctx context.Context, key domain.ProcessKey, uid uint32, token uint64) error
}

const (
	// maxLowered bounds how many processes throtd keeps lowered.
	maxLowered = 4096
	// maxPriorityCalls bounds the Background calls of one tick; the rest
	// wait for the next.
	maxPriorityCalls = 256
)

// Reasons of priority_restored events.
const (
	RestoreDisabled        = "disabled"
	RestoreEnforceDisabled = ReasonEnforceDisabled
	RestoreShutdown        = "shutdown"
)

// loweredWorkload is one workload whose members throtd lowered.
type loweredWorkload struct {
	name, owner string
	// members maps each lowered process to its Foreground token.
	members map[domain.ProcessKey]uint64
}

// priorities is what throtd lowered. It belongs to the sampling goroutine
// (and to Run once that goroutine has stopped).
type priorities struct {
	workloads map[domain.ProcessKey]*loweredWorkload  // by top process
	owner     map[domain.ProcessKey]domain.ProcessKey // member -> top process
	// failed holds members whose Background failed; they are not retried
	// while they live.
	failed map[domain.ProcessKey]bool
}

func newPriorities() priorities {
	return priorities{
		workloads: map[domain.ProcessKey]*loweredWorkload{},
		owner:     map[domain.ProcessKey]domain.ProcessKey{},
		failed:    map[domain.ProcessKey]bool{},
	}
}

// backgroundActive reports whether heavy workloads should run in the
// background now: in enforce mode with scheduler.background on, on a
// platform that can lower priority.
func (d *Daemon) backgroundActive() bool {
	return d.pri != nil && d.bgEnabled.Load() && d.sched.Policy().Mode == scheduler.ModeEnforce
}

// background runs on the sampling goroutine after scheduling. In enforce
// mode with scheduler.background on, it lowers every member of every
// attributed heavy workload not lowered yet; otherwise it restores what it
// lowered. r is the round of a successful process sample, or nil.
// Lowering never touches an unattributed workload, an agent, a process of
// another user, a manually paused workload or an orphan, and every call
// verifies identity. Failures are events; nothing here affects scheduling.
func (d *Daemon) background(parent context.Context, procs []domain.ProcessSample, r *processRound) {
	if !d.backgroundActive() {
		if len(d.prio.workloads) > 0 {
			reason := RestoreDisabled
			if d.sched.Policy().Mode != scheduler.ModeEnforce {
				reason = RestoreEnforceDisabled
			}
			d.restorePriorities(parent, reason)
		}
		return
	}
	if r == nil {
		return // no fresh snapshot to act on
	}
	d.prunePriorities(procs)

	held := map[domain.ProcessKey]bool{}
	for _, k := range r.stoppedOrphans {
		held[k] = true
	}
	for _, p := range d.ctl.Paused() {
		if p.By != control.OriginScheduler {
			held[p.Key] = true
			for _, m := range p.Members {
				held[m] = true
			}
		}
	}
	for _, a := range r.snap.Agents {
		held[a.Key] = true
	}

	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	calls := 0
	for _, h := range r.snap.Heavy {
		if h.Owner == "" || held[h.Key] {
			continue
		}
		if calls >= maxPriorityCalls || len(d.prio.owner) >= maxLowered || ctx.Err() != nil {
			break
		}
		t, ok := d.workload(h.Key)
		if !ok || t.Owner == "" || !t.Class.Heavy() {
			continue
		}
		calls += d.lowerWorkload(ctx, t, held, maxPriorityCalls-calls)
	}
	d.markBackground()
}

// lowerWorkload lowers t's members that are not lowered yet, at most
// budget of them, and returns how many calls it made.
func (d *Daemon) lowerWorkload(ctx context.Context, t process.WorkloadTarget, held map[domain.ProcessKey]bool, budget int) int {
	w := d.prio.workloads[t.Key]
	calls, lowered := 0, 0
	var errs []error
	for _, m := range t.Members {
		if calls >= budget || len(d.prio.owner) >= maxLowered || ctx.Err() != nil {
			break
		}
		if _, done := d.prio.owner[m.Key]; done || d.prio.failed[m.Key] || held[m.Key] ||
			m.UID != d.uid || m.Key.PID <= 1 || m.Key.PID == os.Getpid() {
			continue
		}
		calls++
		token, err := d.pri.Background(ctx, m.Key, d.uid)
		switch {
		case errors.Is(err, domain.ErrProcessGone):
			d.prio.failed[m.Key] = true
			continue
		case err != nil:
			d.prio.failed[m.Key] = true
			errs = append(errs, err)
			continue
		}
		if w == nil {
			w = &loweredWorkload{name: t.Name, owner: t.Owner, members: map[domain.ProcessKey]uint64{}}
			d.prio.workloads[t.Key] = w
		}
		w.members[m.Key] = token
		d.prio.owner[m.Key] = t.Key
		lowered++
	}
	pid := strconv.Itoa(t.Key.PID)
	if lowered > 0 && lowered == len(w.members) {
		// The workload's first lowering; members found later are lowered
		// quietly, and priority_restored counts them all.
		d.emit(domain.EventPriorityLowered, fmt.Sprintf("background priority for %s (%s), %d processes", t.Name, t.Owner, lowered),
			map[string]string{"pid": pid, "name": t.Name, "owner": t.Owner, "processes": strconv.Itoa(lowered)})
	}
	if len(errs) > 0 {
		d.emit(domain.EventControlFailed, fmt.Sprintf("background priority for %s failed, it keeps running: %v", t.Name, errors.Join(errs...)),
			map[string]string{"pid": pid, "action": "background"})
	}
	return calls
}

// prunePriorities forgets lowered and failed processes that are no longer
// in the process sample.
func (d *Daemon) prunePriorities(procs []domain.ProcessSample) {
	if len(d.prio.owner) == 0 && len(d.prio.failed) == 0 {
		return
	}
	alive := make(map[domain.ProcessKey]bool, len(procs))
	for _, p := range procs {
		alive[p.Key()] = true
	}
	for m, top := range d.prio.owner {
		if !alive[m] {
			delete(d.prio.owner, m)
			w := d.prio.workloads[top]
			delete(w.members, m)
			if len(w.members) == 0 {
				delete(d.prio.workloads, top)
			}
		}
	}
	for m := range d.prio.failed {
		if !alive[m] {
			delete(d.prio.failed, m)
		}
	}
}

// restorePriorities restores every process throtd lowered, one
// priority_restored event per workload. It runs to completion even if ctx
// is cancelled (shutdown), each call bounded by its own deadline. A process
// left lowered by a failure keeps running; low priority is harmless.
func (d *Daemon) restorePriorities(ctx context.Context, reason string) {
	tops := make([]domain.ProcessKey, 0, len(d.prio.workloads))
	for k := range d.prio.workloads {
		tops = append(tops, k)
	}
	slices.SortFunc(tops, func(a, b domain.ProcessKey) int { return a.PID - b.PID })
	for _, top := range tops {
		w := d.prio.workloads[top]
		restored := 0
		var errs []error
		for m, token := range w.members {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), control.CallTimeout)
			err := d.pri.Foreground(cctx, m, d.uid, token)
			cancel()
			switch {
			case err == nil:
				restored++
			case !errors.Is(err, domain.ErrProcessGone):
				errs = append(errs, err)
			}
		}
		pid := strconv.Itoa(top.PID)
		d.emit(domain.EventPriorityRestored, fmt.Sprintf("normal priority for %s (%s), %d processes: %s", w.name, w.owner, restored, reason),
			map[string]string{"pid": pid, "name": w.name, "owner": w.owner, "processes": strconv.Itoa(restored), "reason": reason})
		if len(errs) > 0 {
			d.emit(domain.EventControlFailed, fmt.Sprintf("restoring priority for %s failed, it stays in the background: %v", w.name, errors.Join(errs...)),
				map[string]string{"pid": pid, "action": "foreground"})
		}
	}
	d.prio = newPriorities()
	d.markBackground()
}

// markBackground sets Background on the status's heavy workloads that
// have lowered members. It replaces the slice: status readers may hold the
// old one.
func (d *Daemon) markBackground() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.Heavy = d.withBackground(d.status.Heavy)
}

// withBackground returns heavy with Background set from what throtd
// lowered, sharing heavy when nothing changes.
func (d *Daemon) withBackground(heavy []process.WorkloadView) []process.WorkloadView {
	changed := false
	for _, h := range heavy {
		if h.Background != (d.prio.workloads[h.Key] != nil) {
			changed = true
			break
		}
	}
	if !changed {
		return heavy
	}
	out := slices.Clone(heavy)
	for i := range out {
		out[i].Background = d.prio.workloads[out[i].Key] != nil
	}
	return out
}
