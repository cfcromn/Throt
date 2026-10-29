package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
	if !r.Host.OK || !r.Processes.OK || r.Processes.Count != 2 {
		t.Errorf("checks = %+v %+v", r.Host, r.Processes)
	}
	// Without a host CPU counter, throtd derives CPU from the process
	// table, so doctor must not list it as missing.
	if len(r.Host.Missing) != 0 || r.Host.Derived[domain.MetricCPU] != "measured by throtd (iostat on macOS, else process CPU)" {
		t.Errorf("host = %+v, want cpu derived, nothing missing", r.Host)
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

type stoppedProcs struct{}

func (stoppedProcs) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return []domain.ProcessSample{
		{PID: 10, PPID: 1, UID: 501, Name: "claude"},
		{PID: 11, PPID: 10, UID: 501, Name: "make", State: domain.StateStopped},
		{PID: 20, PPID: 1, UID: 501, Name: "zsh"},
		{PID: 21, PPID: 20, UID: 501, Name: "vim", State: domain.StateStopped},
		{PID: 30, PPID: 1, UID: 502, Name: "make", State: domain.StateStopped}, // another user's
	}, nil
}

func TestCollectOrphansFromDaemon(t *testing.T) {
	now := time.Now()
	r := Collect(context.Background(), Inputs{
		Ping: func(context.Context) (app.PingResult, error) { return app.PingResult{Version: "v1"}, nil },
		Status: func(context.Context) (app.StatusResult, error) {
			return app.StatusResult{OrphanCount: 51, Orphans: []process.Orphan{
				{Key: domain.ProcessKey{PID: 11}, Name: "make", Owner: "claude-1", Hint: "likely left paused", Since: now.Add(-90 * time.Second)},
				{Key: domain.ProcessKey{PID: 12}, Name: "alice-tool", Hint: "likely job control (Ctrl-Z)"},
			}}, nil
		},
		Host:      fakeHost{},
		Processes: stoppedProcs{},
	})
	r = redact(r, redactor{pairs: []string{"alice", "<user>"}})
	o := r.Orphans
	if o.Source != "throtd" || o.Count != 51 || len(o.Entries) != 2 {
		t.Fatalf("orphans = %+v", o)
	}
	if e := o.Entries[0]; e.PID != 11 || e.Name != "make" || e.Owner != "claude-1" || e.Hint != "likely left paused" || e.StoppedFor != "1m30s" {
		t.Errorf("entry 0 = %+v", e)
	}
	if e := o.Entries[1]; e.Name != "<user>-tool" || e.StoppedFor != "" {
		t.Errorf("entry 1 = %+v, want a redacted name and no duration", e)
	}
}

func TestCollectOrphansLocal(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		Ping: func(context.Context) (app.PingResult, error) {
			return app.PingResult{}, errors.New("connection refused")
		},
		Status:    func(context.Context) (app.StatusResult, error) { return app.StatusResult{}, nil },
		Host:      fakeHost{},
		Processes: stoppedProcs{},
		Adapters:  app.Adapters(nil),
		UID:       501,
	})
	o := r.Orphans
	if o.Source != "local" || o.Count != 2 || len(o.Entries) != 2 {
		t.Fatalf("orphans = %+v, want make and vim of uid 501", o)
	}
	if e := o.Entries[0]; e.PID != 11 || e.Owner != "claude-1" || !strings.Contains(e.Hint, "left paused by an earlier throtd") {
		t.Errorf("entry 0 = %+v", e)
	}
	if e := o.Entries[1]; e.PID != 21 || e.Owner != "" || !strings.Contains(e.Hint, "job control") {
		t.Errorf("entry 1 = %+v", e)
	}
	b, _ := json.Marshal(r.Orphans)
	for _, f := range []string{`"pid"`, `"name"`, `"hint"`, `"count"`} {
		if !strings.Contains(string(b), f) {
			t.Errorf("orphans JSON lacks %s: %s", f, b)
		}
	}
}

func TestCollectNoOrphans(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{}, errors.New("down") },
		Host:      fakeHost{},
		Processes: fakeProcs{},
	})
	b, _ := json.Marshal(r)
	if r.Orphans.Count != 0 || !strings.Contains(string(b), `"orphans":{"source":"local","count":0}`) {
		t.Errorf("report = %s", b)
	}
}

type failingProcs struct{}

func (failingProcs) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return nil, errors.New("ps failed")
}

// TestCollectCPUMissingWithoutProcesses checks that CPU stays missing when
// the process table cannot be read, since throtd derives CPU from it.
func TestCollectCPUMissingWithoutProcesses(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{}, errors.New("down") },
		Host:      fakeHost{},
		Processes: failingProcs{},
	})
	if len(r.Host.Missing) != 1 || r.Host.Missing[0] != domain.MetricCPU || r.Host.Derived != nil {
		t.Errorf("host = %+v, want cpu missing", r.Host)
	}
}

func TestCollectUnattributedJava(t *testing.T) {
	heavy := []process.WorkloadView{
		{Key: domain.ProcessKey{PID: 40}, Name: "java", CPU: 2.5},
		{Key: domain.ProcessKey{PID: 41}, Name: "java", Owner: "claude-1", CPU: 3}, // under an agent
		{Key: domain.ProcessKey{PID: 42}, Name: "java"},                            // idle
		{Key: domain.ProcessKey{PID: 43}, Name: "secret-tool", CPU: 4},             // not java
		{Key: domain.ProcessKey{PID: 44}, Name: "java", CPU: 1},
		{Key: domain.ProcessKey{PID: 45}, Name: "java", CPU: 1},
		{Key: domain.ProcessKey{PID: 46}, Name: "java", CPU: 1}, // over the cap
	}
	collect := func(heavy []process.WorkloadView) Report {
		return Collect(context.Background(), Inputs{
			Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{Version: "v1"}, nil },
			Status:    func(context.Context) (app.StatusResult, error) { return app.StatusResult{Heavy: heavy}, nil },
			Host:      fakeHost{},
			Processes: fakeProcs{},
		})
	}
	r := collect(heavy)
	if len(r.Findings) != maxDaemonFindings {
		t.Fatalf("findings = %q", r.Findings)
	}
	want := "unattributed java (pid 40, 250% CPU) is probably a Gradle or Kotlin daemon: builds there cannot be queued; " +
		"set org.gradle.daemon=false in ~/.gradle/gradle.properties so they run under the agent " +
		"(for the Kotlin daemon also kotlin.compiler.execution.strategy=in-process)"
	if r.Findings[0] != want || !strings.Contains(r.Findings[1], "pid 44,") || !strings.Contains(r.Findings[2], "pid 45,") {
		t.Errorf("findings = %q", r.Findings)
	}
	if b, _ := json.Marshal(r); strings.Contains(string(b), "secret-tool") {
		t.Errorf("report leaks a process name: %s", b)
	}
	r = collect(heavy[1:4])
	if b, _ := json.Marshal(r); r.Findings != nil || strings.Contains(string(b), "findings") {
		t.Errorf("findings without busy unattributed java: %s", b)
	}
}
