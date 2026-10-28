package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

type fakeHost struct{ err error }

func (f fakeHost) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{Missing: []domain.Metric{domain.MetricCPU}}, f.err
}

type fakeProcs struct{}

func (fakeProcs) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return []domain.ProcessSample{{PID: 1, Name: "secret-tool"}, {PID: 2, Name: "claude"}}, nil
}

func TestCollect(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		ConfigPath: "/home/alice/.config/throt/config.json",
		SocketPath: "/tmp/throt-501/throtd.sock",
		Ping:       func(context.Context) (app.PingResult, error) { return app.PingResult{Version: "v1", Protocol: 1}, nil },
		Status: func(context.Context) (app.StatusResult, error) {
			return app.StatusResult{
				Pressure: domain.PressureHigh,
				Degraded: "processes: /home/alice/bin/ps failed",
				Agents:   []process.AgentView{{Agent: "claude-1", Name: "claude"}},
			}, nil
		},
		Host:      fakeHost{},
		Processes: fakeProcs{},
	})
	r = redact(r, redactor{pairs: []string{"/home/alice", "~", "alice", "<user>"}})

	if !r.Daemon.Reachable || r.Status == nil || r.Status.Agents != 1 || r.Status.Pressure != domain.PressureHigh {
		t.Errorf("daemon/status = %+v %+v", r.Daemon, r.Status)
	}
	if !r.Host.OK || len(r.Host.Missing) != 1 || !r.Processes.OK || r.Processes.Count != 2 {
		t.Errorf("checks = %+v %+v", r.Host, r.Processes)
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"alice", "secret-tool", "claude"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("report leaks %q: %s", leak, b)
		}
	}
	if r.ConfigPath != "~/.config/throt/config.json" || r.Config != "default (no file)" {
		t.Errorf("config = %q %q", r.ConfigPath, r.Config)
	}
}

func TestCollectDaemonDown(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		Ping: func(context.Context) (app.PingResult, error) {
			return app.PingResult{}, errors.New("connection refused")
		},
		Status: func(context.Context) (app.StatusResult, error) {
			t.Fatal("status called")
			return app.StatusResult{}, nil
		},
		Host:      fakeHost{err: errors.New("no sysctl")},
		Processes: fakeProcs{},
		ConfigErr: errors.New("unknown field"),
	})
	if r.Daemon.Reachable || r.Daemon.Error == "" || r.Status != nil || r.Host.OK || r.Config != "unknown field" {
		t.Errorf("report = %+v", r)
	}
}
