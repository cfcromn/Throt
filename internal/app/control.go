package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
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

func (d *Daemon) pause(ctx context.Context, raw json.RawMessage) (any, error) {
	params, err := controlParams(raw)
	if err != nil {
		return nil, err
	}
	pid := params.PID
	key, ok := d.heavyKey(pid)
	if !ok {
		return nil, d.refused("pause", pid, "not a heavy workload in the latest sample (see `throt status`)")
	}
	if !sameStart(params, key) {
		return nil, d.refused("pause", pid, "the process changed since it was shown (PID reused)")
	}
	d.graphMu.Lock()
	target, ok := d.graph.Workload(key)
	d.graphMu.Unlock()
	if !ok {
		return nil, d.refused("pause", pid, "workload is gone")
	}
	if err := d.ctl.Pause(ctx, target); err != nil {
		return nil, d.controlError("pause", pid, err)
	}
	d.emit(domain.EventControlPaused, "paused "+target.Name+" ("+target.Owner+")",
		map[string]string{"pid": strconv.Itoa(pid), "name": target.Name, "owner": target.Owner})
	return nil, nil
}

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
	d.emit(domain.EventControlResumed, "resumed pid "+strconv.Itoa(pid), map[string]string{"pid": strconv.Itoa(pid)})
	return nil, nil
}

// resumeOrphan continues a stopped process that no paused record explains,
// e.g. one an earlier throtd paused before it was killed hard. Without
// Force only an attributed heavy workload qualifies, the kind throtd
// pauses; with Force any orphan of the user does. For a workload's top
// process, its stopped unrecorded members are continued too, children
// first. Every signal verifies identity (PID, start, uid) first.
func (d *Daemon) resumeOrphan(ctx context.Context, params ControlParams) error {
	pid := params.PID
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	i := slices.IndexFunc(stopped, func(v process.StoppedView) bool { return v.Key.PID == pid && sameStart(params, v.Key) })
	if i < 0 {
		return d.refused("resume", pid, "not paused, and not stopped in the latest sample (see `throt doctor`)")
	}
	v := stopped[i]
	recorded := recordedKeys(d.ctl.Paused())
	switch {
	case v.UID != d.uid:
		return d.refused("resume", pid, "owned by another user")
	case recorded[v.Key]:
		return d.refused("resume", pid, "paused by throtd as part of another workload; resume that workload")
	}
	o := process.ClassifyOrphan(v)
	if !o.AttributedHeavy() && !params.Force {
		reason := "not an attributed heavy workload, so throtd did not pause it; use kill -CONT, or throt resume --force"
		if o.Kind == process.OrphanJobControl {
			reason = "likely a shell job stopped with Ctrl-Z; use fg in its shell, or throt resume --force"
		}
		return d.refused("resume", pid, reason)
	}

	targets := []domain.ProcessKey{v.Key}
	if o.AttributedHeavy() {
		d.graphMu.Lock()
		w, ok := d.graph.Workload(v.Key)
		d.graphMu.Unlock()
		if ok {
			orphan := make(map[domain.ProcessKey]bool, len(stopped))
			for _, s := range stopped {
				orphan[s.Key] = s.UID == d.uid && !recorded[s.Key]
			}
			targets = targets[:0]
			for _, m := range slices.Backward(w.Members) {
				if orphan[m.Key] && m.UID == d.uid {
					targets = append(targets, m.Key)
				}
			}
		}
	}
	var errs []error
	resumed := 0
	for _, k := range targets {
		cctx, cancel := context.WithTimeout(ctx, control.CallTimeout)
		err := d.sig.Continue(cctx, k, d.uid)
		cancel()
		switch {
		case errors.Is(err, domain.ErrProcessGone):
		case err != nil:
			errs = append(errs, err)
		default:
			resumed++
		}
	}
	if len(errs) > 0 {
		return d.controlError("resume", pid, errors.Join(errs...))
	}
	if resumed == 0 {
		return d.refused("resume", pid, "the process is gone")
	}
	attrs := map[string]string{"pid": strconv.Itoa(pid), "name": v.Name, "orphan": "true", "processes": strconv.Itoa(resumed)}
	if v.Owner != "" {
		attrs["owner"] = v.Owner
	}
	if params.Force {
		attrs["force"] = "true"
	}
	d.emit(domain.EventControlResumed, "resumed stopped pid "+strconv.Itoa(pid)+" ("+v.Name+") that throtd did not pause", attrs)
	return nil
}

// recordedKeys returns every process in the paused records, members
// included.
func recordedKeys(lists ...[]control.PausedWorkload) map[domain.ProcessKey]bool {
	out := map[domain.ProcessKey]bool{}
	for _, l := range lists {
		for _, p := range l {
			out[p.Key] = true
			for _, m := range p.Members {
				out[m] = true
			}
		}
	}
	return out
}

