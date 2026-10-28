package app

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/monitor"
	"github.com/cfcromn/Throt/internal/process"
)

// allMissing marks every host metric unavailable. A failed host sample still
// reaches the pressure engine this way, so stale readings age out of the
// window and pressure falls back to unknown instead of freezing.
var allMissing = []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricMemory, domain.MetricSwap, domain.MetricLatency, domain.MetricMemoryPressure}

// latencyProbe is the timer the responsiveness probe waits on each tick.
const latencyProbe = 10 * time.Millisecond

// sampleTimeout bounds one sampling round.
const sampleTimeout = 5 * time.Second

// tick takes one host and process sample and publishes the result.
func (d *Daemon) tick(parent context.Context) {
	// Bound each round so a hung sampler (e.g. ps) cannot stall updates.
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	latency, latencyOK := monitor.ProbeLatency(ctx, latencyProbe)
	// Records from before and after the process sample: a workload paused
	// or resumed while ps runs is in one of them, so it is not an orphan.
	pausedBefore := d.ctl.Paused()
	host, herr := d.host.SampleHost(ctx)
	host.Latency = latency
	if !latencyOK {
		host.Missing = append(slices.Clone(host.Missing), domain.MetricLatency)
	}
	// A lease granted after this instant may belong to a process the
	// sample below does not show yet.
	sampled := time.Now()
	procs, perr := d.procs.SampleProcesses(ctx)
	if parent.Err() != nil {
		return // shutting down; partial samples are meaningless
	}
	now := time.Now()
	d.track("host", herr)
	d.track("processes", perr)

	var snap process.Snapshot
	var orphans []process.Orphan
	var orphanCount int
	var stoppedOrphans []domain.ProcessKey
	if perr == nil {
		d.graphMu.Lock()
		changes := d.graph.Update(now, procs)
		snap = d.graph.Snapshot()
		d.graphMu.Unlock()
		for _, c := range changes {
			d.emitChange(c)
		}
		// Before anything below resumes a record and drops it.
		recorded := recordedKeys(pausedBefore, d.ctl.Paused())
		orphans, orphanCount = process.FindOrphans(snap.Stopped, d.uid, func(k domain.ProcessKey) bool { return recorded[k] })
		d.reportOrphans(now, orphans)
		for _, s := range snap.Stopped {
			if s.UID == d.uid && !recorded[s.Key] {
				stoppedOrphans = append(stoppedOrphans, s.Key)
			}
		}
		d.forgetExited(ctx, procs)
	}
	// Retry even when sampling fails: that is when memory is tight and a
	// resume is most likely to have failed.
	d.retryResumes(ctx)
	if herr != nil {
		host = domain.HostSample{Time: now, Missing: allMissing}
	} else if !host.Has(domain.MetricCPU) && snap.HasCPU {
		// Platforms without a host CPU counter (macOS without cgo) get the
		// sum of per-process CPU deltas instead.
		host.CPUUsage = snap.TotalCPU
		host.Missing = slices.DeleteFunc(slices.Clone(host.Missing), func(m domain.Metric) bool { return m == domain.MetricCPU })
	}
	level, changed, reason := d.engine.Observe(host)

	d.mu.Lock()
	from := d.status.Pressure
	d.status.Pressure, d.status.Reason, d.status.Degraded = level, reason, d.degradedSummary()
	if herr == nil {
		d.status.Host = &host
	}
	var ended []leaseEnd
	if perr == nil {
		d.status.Agents, d.status.Heavy = snap.Agents, snap.Heavy
		d.status.Orphans, d.status.OrphanCount = orphans, orphanCount
		d.stopped = snap.Stopped
		ended = d.leases.observe(sampled, now, procs, snap.Heavy)
		d.waiting.prune(now, sampled, procs)
	} else {
		ended = d.leases.expire(now)
		d.waiting.prune(now, sampled, nil)
	}
	reserved := d.leases.owners()
	waiters := d.waiting.line()
	d.mu.Unlock()
	d.emitLeaseEnds(ended)

	if changed {
		d.emit(domain.EventPressureChanged, "pressure "+from.String()+" -> "+level.String(),
			map[string]string{"from": from.String(), "to": level.String(), "reason": reason})
	}
	d.applyPolicy(parent)
	if perr == nil {
		// Only schedule on a fresh process snapshot: acting on stale data
		// could queue a workload that already finished.
		d.schedule(parent, now, level, d.engine.ThroughputOnly(), snap.Heavy, reserved, admitted(ended), waiters, stoppedOrphans)
		d.account(now, snap)
	}
	sched := d.schedulerStatus()
	line, linePolicy := d.sched.Queued(), d.sched.Policy()
	d.mu.Lock()
	d.status.Scheduler = sched
	d.queuedLine, d.linePolicy = line, linePolicy
	d.mu.Unlock()
}

// track records a sampler's result and emits an event on each transition
// between failing and healthy, not on every failed tick.
func (d *Daemon) track(name string, err error) {
	switch {
	case err != nil && !d.failing[name]:
		d.failing[name] = true
		d.emit(domain.EventDegraded, name+" sampling failed: "+err.Error(), map[string]string{"sampler": name})
	case err == nil && d.failing[name]:
		d.failing[name] = false
		d.emit(domain.EventRecovered, name+" sampling recovered", map[string]string{"sampler": name})
	}
	if err != nil {
		d.failures[name] = err.Error()
	} else {
		delete(d.failures, name)
	}
}

func (d *Daemon) degradedSummary() string {
	var parts []string
	for _, name := range []string{"host", "processes"} {
		if msg, ok := d.failures[name]; ok {
			parts = append(parts, name+": "+msg)
		}
	}
	return strings.Join(parts, "; ")
}

// account adds a fresh snapshot to the per-agent usage. Workloads the
// scheduler holds after this round are charged queue time until the next.
func (d *Daemon) account(now time.Time, snap process.Snapshot) {
	var queued []domain.ProcessKey
	for _, p := range d.ctl.Paused() {
		if p.By == control.OriginScheduler {
			queued = append(queued, p.Key)
		}
	}
	d.statsMu.Lock()
	d.acct.Observe(now, snap, queued)
	d.statsMu.Unlock()
}
