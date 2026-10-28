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
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
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

// Daemon is throtd's runtime state.
type Daemon struct {
	cfg     config.Config
	log     *slog.Logger
	events  *domain.EventLog
	started time.Time
}

// NewDaemon returns a daemon for cfg logging to log.
func NewDaemon(cfg config.Config, log *slog.Logger) *Daemon {
	return &Daemon{cfg: cfg, log: log, events: domain.NewEventLog(cfg.EventHistory)}
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
	d.emit(domain.EventDaemonStarted, "listening on "+d.cfg.SocketPath)
	srv := transport.NewServer(map[string]transport.Handler{
		"ping":   d.ping,
		"events": d.recentEvents,
	})
	err = srv.Serve(ctx, l)
	d.emit(domain.EventDaemonStopped, "shutdown")
	return err
}

func (d *Daemon) emit(kind domain.EventKind, msg string) {
	d.events.Append(domain.Event{Time: time.Now(), Kind: kind, Message: msg})
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
