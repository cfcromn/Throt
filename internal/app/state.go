package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
)

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
	if err := d.writeFile(path, state); err != nil {
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
		where := "; moved to " + bad
		if rerr := d.rename(path, bad); rerr != nil {
			d.log.Warn("move aside paused state", "err", rerr)
			where = "; left in place: " + rerr.Error()
		}
		d.emit(domain.EventControlFailed, "recover paused workloads: "+errs[0].Error()+where, nil)
		return
	}
	if len(errs) > 0 {
		d.retryFailing = true
		d.emit(domain.EventControlFailed, "recover paused workloads, will keep retrying: "+errors.Join(errs...).Error(), nil)
		return
	}
	d.emit(domain.EventControlResumed, "resumed workloads left paused by a previous throtd", nil)
}

// resumeAll resumes everything on shutdown and returns how many processes
// it continued; recorded members that already exited are not counted.
// Anything that cannot be resumed stays in paused.json for the next daemon.
func (d *Daemon) resumeAll() int {
	n, errs := d.ctl.ResumeAll(context.Background())
	if len(errs) > 0 {
		d.emit(domain.EventControlFailed, "resume on shutdown, left in paused state for the next throtd: "+errors.Join(errs...).Error(), nil)
	}
	return n
}
