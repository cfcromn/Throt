// Package app wires Throt's components into the throtd daemon.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/monitor"
	"github.com/cfcromn/Throt/internal/pressure"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/stats"
	"github.com/cfcromn/Throt/internal/transport"
)

// Version is the release version, set at build time with -ldflags "-X".
// A build without it (go install) reports its build info instead.
var Version = "dev"

func init() {
	bi, ok := debug.ReadBuildInfo()
	Version = buildVersion(Version, bi, ok)
}

// buildVersion returns v unless it is "dev", the unstamped default. Then it
// returns the module version from bi, e.g. v0.8.0 for go install
// github.com/cfcromn/Throt/cmd/throtd@v0.8.0, or for a build from a
// checkout, "dev-" and the short VCS revision, with "-dirty" when the tree
// had uncommitted changes.
func buildVersion(v string, bi *debug.BuildInfo, ok bool) string {
	if v != "dev" || !ok || bi == nil {
		return v
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision":
			rev = s.Value[:min(len(s.Value), 7)]
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "-dirty"
		}
	}
	if rev == "" {
		return v
	}
	return v + "-" + rev + dirty
}

// PingResult is returned by the "ping" method.
type PingResult struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	PID      int    `json:"pid"`
	Uptime   string `json:"uptime"`
}

// MaxEventsPerCall caps one "events" response so it fits the protocol's
// message size limit.
const MaxEventsPerCall = 1000

// EventsParams selects which events the "events" method returns.
type EventsParams struct {
	Limit int `json:"limit"`
	// Since, when > 0, returns only events with a greater seq, oldest first,
	// up to Limit; poll again with the last seq to get the rest. A Since
	// beyond the newest event means the daemon restarted, so the newest
	// events are returned instead and the client sees seq go down.
	Since uint64 `json:"since,omitempty"`
}

// StatusResult is returned by the "status" method.
type StatusResult struct {
	Pressure domain.PressureLevel `json:"pressure"`
	Reason   string               `json:"reason,omitempty"`
	// Host is the latest successful sample; nil until one succeeds.
	Host   *domain.HostSample     `json:"host,omitempty"`
	Agents []process.AgentView    `json:"agents"`
	Heavy  []process.WorkloadView `json:"heavy"`
	// Degraded describes failing samplers; empty when all succeed.
	Degraded string `json:"degraded,omitempty"`
	// Paused lists every paused workload, manual or scheduled.
	Paused    []control.PausedWorkload `json:"paused"`
	Scheduler SchedulerStatus          `json:"scheduler"`
	// Orphans lists stopped processes of the user that no paused record
	// explains, at most process.MaxOrphans of them; OrphanCount counts them
	// all. Throt reports them and never resumes one unasked.
	Orphans     []process.Orphan `json:"orphans,omitempty"`
	OrphanCount int              `json:"orphan_count,omitempty"`
}

// StatsResult is returned by the "stats" method: per-agent usage since the
// daemon started, in memory only.
type StatsResult = stats.Report

// HostSampler reads host-wide metrics.
type HostSampler interface {
	SampleHost(ctx context.Context) (domain.HostSample, error)
}

// ProcessSampler reads the process table.
type ProcessSampler interface {
	SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error)
}