// reportOrphans emits one orphan_stopped event per tick that finds
// attributed heavy orphans not yet reported. An orphan must have been
// stopped in an earlier sample too, so a resume racing the sample is not
// reported. Shell jobs and other orphans are only listed in status.
func (d *Daemon) reportOrphans(now time.Time, orphans []process.Orphan) {
	current := make(map[domain.ProcessKey]bool, len(orphans))
	var fresh []process.Orphan
	for _, o := range orphans {
		if !o.AttributedHeavy() || o.Kind != process.OrphanLeftPaused || !o.Since.Before(now) {
			continue
		}
		current[o.Key] = true
		if !d.reported[o.Key] {
			fresh = append(fresh, o)
		}
	}
	maps.DeleteFunc(d.reported, func(k domain.ProcessKey, _ bool) bool { return !current[k] })
	if len(fresh) == 0 {
		return
	}
	var names, pids []string
	for _, o := range fresh {
		d.reported[o.Key] = true
		names = append(names, o.Name+" (pid "+strconv.Itoa(o.Key.PID)+", "+o.Owner+")")
		pids = append(pids, strconv.Itoa(o.Key.PID))
	}
	d.emit(domain.EventOrphanStopped, "stopped, not paused by throtd: "+strings.Join(names, ", ")+"; see `throt doctor`",
		map[string]string{"count": strconv.Itoa(len(fresh)), "pids": strings.Join(pids, ",")})
}

func controlParams(raw json.RawMessage) (ControlParams, error) {
	var p ControlParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("invalid params: %w", err)
	}
	if p.PID <= 1 {
		return p, errors.New("invalid params: pid must be > 1")
	}
	return p, nil
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

func (d *Daemon) refused(action string, pid int, reason string) error {
	d.emit(domain.EventControlRefused, action+" pid "+strconv.Itoa(pid)+" refused: "+reason,
		map[string]string{"action": action, "pid": strconv.Itoa(pid), "reason": reason})
	return fmt.Errorf("%s refused: %s", action, reason)
}

func (d *Daemon) controlError(action string, pid int, err error) error {
	var r *control.RefusedError
	if errors.As(err, &r) {
		return d.refused(action, pid, r.Reason)
	}
	d.emit(domain.EventControlFailed, action+" pid "+strconv.Itoa(pid)+" failed: "+err.Error(),
		map[string]string{"action": action, "pid": strconv.Itoa(pid)})
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

// statePath holds paused workloads so a daemon restarted after a crash can
// resume them. It lives in the private socket directory.
func (d *Daemon) statePath() string {
	return filepath.Join(filepath.Dir(d.cfg.SocketPath), "paused.json")
}

// saveState is the controller's onChange hook: it persists the records
// before the controller signals anything that depends on them, or removes
// the file when state is nil (no records). It runs under the controller's
// lock and must not call back into the controller.
func (d *Daemon) saveState(state []byte) error {
	path := d.statePath()
	if state == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.log.Warn("remove paused state", "err", err)
			return err
		}
		return nil
	}
	if err := writeFileAtomic(path, state); err != nil {
		d.log.Warn("save paused state", "err", err)
		return err
	}
	return nil
}

// writeFileAtomic replaces path with data so readers see either the old or
// the new contents, and the new contents survive a crash once it returns.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if df, derr := os.Open(dir); derr == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

// recoverPaused resumes anything a previous daemon left paused. Restoring
// never pauses; it only continues processes whose identity still matches.
// Restored records that cannot be resumed stay in paused.json (rewritten by
// the controller) and are retried every tick. A state file that cannot be
// parsed is moved aside for inspection rather than deleted.
func (d *Daemon) recoverPaused(ctx context.Context) {
	path := d.statePath()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		d.emit(domain.EventControlFailed, "recover paused workloads: "+err.Error(), nil)
		return
	}
	errs := d.ctl.RestoreAndResume(ctx, b)
	if len(errs) == 1 && errors.Is(errs[0], control.ErrMalformedState) {
		bad := path + ".invalid"
		if rerr := os.Rename(path, bad); rerr != nil {
			d.log.Warn("move aside paused state", "err", rerr)
		}
		d.emit(domain.EventControlFailed, "recover paused workloads: "+errs[0].Error()+"; moved to "+bad, nil)
		return
	}
	if len(errs) > 0 {
		d.retryFailing = true
		d.emit(domain.EventControlFailed, "recover paused workloads, will keep retrying: "+errors.Join(errs...).Error(), nil)
		return
	}
	d.emit(domain.EventControlResumed, "resumed workloads left paused by a previous throtd", nil)
}

// resumeAll resumes everything on shutdown. Anything that cannot be resumed
// stays in paused.json for the next daemon.
func (d *Daemon) resumeAll() {
	if errs := d.ctl.ResumeAll(context.Background()); len(errs) > 0 {
		d.emit(domain.EventControlFailed, "resume on shutdown, left in paused state for the next throtd: "+errors.Join(errs...).Error(), nil)
	}
}
