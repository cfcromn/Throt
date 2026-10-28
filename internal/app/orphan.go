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

	targets := []domain.ProcessKey{v.Key}
	if leftPaused {
		w, ok := d.workload(v.Key)
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