// Daemon is throtd's runtime state.
type Daemon struct {
	cfg     config.Config
	log     *slog.Logger
	events  *domain.EventLog
	host    HostSampler
	procs   ProcessSampler
	started time.Time

	ctl *control.Controller // safe for concurrent use
	sig control.Signaler    // resumes orphans, which have no paused record
	uid uint32              // the daemon's user; only its processes are controlled
	// ident resolves acquire callers; nil when sig cannot identify
	// processes, and then acquire is refused.
	ident ProcessIdentifier

	graphMu sync.Mutex // guards graph; held briefly, never while signaling
	graph   *process.Graph

	// Owned by the sampling goroutine.
	sched        *scheduler.Scheduler
	engine       *pressure.Engine
	failing      map[string]bool // sampler name -> currently failing
	failures     map[string]string
	retryFailing bool // resume retries are failing
	// reported holds the orphans an orphan_stopped event already named; it
	// is pruned to the current orphans each tick, so it stays bounded.
	reported map[domain.ProcessKey]bool

	// mu guards status, leases, and stopped together, so an acquire checks
	// and grants against one consistent view, and a tick replaces the heavy
	// workloads and drops the leases they supersede in one step.
	mu     sync.Mutex
	status StatusResult
	leases leaseTable
	// stopped is the latest snapshot's stopped processes, for resuming an
	// orphan beyond the MaxOrphans that status lists.
	stopped []process.StoppedView

	statsMu sync.Mutex // guards acct; held briefly
	acct    *stats.Accountant

	// cfgPath is the file Reload re-reads; empty disables reloading.
	cfgPath string
	// Reload handoff. The scheduler and engine belong to the sampling
	// goroutine, so a reload never touches them: it leaves the new policy
	// in policyCh (capacity 1), replacing one the tick has not taken yet,
	// and the next tick applies it before scheduling. A channel instead of
	// a lock keeps ticks from ever waiting on a reload. reloadMu serializes
	// reloads, so after draining policyCh the send cannot block (the tick
	// only receives). It also guards live, the latest accepted config, in
	// which restart-only keys keep their running values. The graph is
	// updated directly under graphMu.
	reloadMu sync.Mutex
	live     config.Config
	policyCh chan scheduler.Policy

	// writeFile and rename persist paused.json; tests replace them to
	// inject filesystem failures.
	writeFile func(path string, data []byte) error
	rename    func(oldpath, newpath string) error
}

// NewDaemon returns a daemon for cfg using the given samplers and process
// signaler, logging to log.
func NewDaemon(cfg config.Config, host HostSampler, procs ProcessSampler, sig control.Signaler, log *slog.Logger) *Daemon {
	d := &Daemon{
		cfg:    cfg,
		log:    log,
		events: domain.NewEventLog(cfg.EventHistory, strconv.FormatInt(time.Now().UnixNano(), 36)),
		host:   host,
		procs:  procs,
		graph: process.NewGraph(Adapters(cfg.Agents),
			process.WithCPUCount(runtime.NumCPU()), process.WithAgentUID(uint32(os.Getuid()))),
		engine:   pressure.New(pressure.DefaultWindow, pressure.DefaultThresholds()),
		sched:    newScheduler(cfg.Scheduler),
		failing:  map[string]bool{},
		failures: map[string]string{},
		acct:     stats.New(),
		sig:      sig,
		uid:      uint32(os.Getuid()),
		reported: map[domain.ProcessKey]bool{},
		live:     cfg,
		policyCh: make(chan scheduler.Policy, 1),

		writeFile: writeFileAtomic,
		rename:    os.Rename,
	}
	d.ctl = control.New(sig, d.uid, time.Now, d.saveState)
	d.ident, _ = sig.(ProcessIdentifier)
	return d
}

// Run listens on the configured socket and serves clients until ctx is
// cancelled. It refuses to start if another daemon already owns the socket.
func (d *Daemon) Run(ctx context.Context) error {
	unlock, err := lockInstance(d.cfg.SocketPath)
	if err != nil {
		return err
	}
	defer unlock()
	l, err := listen(d.cfg.SocketPath)
	if err != nil {
		return err
	}
	// Closing the listener (done by Serve) unlinks the socket.

	d.started = time.Now()
	d.emit(domain.EventDaemonStarted, "listening on "+d.cfg.SocketPath, nil)
	d.recoverPaused(ctx)
	srv := transport.NewServer(map[string]transport.Handler{
		"ping":    d.ping,
		"events":  d.recentEvents,
		"status":  d.currentStatus,
		"stats":   d.currentStats,
		"pause":   d.pause,
		"resume":  d.resume,
		"reload":  d.reload,
		"acquire": d.acquire,
	}, transport.WithPrivileged(privilegedMethods()...))

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitor.Run(ctx, time.Duration(d.cfg.SampleInterval), d.tick)
	}()
	err = srv.Serve(ctx, l)
	cancel()
	wg.Wait()
	d.resumeAll() // fail open: never leave work paused behind us
	d.emit(domain.EventDaemonStopped, "shutdown", nil)
	return err
}

