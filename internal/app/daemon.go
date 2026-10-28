// Package app wires Throt's components into the throtd daemon.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	"github.com/cfcromn/Throt/internal/transport"
)

// Version is the release version, set at build time with -ldflags "-X".
var Version = "dev"

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

// EventsParams selects how many recent events the "events" method returns.
type EventsParams struct {
	Limit int `json:"limit"`
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
	// Paused lists workloads paused by `throt pause`.
	Paused []control.PausedWorkload `json:"paused"`
}

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

	graphMu sync.Mutex // guards graph; held briefly, never while signaling
	graph   *process.Graph

	// Owned by the sampling goroutine.
	engine   *pressure.Engine
	failing  map[string]bool // sampler name -> currently failing
	failures map[string]string

	mu     sync.Mutex // guards status
	status StatusResult
}

// NewDaemon returns a daemon for cfg using the given samplers and process
// signaler, logging to log.
func NewDaemon(cfg config.Config, host HostSampler, procs ProcessSampler, sig control.Signaler, log *slog.Logger) *Daemon {
	return &Daemon{
		ctl:    control.New(sig, uint32(os.Getuid()), time.Now),
		cfg:    cfg,
		log:    log,
		events: domain.NewEventLog(cfg.EventHistory),
		host:   host,
		procs:  procs,
		graph: process.NewGraph(agent.DefaultAdapters(),
			process.WithCPUCount(runtime.NumCPU()), process.WithAgentUID(uint32(os.Getuid()))),
		engine:   pressure.New(pressure.DefaultWindow, pressure.DefaultThresholds()),
		failing:  map[string]bool{},
		failures: map[string]string{},
	}
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
		"ping":   d.ping,
		"events": d.recentEvents,
		"status": d.currentStatus,
		"pause":  d.pause,
		"resume": d.resume,
	}, transport.WithPrivileged("pause", "resume"))

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

// allMissing marks every host metric unavailable. A failed host sample still
// reaches the pressure engine this way, so stale readings age out of the
// window and pressure falls back to unknown instead of freezing.
var allMissing = []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricMemory, domain.MetricSwap}

// sampleTimeout bounds one sampling round.
const sampleTimeout = 5 * time.Second

// tick takes one host and process sample and publishes the result.
func (d *Daemon) tick(parent context.Context) {
	// Bound each round so a hung sampler (e.g. ps) cannot stall updates.
	ctx, cancel := context.WithTimeout(parent, sampleTimeout)
	defer cancel()
	host, herr := d.host.SampleHost(ctx)
	procs, perr := d.procs.SampleProcesses(ctx)
	if parent.Err() != nil {
		return // shutting down; partial samples are meaningless
	}
	now := time.Now()
	d.track("host", herr)
	d.track("processes", perr)

	var snap process.Snapshot
	if perr == nil {
		d.graphMu.Lock()
		changes := d.graph.Update(now, procs)
		snap = d.graph.Snapshot()
		d.graphMu.Unlock()
		for _, c := range changes {
			d.emitChange(c)
		}
		d.forgetExited(procs)
	}
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
	if perr == nil {
		d.status.Agents, d.status.Heavy = snap.Agents, snap.Heavy
	}
	d.mu.Unlock()

	if changed {
		d.emit(domain.EventPressureChanged, "pressure "+from.String()+" -> "+level.String(),
			map[string]string{"from": from.String(), "to": level.String(), "reason": reason})
	}
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
	d.mu.Unlock()
	st.Paused = d.ctl.Paused()
	return st, nil
}

func (d *Daemon) emit(kind domain.EventKind, msg string, attrs map[string]string) {
	d.events.Append(domain.Event{Time: time.Now(), Kind: kind, Message: msg, Attrs: attrs})
	d.log.Info(msg, "event", kind)
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
	return d.events.Recent(p.Limit), nil
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
