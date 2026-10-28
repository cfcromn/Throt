package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/transport"
)

func TestVersionJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		ping func(context.Context) (app.PingResult, error)
	}{
		{"daemon up", func(context.Context) (app.PingResult, error) {
			return app.PingResult{Version: "v9", Protocol: transport.ProtocolVersion, PID: 42, Uptime: "1s"}, nil
		}},
		{"daemon down", func(context.Context) (app.PingResult, error) {
			return app.PingResult{}, errors.New("connection refused")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := printVersionJSON(context.Background(), &b, tc.ping); err != nil {
				t.Fatal(err)
			}
			var v versionInfo
			dec := json.NewDecoder(&b)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if v.Version != app.Version || v.Protocol != transport.ProtocolVersion || v.Go != runtime.Version() {
				t.Errorf("client fields = %+v", v)
			}
			if _, err := tc.ping(context.Background()); err == nil {
				if v.Daemon == nil || v.Daemon.Version != "v9" || v.DaemonError != "" {
					t.Errorf("daemon = %+v, error %q", v.Daemon, v.DaemonError)
				}
			} else if v.Daemon != nil || v.DaemonError == "" {
				t.Errorf("daemon = %+v, error %q; want null and a reason", v.Daemon, v.DaemonError)
			}
		})
	}
}

func TestRunVersionArgs(t *testing.T) {
	var b bytes.Buffer
	if err := runVersion(nil, "", &b); err != nil || b.String() != app.Version+"\n" {
		t.Errorf("plain version = %q, %v", b.String(), err)
	}
	// --json against a socket nobody listens on still succeeds.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	b.Reset()
	if err := runVersion([]string{"--json"}, filepath.Join(t.TempDir(), "none.json"), &b); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b.Bytes()) {
		t.Errorf("invalid JSON: %s", b.String())
	}
	if err := runVersion([]string{"--yaml"}, "", &b); err == nil {
		t.Error("want a usage error")
	}
}
