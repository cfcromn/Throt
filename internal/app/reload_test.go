package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// reloadEnv is a daemon over a fake world, started from a config file that
// tests rewrite and reload.
type reloadEnv struct {
	t    *testing.T
	path string
	sock string
	w    *world
	d    *Daemon
}

// file is the config a test writes: fixed base settings plus Scheduler
// overrides, Agents, and Extra top-level keys, or Raw verbatim.
type file struct {
	Scheduler map[string]any
	Agents    []config.Agent
	Extra     map[string]any
	Raw       string
}

func (e *reloadEnv) write(f file) {
	e.t.Helper()
	body := f.Raw
	if body == "" {
		m := map[string]any{
			"socket_path":     e.sock,
			"sample_interval": "100ms",
			"profile":         "performance",
			"scheduler":       map[string]any{"mode": "off", "heavy_limit": 1, "resume_cooldown": "0s"},
		}
		for k, v := range f.Scheduler {
			m["scheduler"].(map[string]any)[k] = v
		}
		if f.Agents != nil {
			m["agents"] = f.Agents
		}
		for k, v := range f.Extra {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			e.t.Fatal(err)
		}
		body = string(b)
	}
	if err := os.WriteFile(e.path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func startReloadEnv(t *testing.T, w *world, f file) *reloadEnv {
	t.Helper()
	dir := shortTempDir(t)
	e := &reloadEnv{t: t, path: filepath.Join(dir, "config.json"), sock: filepath.Join(dir, "throtd.sock"), w: w}
	e.write(f)
	cfg, err := config.Load(e.path)
	if err != nil {
		t.Fatal(err)
	}
	e.d = NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.d.SetConfigPath(e.path)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.d.Run(ctx) }()
	t.Cleanup(func() { stop(); <-done })
	debugEvents = e.events
	t.Cleanup(func() { debugEvents = nil })
	waitFor(t, "daemon up", func() bool { return transport.Call(context.Background(), e.sock, "ping", nil, nil) == nil })
	return e
}

// reload calls the privileged protocol method, as `throt reload` does.
func (e *reloadEnv) reload() (ReloadResult, error) {
	var r ReloadResult
	err := transport.Call(context.Background(), e.sock, "reload", nil, &r)
	return r, err
}

func (e *reloadEnv) mustReload() ReloadResult {
	e.t.Helper()
	r, err := e.reload()
	if err != nil {
		e.t.Fatalf("reload: %v", err)
	}
	return r
}

func (e *reloadEnv) status() StatusResult {
	var st StatusResult
	transport.Call(context.Background(), e.sock, "status", nil, &st)
	return st
}

func (e *reloadEnv) events() []domain.Event {
	var events []domain.Event
	transport.Call(context.Background(), e.sock, "events", nil, &events)
	return events
}

func (e *reloadEnv) lastEvent(kind domain.EventKind) (domain.Event, bool) {
	events := e.events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			return events[i], true
		}
	}
	return domain.Event{}, false
}

func TestReloadSchedulerMode(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	e := startReloadEnv(t, w, file{})
	waitFor(t, "normal pressure", func() bool {
		st := e.status()
		return len(st.Heavy) == 1 && st.Pressure == domain.PressureNormal
	})

	// off -> enforce: the running build is pre-existing, a new one queues.
	e.write(file{Scheduler: map[string]any{"mode": "enforce"}})
	r := e.mustReload()
	if !slices.Equal(r.Changed, []string{"scheduler.mode off -> enforce"}) || len(r.RequiresRestart) != 0 {
		t.Fatalf("reload = %+v", r)
	}
	if ev, ok := e.lastEvent(domain.EventConfigReloaded); !ok || ev.Message != "config reloaded: scheduler.mode off -> enforce" {
		t.Fatalf("config_reloaded event = %+v", ev)
	}
	waitFor(t, "enforce applied", func() bool { return e.status().Scheduler.Mode == "enforce" })
	if got := w.signals(); got != "" {
		t.Fatalf("pre-existing workload was signaled: %q", got)
	}
	w.add(domain.ProcessSample{PID: 12, PPID: 10, Name: "go"})
	waitFor(t, "pid 12 queued", func() bool {
		q := e.status().Scheduler.Queued
		return len(q) == 1 && q[0].PID == 12
	})

	// enforce -> off: the queued build is resumed rather than stranded.
	e.write(file{Scheduler: map[string]any{"mode": "off"}})
	e.mustReload()
	waitFor(t, "queued work resumed", func() bool { return len(e.status().Scheduler.Queued) == 0 })
	if got := w.signals(); got != "stop:12 cont:12" {
		t.Fatalf("signals = %q, want stop:12 cont:12", got)
	}
	ev, ok := e.lastEvent(domain.EventSchedulerDecision)
	if !ok || ev.Attrs["reason"] != ReasonEnforceDisabled || ev.Attrs["pid"] != "12" {
		t.Fatalf("resume event = %+v", ev)
	}
	if st := e.status(); st.Scheduler.Mode != "off" || len(st.Paused) != 0 {
		t.Fatalf("status = %+v", st.Scheduler)
	}
}

