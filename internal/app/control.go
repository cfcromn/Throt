package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	d.saveState()
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
	err = d.ctl.Resume(ctx, key)
	d.saveState() // the record is removed even on partial failure
	if err != nil {
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

// forgetExited drops paused records whose processes have all exited.
func (d *Daemon) forgetExited(procs []domain.ProcessSample) {
	before := len(d.ctl.Paused())
	if before == 0 {
		return
	}
	alive := make(map[domain.ProcessKey]bool, len(procs))
	for _, p := range procs {
		alive[p.Key()] = true
	}
	d.ctl.Forget(func(k domain.ProcessKey) bool { return alive[k] })
	if len(d.ctl.Paused()) != before {
		d.saveState()
	}
}

// statePath holds paused workloads so a daemon restarted after a crash can
// resume them. It lives in the private socket directory.
func (d *Daemon) statePath() string {
	return filepath.Join(filepath.Dir(d.cfg.SocketPath), "paused.json")
}

// saveState persists paused workloads, or removes the file when none are.
func (d *Daemon) saveState() {
	path := d.statePath()
	if len(d.ctl.Paused()) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.log.Warn("remove paused state", "err", err)
		}
		return
	}
	b, err := d.ctl.MarshalState()
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		d.log.Warn("save paused state", "err", err)
	}
}

// recoverPaused resumes anything a previous daemon left paused. Restoring
// never pauses; it only continues processes whose identity still matches.
func (d *Daemon) recoverPaused(ctx context.Context) {
	b, err := os.ReadFile(d.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		errs := d.ctl.RestoreAndResume(ctx, b)
		err = errors.Join(errs...)
		d.emit(domain.EventControlResumed, "resumed workloads left paused by a previous throtd", nil)
	}
	if err != nil {
		d.emit(domain.EventControlFailed, "recover paused workloads: "+err.Error(), nil)
	}
	os.Remove(d.statePath())
}

func (d *Daemon) resumeAll() {
	if errs := d.ctl.ResumeAll(context.Background()); len(errs) > 0 {
		d.emit(domain.EventControlFailed, "resume on shutdown: "+errors.Join(errs...).Error(), nil)
	}
	d.saveState()
}