// privilegedMethods may only be called by the user running the daemon: they
// signal processes, change its configuration, or reserve heavy slots.
func privilegedMethods() []string { return []string{"pause", "resume", "reload", "acquire"} }

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
	} else {
		ended = d.leases.expire(now)
	}
	reserved := d.leases.owners()
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
		d.schedule(parent, now, level, snap.Heavy, reserved, admitted(ended), stoppedOrphans)
		d.account(now, snap)
	}
	sched := d.schedulerStatus()
	d.mu.Lock()
	d.status.Scheduler = sched
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

func (d *Daemon) emitChange(c process.Change) {
	attrs := map[string]string{"pid": strconv.Itoa(c.Process.PID), "name": c.Name}
	msg := string(c.Kind) + " " + c.Name
	if c.Agent != "" {
		attrs["agent"] = c.Agent
		msg = string(c.Kind) + " " + c.Agent
	}
	d.emit(c.Kind, msg, attrs)
}

func (d *Daemon) currentStatus(context.Context, json.RawMessage) (any, error) {
	d.mu.Lock()
	st := d.status
	st.Scheduler.Leases = d.leases.views()
	d.mu.Unlock()
	st.Paused = d.ctl.Paused()
	return st, nil
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

func (d *Daemon) currentStats(context.Context, json.RawMessage) (any, error) {
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	return d.acct.Report(), nil
}

func (d *Daemon) emit(kind domain.EventKind, msg string, attrs map[string]string) {
	d.events.Append(domain.Event{Time: time.Now(), Kind: kind, Message: msg, Attrs: attrs})
	if !d.log.Enabled(context.Background(), eventLevel(kind)) {
		return
	}
	args := make([]any, 0, 2+2*len(attrs))
	args = append(args, "event", kind)
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		args = append(args, k, attrs[k])
	}
	d.log.Log(context.Background(), eventLevel(kind), msg, args...)
}

// eventLevel is the log level of an event. Every event is in the event log;
// info stays quiet in steady state, so heavy workloads starting and exiting
// and admission leases, which come and go with every build, are debug.
func eventLevel(kind domain.EventKind) slog.Level {
	switch kind {
	case domain.EventProcessDiscovered, domain.EventProcessLost,
		domain.EventLeaseGranted, domain.EventLeaseReleased:
		return slog.LevelDebug
	case domain.EventDegraded, domain.EventControlFailed, domain.EventControlRefused,
		domain.EventConfigReloadFailed, domain.EventOrphanStopped:
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func (d *Daemon) ping(context.Context, json.RawMessage) (any, error) {
	return PingResult{
		Version:  Version,
		Protocol: transport.ProtocolVersion,
		PID:      os.Getpid(),
		Uptime:   time.Since(d.started).Round(time.Second).String(),
	}, nil
}

func (d *Daemon) recentEvents(_ context.Context, raw json.RawMessage) (any, error) {
	var p EventsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if p.Limit <= 0 || p.Limit > MaxEventsPerCall {
		p.Limit = MaxEventsPerCall
	}
	if p.Since == 0 {
		return d.events.Recent(p.Limit), nil
	}
	if evs := d.events.Since(p.Since, p.Limit); len(evs) > 0 {
		return evs, nil
	}
	if newest := d.events.Recent(1); len(newest) == 1 && newest[0].Seq < p.Since {
		return d.events.Recent(p.Limit), nil
	}
	return []domain.Event{}, nil
}

// lockInstance takes an exclusive lock next to the socket so only one daemon
// can own it, even when two start at once. The lock dies with the process.
func lockInstance(socket string) (unlock func(), err error) {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := transport.CheckPrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("throtd is already running on %s", socket)
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

// listen creates an owner-only socket at path. The caller holds the instance
// lock, so an existing socket is stale. Only an actual socket is removed;
// any other file at path is an error rather than collateral damage.
func listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return l, nil
}

// Adapters returns the agent adapters for a config: custom agents first, so
// they win over built-in names.
func Adapters(custom []config.Agent) []agent.Adapter {
	var out []agent.Adapter
	for _, a := range custom {
		out = append(out, agent.NewNameAdapter(a.Name, a.Executables...))
	}
	return append(out, agent.DefaultAdapters()...)
}