func TestReloadKeepsManualPauses(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
	)
	e := startReloadEnv(t, w, file{Scheduler: map[string]any{"mode": "enforce"}})
	waitFor(t, "heavy seen", func() bool { return len(e.status().Heavy) == 1 })
	if err := transport.Call(context.Background(), e.sock, "pause", ControlParams{PID: 11}, nil); err != nil {
		t.Fatal(err)
	}
	e.write(file{Scheduler: map[string]any{"mode": "off"}})
	e.mustReload()
	waitFor(t, "off applied", func() bool { return e.status().Scheduler.Mode == "off" })
	if st := e.status(); len(st.Paused) != 1 || st.Paused[0].Key.PID != 11 {
		t.Fatalf("manual pause was not kept: %+v", st.Paused)
	}
	if got := w.signals(); got != "stop:11" {
		t.Fatalf("signals = %q, want stop:11", got)
	}
}

func TestReloadAgents(t *testing.T) {
	w := newWorld(
		domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
		domain.ProcessSample{PID: 20, PPID: 1, Name: "my-agent"},
		domain.ProcessSample{PID: 21, PPID: 20, Name: "go"},
	)
	e := startReloadEnv(t, w, file{})
	agents := func() map[int]string {
		m := map[int]string{}
		for _, a := range e.status().Agents {
			m[a.Key.PID] = a.Agent
		}
		return m
	}
	waitFor(t, "claude seen", func() bool { return agents()[10] == "claude-1" })

	e.write(file{Agents: []config.Agent{{Name: "mine", Executables: []string{"my-agent"}}}})
	r := e.mustReload()
	if !slices.Equal(r.Changed, []string{"agents [] -> [mine(my-agent)]"}) {
		t.Fatalf("reload = %+v", r)
	}
	waitFor(t, "custom agent", func() bool {
		st := e.status()
		return len(st.Heavy) == 1 && st.Heavy[0].Owner == "mine-1"
	})
	if got := agents(); got[10] != "claude-1" || got[20] != "mine-1" {
		t.Fatalf("agents = %v", got)
	}

	// Renaming the adapter relabels its agent; the built-in one is untouched.
	e.write(file{Agents: []config.Agent{{Name: "ours", Executables: []string{"my-agent"}}}})
	e.mustReload()
	waitFor(t, "relabeled", func() bool { return agents()[20] == "ours-1" })
	if got := agents(); got[10] != "claude-1" {
		t.Fatalf("agents = %v", got)
	}
	var lost, found bool
	for _, ev := range e.events() {
		lost = lost || ev.Kind == domain.EventAgentLost && ev.Attrs["agent"] == "mine-1"
		found = found || ev.Kind == domain.EventAgentDiscovered && ev.Attrs["agent"] == "ours-1"
	}
	if !lost || !found {
		t.Fatalf("lost mine-1 = %v, found ours-1 = %v", lost, found)
	}
}

