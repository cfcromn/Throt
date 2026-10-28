// Package app wires Throt's components into the throtd daemon.
package app

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/pressure"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/stats"
	"github.com/cfcromn/Throt/internal/transport"
)

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
	// UnattributedMemory lists the process names with the most resident
	// memory outside every agent (at most process.MaxUnattributed);
	// UnattributedRSS totals every process outside the agents. Names only:
	// no arguments, no paths.
	UnattributedMemory []process.NameUsage `json:"unattributed_memory,omitempty"`
	UnattributedRSS    uint64              `json:"unattributed_rss,omitempty"`
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
	// exits polls running heavy workloads for an early refill; nil when
	// sig cannot, and then freed slots refill at the next sample.
	exits ExitChecker
	// refillEvery is how often exits is polled; tests shorten it.
	refillEvery time.Duration

	graphMu sync.Mutex // guards graph; held briefly, never while signaling
	graph   *process.Graph

	// Owned by the sampling goroutine.
	sched        *scheduler.Scheduler
	engine       *pressure.Engine
	failing      map[string]bool // sampler name -> currently failing
	failures     map[string]string
	retryFailing bool // resume retries are failing
	// wouldQueue holds the workloads the latest observe-mode round would
	// have queued; bounded by the snapshot.
	wouldQueue []domain.ProcessKey
	// reported holds the orphans an orphan_stopped event already named; it
	// is pruned to the current orphans each tick, so it stays bounded.
	reported map[domain.ProcessKey]bool
	// watch holds the running heavy workloads whose exit the loop polls
	// for while work waits for a slot (see refillWatch); nil otherwise.
	watch []domain.ProcessKey
	// procsFailingSince is when the current run of failed process samples
	// began; zero while sampling works.
	procsFailingSince time.Time

	// mu guards status, leases, and stopped together, so an acquire checks
	// and grants against one consistent view, and a tick replaces the heavy
	// workloads and drops the leases they supersede in one step.
	mu     sync.Mutex
	status StatusResult
	leases leaseTable
	// waiting holds the lease waiters; queuedLine and linePolicy are the
	// scheduler's queue and policy as of its latest round, so acquire can
	// place a waiter in the line it shares with queued workloads.
	waiting    waitList
	queuedLine []scheduler.Waiter
	linePolicy scheduler.Policy
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
		engine:   pressure.New(pressure.WindowFor(time.Duration(cfg.SampleInterval)), pressure.DefaultThresholds()),
		sched:    newScheduler(cfg.Scheduler),
		failing:  map[string]bool{},
		failures: map[string]string{},
		acct:     stats.New(),
		sig:      sig,
		uid:      uint32(os.Getuid()),
		reported: map[domain.ProcessKey]bool{},
		live:     cfg,
		policyCh: make(chan scheduler.Policy, 1),

		refillEvery: refillCheck,

		writeFile: writeFileAtomic,
		rename:    os.Rename,
	}
	d.ctl = control.New(sig, d.uid, time.Now, d.saveState)
	d.ident, _ = sig.(ProcessIdentifier)
	d.exits, _ = sig.(ExitChecker)
	d.linePolicy = d.sched.Policy()
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
	srv := transport.NewServer(d.handlers(), transport.WithPrivileged(privilegedMethods()...))

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sampleLoop(ctx, time.Duration(d.cfg.SampleInterval), d.refillEvery, d.tickAndWatch)
	}()
	err = srv.Serve(ctx, l)
	cancel()
	wg.Wait()
	msg, attrs := "shutdown", map[string]string(nil)
	// Fail open: never leave work paused behind us.
	if n := d.resumeAll(); n > 0 {
		msg, attrs = "shutdown; resumed "+strconv.Itoa(n)+" paused/queued processes", map[string]string{"resumed": strconv.Itoa(n)}
	}
	d.emit(domain.EventDaemonStopped, msg, attrs)
	return err
}

// workload returns the current members of the workload with top process
// key. It holds graphMu only for the lookup, so the caller may signal the
// result.
func (d *Daemon) workload(key domain.ProcessKey) (process.WorkloadTarget, bool) {
	d.graphMu.Lock()
	defer d.graphMu.Unlock()
	return d.graph.Workload(key)
}

// handlers returns the methods of local protocol v1.
func (d *Daemon) handlers() map[string]transport.Handler {
	return map[string]transport.Handler{
		"ping":    d.ping,
		"events":  d.recentEvents,
		"status":  d.currentStatus,
		"stats":   d.currentStats,
		"pause":   d.pause,
		"resume":  d.resume,
		"reload":  d.reload,
		"acquire": d.acquire,
	}
}

// privilegedMethods may only be called by the user running the daemon: they
// signal processes, change its configuration, or reserve heavy slots.
func privilegedMethods() []string { return []string{"pause", "resume", "reload", "acquire"} }

// Adapters returns the agent adapters for a config: custom agents first, so
// they win over built-in names.
func Adapters(custom []config.Agent) []agent.Adapter {
	var out []agent.Adapter
	for _, a := range custom {
		out = append(out, agent.NewNameAdapter(a.Name, a.Executables...))
	}
	return append(out, agent.DefaultAdapters()...)
}
