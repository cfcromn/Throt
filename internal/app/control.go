package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
)

// ControlParams selects a heavy workload by the PID of its top process, as
// shown in `throt status`.
type ControlParams struct {
	PID int `json:"pid"`
}

func (d *Daemon) pause(ctx context.Context, raw json.RawMessage) (any, error) {
	pid, err := controlPID(raw)
	if err != nil {
		return nil, err
	}
	key, ok := d.heavyKey(pid)
	if !ok {
		return nil, d.refused("pause", pid, "not a heavy workload in the latest sample (see `throt status`)")
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
	pid, err := controlPID(raw)
	if err != nil {
		return nil, err
	}
	var key domain.ProcessKey
	found := false
	for _, p := range d.ctl.Paused() {
		if p.Key.PID == pid {
			key, found = p.Key, true
		}
	}
	if !found {
		return nil, d.refused("resume", pid, "not paused")
	}
	// On partial failure the record stays, and each tick retries it.
	if err := d.ctl.Resume(ctx, key); err != nil {
		return nil, d.controlError("resume", pid, err)
	}
	d.emit(domain.EventControlResumed, "resumed pid "+strconv.Itoa(pid), map[string]string{"pid": strconv.Itoa(pid)})
	return nil, nil
}

func controlPID(raw json.RawMessage) (int, error) {
	var p ControlParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, fmt.Errorf("invalid params: %w", err)
	}
	if p.PID <= 1 {
		return 0, errors.New("invalid params: pid must be > 1")
	}
	return p.PID, nil
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