func TestReloadInvalidChangesNothing(t *testing.T) {
	w := newWorld(domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"})
	e := startReloadEnv(t, w, file{})
	for name, f := range map[string]file{
		"invalid value": {Scheduler: map[string]any{"mode": "bogus"}},
		"unknown key":   {Extra: map[string]any{"sample_intervl": "1s"}},
		"bad json":      {Raw: `{"scheduler": {"mode": "enforce"`},
	} {
		t.Run(name, func(t *testing.T) {
			e.write(f)
			if _, err := e.reload(); err == nil {
				t.Fatal("reload succeeded")
			}
			ev, ok := e.lastEvent(domain.EventConfigReloadFailed)
			if !ok || ev.Attrs["error"] == "" {
				t.Fatalf("config_reload_failed event = %+v", ev)
			}
		})
	}
	// Nothing changed: a valid reload diffs against the original config.
	e.write(file{Scheduler: map[string]any{"mode": "observe"}})
	if r := e.mustReload(); !slices.Equal(r.Changed, []string{"scheduler.mode off -> observe"}) {
		t.Fatalf("reload = %+v", r)
	}
	waitFor(t, "observe applied", func() bool { return e.status().Scheduler.Mode == "observe" })
}

func TestReloadRestartOnlyKeys(t *testing.T) {
	w := newWorld(domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"})
	e := startReloadEnv(t, w, file{})
	other := filepath.Join(filepath.Dir(e.sock), "other.sock")
	e.write(file{
		Scheduler: map[string]any{"heavy_limit": 3},
		Extra:     map[string]any{"socket_path": other, "sample_interval": "1s", "event_history": 50},
	})
	r := e.mustReload()
	want := []string{"socket_path " + e.sock + " -> " + other, "sample_interval 100ms -> 1s", "event_history 1000 -> 50"}
	if !slices.Equal(r.RequiresRestart, want) || !slices.Equal(r.Changed, []string{"scheduler.heavy_limit 1 -> 3"}) {
		t.Fatalf("reload = %+v", r)
	}
	ev, ok := e.lastEvent(domain.EventConfigReloaded)
	if !ok || ev.Attrs["requires_restart"] != "socket_path,sample_interval,event_history" || !strings.Contains(ev.Message, "requires restart") {
		t.Fatalf("config_reloaded event = %+v", ev)
	}
	// The daemon keeps its socket, and reports the keys again next time.
	waitFor(t, "limit applied", func() bool { return e.status().Scheduler.HeavyLimit == 3 })
	if _, err := os.Stat(other); err == nil {
		t.Fatal("daemon listened on the new socket")
	}
	if r := e.mustReload(); !slices.Equal(r.RequiresRestart, want) || len(r.Changed) != 0 {
		t.Fatalf("second reload = %+v", r)
	}
}

func TestReloadWithoutConfigPath(t *testing.T) {
	cfg := config.Default()
	d := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := d.Reload(); err == nil {
		t.Fatal("Reload without a config path succeeded")
	}
	if ev := d.events.Recent(1); len(ev) != 1 || ev[0].Kind != domain.EventConfigReloadFailed {
		t.Fatalf("events = %+v", ev)
	}
}

func TestReloadSupersedesPendingPolicy(t *testing.T) {
	// Two reloads before a tick never block, and the newest policy wins.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetConfigPath(path)
	for _, mode := range []string{"observe", "enforce"} {
		if err := os.WriteFile(path, []byte(`{"scheduler":{"mode":"`+mode+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { d.Reload(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Reload blocked")
		}
	}
	d.applyPolicy(context.Background())
	if got := d.sched.Policy().Mode; got != "enforce" {
		t.Fatalf("mode = %s, want enforce", got)
	}
}

func TestReloadIsPrivileged(t *testing.T) {
	// The transport tests cover refusing a foreign uid for every method
	// marked with WithPrivileged; this pins reload to that list.
	if !slices.Contains(privilegedMethods(), "reload") {
		t.Fatalf("privileged methods = %v, want reload included", privilegedMethods())
	}
}

func TestReloadAgentsRebaselinesScheduler(t *testing.T) {
	// New adapters can split a workload into new keys; the scheduler must
	// treat them as pre-existing, so an agents-only change re-sends the policy.
	path := filepath.Join(t.TempDir(), "config.json")
	d := NewDaemon(config.Default(), fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SetConfigPath(path)
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"myagent","executables":["my-agent"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Reload(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.policyCh:
	default:
		t.Fatal("agents-only reload did not hand the tick a policy to rebaseline")
	}
}
