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
	"github.com/cfcromn/Throt/internal/scheduler"
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
	s := d.takeSample(ctx)
	if parent.Err() != nil {
		return // shutting down; partial samples are meaningless
	}
	d.track("host", s.herr)
	d.track("processes", s.perr)

	var r processRound
	if s.perr == nil {
		r = d.observeProcesses(ctx, s)
	}
	// Retry even when sampling fails: that is when memory is tight and a
	// resume is most likely to have failed.
	d.retryResumes(ctx)
	host := hostReading(s, r.snap)
	level, changed, reason := d.engine.Observe(host)

	from, ended, reserved, waiters := d.publish(s, r, host, level, reason)
	d.emitLeaseEnds(ended)
	if changed {
		d.emit(domain.EventPressureChanged, "pressure "+from.String()+" -> "+level.String(),
			map[string]string{"from": from.String(), "to": level.String(), "reason": reason})
	}
	d.applyPolicy(parent)
	if s.perr == nil {
		// Only schedule on a fresh process snapshot: acting on stale data
		// could queue a workload that already finished.
		d.schedule(parent, scheduler.Input{
			Now:            s.now,
			Pressure:       level,
			ThroughputOnly: d.engine.ThroughputOnly(),
			MemoryHeadroom: d.engine.MemoryHeadroom(),
			Heavy:          r.snap.Heavy,
			Exited:         r.exited,
			Reserved:       reserved,
			Admitted:       admitted(ended),
			Waiters:        waiters,
		}, r.stoppedOrphans)
		d.account(s.now, r.snap)
	} else {
		// No round ran, so there is no current would_queue to show.
		d.wouldQueue = d.wouldQueue[:0]
	}
	d.publishScheduler()
}

// sample is one tick's raw readings.
type sample struct {
	host domain.HostSample
	herr error
	// pausedBefore holds the paused records from before the process
	// sample; with those from after it, a workload paused or resumed while
	// ps runs is in one of them, so it is not an orphan.
	pausedBefore []control.PausedWorkload
	// sampled is when the process sample started: a lease granted after
	// this instant may belong to a process the sample does not show yet.
	sampled time.Time
	procs   []domain.ProcessSample
	perr    error
	now     time.Time
}

// takeSample probes responsiveness and samples the host and the process
// table.
func (d *Daemon) takeSample(ctx context.Context) sample {
	var s sample
	latency, latencyOK := monitor.ProbeLatency(ctx, latencyProbe)
	s.pausedBefore = d.ctl.Paused()
	s.host, s.herr = d.host.SampleHost(ctx)
	s.host.Latency = latency
	if !latencyOK {
		s.host.Missing = append(slices.Clone(s.host.Missing), domain.MetricLatency)
	}
	s.sampled = time.Now()
	s.procs, s.perr = d.procs.SampleProcesses(ctx)
	s.now = time.Now()
	return s
}

// processRound is what a successful process sample yields.
type processRound struct {
	snap        process.Snapshot
	orphans     []process.Orphan
	orphanCount int
	// stoppedOrphans holds the keys of all orphans, not only the MaxOrphans
	// listed, for the scheduler to hold.
	stoppedOrphans []domain.ProcessKey
	// exited holds the heavy workloads whose top process exited since the
	// previous sample, not those that are merely no longer heavy.
	exited []domain.ProcessKey
}

// observeProcesses feeds a successful process sample to the graph, emits
// its changes, finds and reports orphans, and forgets recorded processes
// that exited.
func (d *Daemon) observeProcesses(ctx context.Context, s sample) processRound {
	d.graphMu.Lock()
	changes := d.graph.Update(s.now, s.procs)
	snap := d.graph.Snapshot()
	r := processRound{snap: snap}
	for _, c := range changes {
		if c.Kind == domain.EventProcessLost && !d.graph.Live(c.Process) {
			r.exited = append(r.exited, c.Process)
		}
	}
	d.graphMu.Unlock()
	for _, c := range changes {
		d.emitChange(c)
	}
	// Before anything below resumes a record and drops it.
	recorded := recordedKeys(s.pausedBefore, d.ctl.Paused())
	r.orphans, r.orphanCount = process.FindOrphans(snap.Stopped, d.uid, func(k domain.ProcessKey) bool { return recorded[k] })
	d.reportOrphans(s.now, r.orphans)
	for _, v := range snap.Stopped {
		if d.orphaned(v, recorded) {
			r.stoppedOrphans = append(r.stoppedOrphans, v.Key)
		}
	}
	d.forgetExited(ctx, s.procs)
	return r
}

// hostReading is the host sample the pressure engine observes: every metric
// missing when sampling failed, and on platforms without a host CPU counter
// (macOS without cgo) the sum of per-process CPU deltas from snap instead.
func hostReading(s sample, snap process.Snapshot) domain.HostSample {
	host := s.host
	if s.herr != nil {
		return domain.HostSample{Time: s.now, Missing: allMissing}
	}
	if !host.Has(domain.MetricCPU) && snap.HasCPU {
		host.CPUUsage = snap.TotalCPU
		host.Missing = slices.DeleteFunc(slices.Clone(host.Missing), func(m domain.Metric) bool { return m == domain.MetricCPU })
	}
	return host
}

// publish stores the round's results in the status under d.mu and, in the
// same step, ends the leases and drops the waiters the sample supersedes.
// It returns the previous pressure level, the ended leases, and the
// lease owners and waiters for the scheduler.
func (d *Daemon) publish(s sample, r processRound, host domain.HostSample, level domain.PressureLevel, reason string) (from domain.PressureLevel, ended []leaseEnd, reserved []string, waiters []scheduler.Waiter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	from = d.status.Pressure
	d.status.Pressure, d.status.Reason, d.status.Degraded = level, reason, d.degradedSummary()
	if s.herr == nil {
		d.status.Host = &host
	}
	if s.perr == nil {
		d.status.Agents, d.status.Heavy = r.snap.Agents, r.snap.Heavy
		d.status.Orphans, d.status.OrphanCount = r.orphans, r.orphanCount
		d.stopped = r.snap.Stopped
		ended = d.leases.observe(s.sampled, s.now, s.procs, r.snap.Heavy)
		d.waiting.prune(s.now, s.sampled, s.procs)
	} else {
		ended = d.leases.expire(s.now)
		d.waiting.prune(s.now, s.sampled, nil)
	}
	return from, ended, d.leases.owners(), d.waiting.line()
}

// publishScheduler stores the scheduler's state after this round: its
// status, and the queue and policy acquire shares the line with.
func (d *Daemon) publishScheduler() {
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

// degradedSummary describes the failing samplers for StatusResult.Degraded.
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
	queued := d.schedulerQueued()
	d.statsMu.Lock()
	d.acct.Observe(now, snap, queued)
	d.statsMu.Unlock()
}
