package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
)

// ControlParams selects a heavy workload by the PID of its top process, as
// shown in `throt status`.
type ControlParams struct {
	PID int `json:"pid"`
	// Start, when set, must match the workload's start time, so a client
	// acting on an older view never hits a process that reused the PID.
	Start time.Time `json:"start,omitzero"`
	// Force lets resume continue a stopped process throtd did not pause
	// even when it is not an attributed heavy workload, e.g. a shell job
	// suspended with Ctrl-Z. Pause ignores it.
	Force bool `json:"force,omitempty"`
}

// pause handles the "pause" method: it stops the heavy workload the latest
// status shows at params.PID.
func (d *Daemon) pause(ctx context.Context, raw json.RawMessage) (any, error) {
	params, err := controlParams(raw)
	if err != nil {
		return nil, err
	}
	pid := params.PID
	key, ok := d.heavyKey(pid)
	if !ok {
		return nil, d.refused("pause", pid, control.ReasonNotHeavy, "not a heavy workload in the latest sample (see `throt status`)")
	}
	if !sameStart(params, key) {
		return nil, d.refused("pause", pid, ReasonStartMismatch, "the process changed since it was shown (PID reused)")
	}
	target, ok := d.workload(key)
	if !ok {
		return nil, d.refused("pause", pid, ReasonGone, "workload is gone")
	}
	if err := d.ctl.Pause(ctx, target); err != nil {
		return nil, d.controlError("pause", pid, err)
	}
	d.emit(domain.EventControlPaused, "paused "+target.Name+" ("+target.Owner+")",
		map[string]string{"action": "pause", "pid": strconv.Itoa(pid), "name": target.Name, "owner": target.Owner})
	return nil, nil
}

// resume handles the "resume" method: it continues a paused workload by the
// PID of its top process, or else an orphan (see resumeOrphan).
func (d *Daemon) resume(ctx context.Context, raw json.RawMessage) (any, error) {
	params, err := controlParams(raw)
	if err != nil {
		return nil, err
	}
	pid := params.PID
	var key domain.ProcessKey
	found := false
	for _, p := range d.ctl.Paused() {
		if p.Key.PID == pid && sameStart(params, p.Key) {
			key, found = p.Key, true
		}
	}
	if !found {
		return nil, d.resumeOrphan(ctx, params)
	}
	// On partial failure the record stays, and each tick retries it.
	if err := d.ctl.Resume(ctx, key); err != nil {
		return nil, d.controlError("resume", pid, err)
	}
	d.emit(domain.EventControlResumed, "resumed pid "+strconv.Itoa(pid), map[string]string{"action": "resume", "pid": strconv.Itoa(pid)})
	return nil, nil
}

// controlParams decodes and checks the params of pause and resume.
func controlParams(raw json.RawMessage) (ControlParams, error) {
	var p ControlParams
	if err := decodeParams(raw, &p); err != nil {
		return p, err
	}
	return p, checkPID(p.PID)
}

// sameStart reports whether a client-supplied start time (if any) matches.
func sameStart(p ControlParams, k domain.ProcessKey) bool {
	return p.Start.IsZero() || p.Start.Equal(k.Start)
}

// heavyKey resolves pid to the identity of the heavy workload shown in the
// latest status, so the user acts on exactly what they saw.
func (d *Daemon) heavyKey(pid int) (domain.ProcessKey, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.status.Heavy {
		if h.Key.PID == pid {
			return h.Key, true
		}
	}
	return domain.ProcessKey{}, false
}

// Refusal reason codes of checks the daemon makes itself; the controller's
// are in package control. Both are the reason attr of control_refused
// events, stable for the local protocol, and listed in RefusalReasons.
const (
	// ReasonStartMismatch: the PID now belongs to a process with another
	// start time than the client sent.
	ReasonStartMismatch = "start_mismatch"
	// ReasonGone: the process exited.
	ReasonGone = "gone"
	// ReasonPeerMismatch: an acquire for a PID other than the caller's own.
	ReasonPeerMismatch = "peer_mismatch"
	// ReasonUnidentified: the caller's identity or start time cannot be
	// read from the OS.
	ReasonUnidentified = "unidentified"
	// ReasonPausedMember: the process is paused as part of another
	// workload.
	ReasonPausedMember = "paused_member"
	// ReasonNeedsForce: a stopped process throtd did not pause and that is
	// not an attributed heavy workload; resume --force continues it.
	ReasonNeedsForce = "needs_force"
	// ReasonJobControl: as ReasonNeedsForce, for a job a shell stopped.
	ReasonJobControl = "job_control"
	// ReasonNotStopped: a process to resume is not stopped.
	ReasonNotStopped = "not_stopped"
)

// RefusalReasons lists every reason code of control_refused events.
var RefusalReasons = append(slices.Clone(control.Reasons),
	ReasonStartMismatch, ReasonGone, ReasonPeerMismatch, ReasonUnidentified,
	ReasonPausedMember, ReasonNeedsForce, ReasonJobControl, ReasonNotStopped)

// refused emits a control_refused event for action on pid, with a reason
// code for the event's reason attr and a sentence for people, and returns
// the error the client sees.
func (d *Daemon) refused(action string, pid int, code, sentence string) error {
	p := strconv.Itoa(pid)
	d.emit(domain.EventControlRefused, action+" pid "+p+" refused: "+sentence,
		map[string]string{"action": action, "pid": p, "reason": code})
	return fmt.Errorf("%s refused: %s", action, sentence)
}

// controlError reports a failed action on pid: a refusal by the controller
// as refused, anything else as a control_failed event.
func (d *Daemon) controlError(action string, pid int, err error) error {
	var r *control.RefusedError
	if errors.As(err, &r) {
		return d.refused(action, pid, r.Reason, control.Describe(r.Reason))
	}
	p := strconv.Itoa(pid)
	d.emit(domain.EventControlFailed, action+" pid "+p+" failed: "+err.Error(),
		map[string]string{"action": action, "pid": p})
	return fmt.Errorf("%s failed: %w", action, err)
}

// forgetExited drops recorded processes that are absent from procs and that
// the OS confirms are gone.
func (d *Daemon) forgetExited(ctx context.Context, procs []domain.ProcessSample) {
	if len(d.ctl.Paused()) == 0 {
		return
	}
	alive := make(map[domain.ProcessKey]bool, len(procs))
	for _, p := range procs {
		alive[p.Key()] = true
	}
	d.ctl.Forget(ctx, func(k domain.ProcessKey) bool { return alive[k] })
}

// retryResumes continues processes a failed resume left stopped. It emits
// one event when retries start failing and one when nothing is left, not
// one per tick. It is called only from the sampling goroutine.
func (d *Daemon) retryResumes(ctx context.Context) {
	errs := d.ctl.RetryResumes(ctx)
	resuming := slices.ContainsFunc(d.ctl.Paused(), func(p control.PausedWorkload) bool {
		return p.State == control.StateResuming
	})
	switch {
	case len(errs) > 0 && !d.retryFailing:
		d.retryFailing = true
		d.emit(domain.EventControlFailed, "resume retry failed; will keep retrying: "+errors.Join(errs...).Error(), nil)
	case !resuming && d.retryFailing:
		d.retryFailing = false
		d.emit(domain.EventControlResumed, "resumed processes left stopped by an earlier failure", nil)
	}
}
