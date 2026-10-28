package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/transport"
)

// versionInfo is the 'throt version --json' document.
type versionInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Go       string `json:"go"`
	// Daemon is throtd's ping result, or null when it is unreachable.
	Daemon *app.PingResult `json:"daemon"`
	// DaemonError says why the daemon is missing, when it is.
	DaemonError string `json:"daemon_error,omitempty"`
}

// runVersion prints the client version, or with --json a versionInfo that
// also asks throtd for its version.
func runVersion(args []string, cfgPath string, w io.Writer) error {
	switch {
	case len(args) == 0:
		_, err := fmt.Fprintln(w, app.Version)
		return err
	case len(args) == 1 && (args[0] == "--json" || args[0] == "-json"):
	default:
		return errors.New("usage: throt version [--json]")
	}
	// An invalid config must not hide the client version.
	socket := config.DefaultSocketPath()
	if cfg, err := config.Load(cfgPath); err == nil {
		socket = cfg.SocketPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return printVersionJSON(ctx, w, func(ctx context.Context) (app.PingResult, error) {
		var r app.PingResult
		err := transport.Call(ctx, socket, "ping", nil, &r)
		return r, err
	})
}

func printVersionJSON(ctx context.Context, w io.Writer, ping func(context.Context) (app.PingResult, error)) error {
	v := versionInfo{Version: app.Version, Protocol: transport.ProtocolVersion, Go: runtime.Version()}
	if r, err := ping(ctx); err != nil {
		v.DaemonError = err.Error()
	} else {
		v.Daemon = &r
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
