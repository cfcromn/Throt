package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

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
	o := process.ClassifyOrphan(v, stopped)
	// A shell job is never resumed without --force, even when it is an
	// agent's heavy workload: its job stays stopped in the shell.
	leftPaused := o.AttributedHeavy() && o.Kind == process.OrphanLeftPaused
	if !leftPaused && !params.Force {
		reason := "not an attributed heavy workload, so throtd did not pause it; use kill -CONT, or throt resume --force"
		if o.Kind == process.OrphanJobControl {
			reason = "likely a shell job stopped with Ctrl-Z; use fg in its shell, or throt resume --force"
		}
		return d.refused("resume", pid, reason)
	}

	// The snapshot can be up to a sample old: an orphan resumed since then
	// must not be reported as resumed again.
	if reason := d.notStopped(ctx, v.Key); reason != "" {
		return d.refused("resume", pid, reason)
	}
	targets := []domain.ProcessKey{v.Key}
	if leftPaused {
		targets = d.orphanMembers(v, stopped, recorded)
	}
	resumed, err := d.continueAll(ctx, targets)
	if err != nil {
		return d.controlError("resume", pid, err)
	}
	if resumed == 0 {
		return d.refused("resume", pid, "the process is gone")
	}
	attrs := map[string]string{"action": "resume", "pid": strconv.Itoa(pid), "name": v.Name, "orphan": "true", "processes": strconv.Itoa(resumed)}
	if v.Owner != "" {
		attrs["owner"] = v.Owner
	}
	if params.Force {
		attrs["force"] = "true"
	}
	d.emit(domain.EventControlResumed, "resumed stopped pid "+strconv.Itoa(pid)+" ("+v.Name+") that throtd did not pause", attrs)
	return nil
}

// notStopped checks key's live state with the OS and returns why it cannot
// be resumed: it is gone, or it is not stopped. It returns "" when it is
// stopped, or when its state cannot be read (the resume then verifies
// identity as usual, and continuing a running process is harmless).
func (d *Daemon) notStopped(ctx context.Context, key domain.ProcessKey) string {
	if d.ident == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, control.CallTimeout)
	p, err := d.ident.Identify(cctx, key.PID)
	cancel()
	switch {
	case errors.Is(err, domain.ErrProcessGone) || err == nil && !p.Start.Equal(key.Start):
		return "the process is gone"
	case err != nil || p.State == "" || p.State == domain.StateStopped:
		return ""
	}
	return "not stopped (it is " + string(p.State) + "); nothing to resume"
}

// orphanMembers returns the orphans among the members of the workload
// whose top process is v, children first, or just v when the workload is
// gone from the graph.
func (d *Daemon) orphanMembers(v process.StoppedView, stopped []process.StoppedView, recorded map[domain.ProcessKey]bool) []domain.ProcessKey {
	w, ok := d.workload(v.Key)
	if !ok {
		return []domain.ProcessKey{v.Key}
	}
	orphan := make(map[domain.ProcessKey]bool, len(stopped))
	for _, s := range stopped {
		orphan[s.Key] = d.orphaned(s, recorded)
	}
	var targets []domain.ProcessKey
	for _, m := range slices.Backward(w.Members) {
		if orphan[m.Key] && m.UID == d.uid {
			targets = append(targets, m.Key)
		}
	}
	return targets
}

// continueAll continues each of keys, verifying its identity first, and
// returns how many it continued. A process that is gone is skipped.
func (d *Daemon) continueAll(ctx context.Context, keys []domain.ProcessKey) (int, error) {
	var errs []error
	resumed := 0
	for _, k := range keys {
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
	return resumed, errors.Join(errs...)
}

// orphaned reports whether s is a stopped process of the daemon's user
// that no paused record explains.
func (d *Daemon) orphaned(s process.StoppedView, recorded map[domain.ProcessKey]bool) bool {
	return s.UID == d.uid && !recorded[s.Key]
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
