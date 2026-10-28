package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// ReloadResult is returned by the "reload" method.
type ReloadResult struct {
	// Changed lists the applied changes, e.g. "scheduler.mode off -> observe".
	Changed []string `json:"changed"`
	// RequiresRestart lists changes the daemon did not apply; it keeps the
	// running values until it is restarted.
	RequiresRestart []string `json:"requires_restart"`
}

// ReasonEnforceDisabled is the reason code for resuming queued work when
// the scheduler leaves enforce mode.
const ReasonEnforceDisabled = "enforce_disabled"

// restartOnly reports keys fixed for the daemon's lifetime: the socket and
// its lock, the sampling ticker, and the event ring's capacity.
func restartOnly(key string) bool {
	switch key {
	case "socket_path", "sample_interval", "event_history":
		return true
	}
	return false
}

// SetConfigPath sets the file Reload re-reads. Call it before Run.
func (d *Daemon) SetConfigPath(path string) { d.cfgPath = path }

func (d *Daemon) reload(context.Context, json.RawMessage) (any, error) {
	return d.Reload()
}

// Reload re-reads the config file and applies scheduler, profile, and agent
// changes without a restart. It is all or nothing: a file that fails to load
// or validate changes nothing. Agent adapters apply at the next sample and
// the scheduler policy at the next scheduling round, where workloads already
// running count as pre-existing. It is safe to call from any goroutine.
func (d *Daemon) Reload() (ReloadResult, error) {
	if d.cfgPath == "" {
		return ReloadResult{}, d.reloadFailed(errors.New("config path unknown"))
	}
	next, err := config.Load(d.cfgPath)
	if err != nil {
		return ReloadResult{}, d.reloadFailed(err)
	}

	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	res := ReloadResult{Changed: []string{}, RequiresRestart: []string{}}
	var restart []string
	agentsChanged := false
	for _, c := range config.Diff(d.live, next) {
		agentsChanged = agentsChanged || c.Key == "agents"
		if restartOnly(c.Key) {
			res.RequiresRestart = append(res.RequiresRestart, c.String())
			restart = append(restart, c.Key)
		} else {
			res.Changed = append(res.Changed, c.String())
		}
	}
	next.SocketPath, next.SampleInterval, next.EventHistory = d.live.SocketPath, d.live.SampleInterval, d.live.EventHistory

	if agentsChanged {
		d.graphMu.Lock()
		d.graph.SetAdapters(adapters(next.Agents))
		d.graphMu.Unlock()
	}
	if next.Scheduler != d.live.Scheduler {
		select {
		case <-d.policyCh: // superseded before the tick took it
		default:
		}
		d.policyCh <- schedulerPolicy(next.Scheduler)
	}
	d.live = next

	msg := "config reloaded: no changes"
	if len(res.Changed) > 0 {
		msg = "config reloaded: " + strings.Join(res.Changed, ", ")
	}
	if len(res.RequiresRestart) > 0 {
		msg += "; requires restart: " + strings.Join(res.RequiresRestart, ", ")
	}
	d.emit(domain.EventConfigReloaded, msg, map[string]string{
		"changed":          strings.Join(res.Changed, ", "),
		"requires_restart": strings.Join(restart, ","),
	})
	return res, nil
}

func (d *Daemon) reloadFailed(err error) error {
	d.emit(domain.EventConfigReloadFailed, "config reload failed, nothing changed: "+err.Error(),
		map[string]string{"error": err.Error()})
	return err
}

// applyPolicy runs on the sampling goroutine and takes a policy left by
// Reload, if any. Leaving enforce resumes every workload the scheduler
// queued, because nothing would resume it otherwise; manual pauses are
// never touched.
func (d *Daemon) applyPolicy(ctx context.Context) {
	var p scheduler.Policy
	select {
	case p = <-d.policyCh:
	default:
		return
	}
	was := d.sched.Policy().Mode
	d.sched.SetPolicy(p)
	if was != scheduler.ModeEnforce || d.sched.Policy().Mode == scheduler.ModeEnforce {
		return
	}
	for _, w := range d.ctl.Paused() {
		if w.By == control.OriginScheduler {
			d.schedulerResume(ctx, w.Key, w.Name, w.Owner, ReasonEnforceDisabled)
		}
	}
}
