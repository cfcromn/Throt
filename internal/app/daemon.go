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
	"sync"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/monitor"
	"github.com/cfcromn/Throt/internal/pressure"
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
	// Host is the latest sample; nil until the first sample succeeds.
	Host *domain.HostSample `json:"host,omitempty"`
	// Degraded is set when the latest sampling attempt failed.
	Degraded string `json:"degraded,omitempty"`
}

// Daemon is throtd's runtime state.
type Daemon struct {
	cfg     config.Config
	log     *slog.Logger
	events  *domain.EventLog
	sampler monitor.HostSampler
	started time.Time

	mu       sync.Mutex // guards the fields below
	engine   *pressure.Engine
	status   StatusResult
	degraded bool
}

// NewDaemon returns a daemon for cfg that samples the host with sampler and
// logs to log.
func NewDaemon(cfg config.Config, sampler monitor.HostSampler, log *slog.Logger) *Daemon {
	return &Daemon{
		cfg:     cfg,
		log:     log,
		events:  domain.NewEventLog(cfg.EventHistory),
		sampler: sampler,
		engine:  pressure.New(pressure.DefaultWindow, pressure.DefaultThresholds()),
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
	srv := transport.NewServer(map[string]transport.Handler{
		"ping":   d.ping,
		"events": d.recentEvents,
		"status": d.currentStatus,
	})

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitor.Run(ctx, time.Duration(d.cfg.SampleInterval), d.sampler, d.observe, d.sampleFailed)
	}()
	err = srv.Serve(ctx, l)
	cancel()
	wg.Wait()
	d.emit(domain.EventDaemonStopped, "shutdown", nil)
	return err
}

func (d *Daemon) observe(s domain.HostSample) {
	d.mu.Lock()
	level, changed, reason := d.engine.Observe(s)
	from := d.status.Pressure
	d.status = StatusResult{Pressure: level, Reason: reason, Host: &s}
	recovered := d.degraded
	d.degraded = false
	d.mu.Unlock()

	if recovered {
		d.emit(domain.EventDegraded, "host sampling recovered", nil)
	}
	if changed {
		d.emit(domain.EventPressureChanged, "pressure "+from.String()+" -> "+level.String(),
			map[string]string{"from": from.String(), "to": level.String(), "reason": reason})
	}
}

func (d *Daemon) sampleFailed(err error) {
	d.mu.Lock()
	first := !d.degraded
	d.degraded = true
	d.status.Degraded = err.Error()
	d.mu.Unlock()
	if first { // log transitions only, not every failed tick
		d.emit(domain.EventDegraded, "host sampling failed: "+err.Error(), nil)
	}
}

func (d *Daemon) currentStatus(context.Context, json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
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
