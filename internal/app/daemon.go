// Package app wires Throt's components into the throtd daemon.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	l, err := listen(ctx, d.cfg.SocketPath)
	if err != nil {
		return err
	}
	defer os.Remove(d.cfg.SocketPath)

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
	return d.events.Recent(p.Limit), nil
}

// listen creates an owner-only socket at path. A stale socket left by a
// crashed daemon is replaced; a live one is an error.
func listen(ctx context.Context, path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		conn, derr := (&net.Dialer{}).DialContext(dctx, "unix", path)
		cancel()
		if derr == nil {
			conn.Close()
			return nil, fmt.Errorf("throtd is already running on %s", path)
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

// checkPrivateDir refuses socket directories owned by, or writable by, other
// users, which would let them replace the socket.
func checkPrivateDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat socket dir: %w", err)
	}
	if !fi.IsDir() {
		return errors.New("socket dir is not a directory: " + dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket dir %s is owned by uid %d, not the current user", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("socket dir %s is writable by other users (mode %v); use a private directory", dir, fi.Mode().Perm())
	}
	return nil
}
