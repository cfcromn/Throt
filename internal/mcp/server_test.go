package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

type fakeClient struct {
	status    app.StatusResult
	events    []domain.Event
	err       error
	lastLimit int
}

func (f *fakeClient) Status(context.Context) (app.StatusResult, error) { return f.status, f.err }

func (f *fakeClient) Events(_ context.Context, limit int) ([]domain.Event, error) {
	f.lastLimit = limit
	return f.events, f.err
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// serve runs the server over input and returns each output line decoded.
func serve(t *testing.T, c Client, input string) []message {
	t.Helper()
	var out bytes.Buffer
	s := New(c, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var msgs []message
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("output line is not JSON: %q: %v", sc.Text(), err)
		}
		if m.JSONRPC != "2.0" {
			t.Fatalf("jsonrpc = %q", m.JSONRPC)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func one(t *testing.T, c Client, input string) message {
	t.Helper()
	msgs := serve(t, c, input)
	if len(msgs) != 1 {
		t.Fatalf("got %d responses, want 1: %+v", len(msgs), msgs)
	}
	return msgs[0]
}

func TestInitializeVersionNegotiation(t *testing.T) {
	tests := []struct {
		name, params, want string
	}{
		{"echo 2025-06-18", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`, "2025-06-18"},
		{"echo 2024-11-05", `{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		{"echo latest", `{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		{"unknown future", `{"protocolVersion":"2099-01-01"}`, latestVersion},
		{"handshake-free revision", `{"protocolVersion":"2026-07-28"}`, latestVersion},
		{"missing", `{}`, latestVersion},
		{"no params", ``, latestVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := `{"jsonrpc":"2.0","id":1,"method":"initialize"`
			if tt.params != "" {
				req += `,"params":` + tt.params
			}
			m := one(t, &fakeClient{}, req+"}\n")
			if string(m.ID) != "1" || m.Error != nil {
				t.Fatalf("id %s err %+v", m.ID, m.Error)
			}
			var r struct {
				ProtocolVersion string                     `json:"protocolVersion"`
				Capabilities    map[string]json.RawMessage `json:"capabilities"`
				ServerInfo      struct{ Name, Version string }
			}
			if err := json.Unmarshal(m.Result, &r); err != nil {
				t.Fatal(err)
			}
			if r.ProtocolVersion != tt.want {
				t.Errorf("protocolVersion = %q, want %q", r.ProtocolVersion, tt.want)
			}
			if string(r.Capabilities["tools"]) != "{}" {
				t.Errorf("capabilities = %v", r.Capabilities)
			}
			if r.ServerInfo.Name != "throt" || r.ServerInfo.Version != "1.2.3" {
				t.Errorf("serverInfo = %+v", r.ServerInfo)
			}
		})
	}
}

func TestInitializeBadParams(t *testing.T) {
	m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":7}}`+"\n")
	if m.Error == nil || m.Error.Code != codeInvalidParams {
		t.Fatalf("error = %+v", m.Error)
	}
}

func TestHandshakeSequence(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":"p","method":"ping"}` + "\n"
	msgs := serve(t, &fakeClient{}, in)
	if len(msgs) != 2 {
		t.Fatalf("got %d responses, want 2", len(msgs))
	}
	if string(msgs[1].ID) != `"p"` || string(msgs[1].Result) != "{}" {
		t.Fatalf("ping = %+v", msgs[1])
	}
}

func TestToolsList(t *testing.T) {
	m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	var r struct {
		Tools []struct {
			Name        string
			Description string
			InputSchema json.RawMessage
			Annotations map[string]any
		}
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range r.Tools {
		names[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("%s: empty description", tl.Name)
		}
		var schema struct {
			Type                 string
			Properties           map[string]any
			AdditionalProperties *bool
		}
		if !json.Valid(tl.InputSchema) {
			t.Fatalf("%s: schema is not valid JSON", tl.Name)
		}
		if err := json.Unmarshal(tl.InputSchema, &schema); err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		if schema.Type != "object" || schema.Properties == nil || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: schema = %s", tl.Name, tl.InputSchema)
		}
		if tl.Annotations["readOnlyHint"] != true {
			t.Errorf("%s: not marked read-only", tl.Name)
		}
		for _, bad := range []string{"pause", "resume", "config"} {
			if strings.Contains(tl.Name, bad) {
				t.Errorf("tool %s must not exist: control stays with the CLI", tl.Name)
			}
		}
	}
	if len(r.Tools) != 3 || !names["throt_status"] || !names["throt_capacity"] || !names["throt_events"] {
		t.Fatalf("tools = %v", names)
	}
}

func TestProtocolErrors(t *testing.T) {
	tests := []struct {
		name, in string
		wantID   string
		wantCode int
	}{
		{"unknown method", `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, "3", codeMethodNotFound},
		{"malformed JSON", `{"jsonrpc":"2.0","id":4,`, "null", codeParseError},
		{"batch array", `[{"jsonrpc":"2.0","id":5,"method":"ping"}]`, "null", codeInvalidRequest},
		{"wrong jsonrpc", `{"jsonrpc":"1.0","id":6,"method":"ping"}`, "6", codeInvalidRequest},
		{"unknown tool", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"throt_pause"}}`, "7", codeInvalidParams},
		{"call params not object", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":[1]}`, "8", codeInvalidParams},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := one(t, &fakeClient{}, tt.in+"\n")
			if string(m.ID) != tt.wantID || m.Error == nil || m.Error.Code != tt.wantCode || m.Result != nil {
				t.Fatalf("got id %s err %+v result %s", m.ID, m.Error, m.Result)
			}
		})
	}
}

func TestNoReply(t *testing.T) {
	tests := []struct{ name, in string }{
		{"initialized notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{"unknown notification", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`},
		{"notification for a request method", `{"jsonrpc":"2.0","method":"tools/list"}`},
		{"client response", `{"jsonrpc":"2.0","id":9,"result":{}}`},
		{"blank lines", "\n\r\n"},
		{"empty input", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msgs := serve(t, &fakeClient{}, tt.in+"\n"); len(msgs) != 0 {
				t.Fatalf("got %d responses, want none: %+v", len(msgs), msgs)
			}
		})
	}
}

func TestOversizeLine(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxLine) + `"}}`
	in := big + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	msgs := serve(t, &fakeClient{}, in)
	if len(msgs) != 2 {
		t.Fatalf("got %d responses, want 2", len(msgs))
	}
	if msgs[0].Error == nil || msgs[0].Error.Code != codeParseError || string(msgs[0].ID) != "null" {
		t.Fatalf("oversize: %+v", msgs[0])
	}
	if string(msgs[1].ID) != "2" || msgs[1].Error != nil {
		t.Fatalf("server did not recover after oversize line: %+v", msgs[1])
	}
	// A line of exactly maxLine bytes is accepted.
	pad := maxLine - len(`{"jsonrpc":"2.0","id":3,"method":"ping","params":{"x":""}}`)
	exact := `{"jsonrpc":"2.0","id":3,"method":"ping","params":{"x":"` + strings.Repeat("a", pad) + `"}}`
	if m := one(t, &fakeClient{}, exact+"\r\n"); m.Error != nil {
		t.Fatalf("max-size line rejected: %+v", m.Error)
	}
	// An oversize final line without a newline is still reported.
	if m := one(t, &fakeClient{}, big); m.Error == nil || m.Error.Code != codeParseError {
		t.Fatalf("oversize at EOF: %+v", m)
	}
}

func TestEOFWithoutNewline(t *testing.T) {
	m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if string(m.ID) != "1" {
		t.Fatalf("got %+v", m)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestServeIOErrors(t *testing.T) {
	s := New(&fakeClient{}, "dev", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Serve(context.Background(), errReader{}, io.Discard); err == nil {
		t.Error("read error not returned")
	}
	if err := s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), errWriter{}); err == nil {
		t.Error("write error not returned")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Serve(ctx, strings.NewReader("\n"), io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// call runs one tools/call and decodes its result.
func call(t *testing.T, c Client, name, args string) (toolResult, json.RawMessage) {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"` + name + `"`
	if args != "" {
		req += `,"arguments":` + args
	}
	m := one(t, c, req+"}}\n")
	if m.Error != nil {
		t.Fatalf("rpc error: %+v", m.Error)
	}
	var r struct {
		Content           []content       `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text == "" {
		t.Fatalf("content = %+v", r.Content)
	}
	return toolResult{Content: r.Content, IsError: r.IsError}, r.StructuredContent
}

func key(pid int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: time.Unix(int64(pid), 0).UTC()}
}

// sampleStatus has two agents and three heavy workloads: one running for
// claude-1, one running for codex-1, and one paused for claude-1.
func sampleStatus(level domain.PressureLevel, mode scheduler.Mode, limit int) app.StatusResult {
	return app.StatusResult{
		Pressure: level,
		Reason:   "memory_used>=0.9",
		Host: &domain.HostSample{
			CPUUsage: 0.74, Load1: 1.12, MemTotal: 32 << 30, MemAvailable: 2 << 30,
			SwapTotal: 4 << 30, SwapUsed: 1 << 30, Latency: 12 * time.Millisecond,
		},
		Agents: []process.AgentView{
			{Key: key(100), Agent: "claude-1", Name: "claude", Children: 7, CPU: 1.04, RSS: 1800 << 20},
			{Key: key(200), Agent: "codex-1", Name: "codex", Children: 5, CPU: 2.31, RSS: 3 << 30},
		},
		Heavy: []process.WorkloadView{
			{Key: key(101), Name: "java", Class: domain.ClassBuild, Owner: "claude-1", CPU: 1.9, RSS: 2 << 30},
			{Key: key(201), Name: "go", Class: domain.ClassTest, Owner: "codex-1", CPU: 0.5, RSS: 500 << 20},
			{Key: key(102), Name: "buildkitd", Class: domain.ClassContainerBuild, Owner: "claude-1"},
			{Key: key(300), Name: "make", Class: domain.ClassBuild},
		},
		Paused:    []control.PausedWorkload{{Key: key(102), Name: "buildkitd", Owner: "claude-1"}, {Key: key(300)}},
		Scheduler: app.SchedulerStatus{Mode: mode, HeavyLimit: limit, Queued: []domain.ProcessKey{key(300)}},
	}
}

func TestStatusTool(t *testing.T) {
	c := &fakeClient{status: sampleStatus(domain.PressureHigh, scheduler.ModeEnforce, 3)}
	r, sc := call(t, c, "throt_status", "")
	if r.IsError {
		t.Fatalf("isError: %s", r.Content[0].Text)
	}
	text := r.Content[0].Text
	for _, want := range []string{
		"pressure: high (memory_used>=0.9)", "cpu: 74%", "1.12 per CPU", "30.0 GiB used of 32.0 GiB",
		"1.0 GiB used of 4.0 GiB", "latency: 12ms", "scheduler: enforce (heavy limit 3, 1 queued)",
		"claude-1 pid 100: cpu 104%, mem 1.8 GiB, 7 children", "claude-1 pid 101 java (build): running",
		"claude-1 pid 102 buildkitd (container-build): PAUSED", "- pid 300 make (build): QUEUED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	var v statusView
	if err := json.Unmarshal(sc, &v); err != nil {
		t.Fatal(err)
	}
	if v.Pressure != "high" || len(v.Agents) != 2 || len(v.Heavy) != 4 || v.Heavy[3].State != stateQueued ||
		v.Scheduler.Mode != "enforce" || v.Host == nil || v.Host.CPU != "74%" {
		t.Fatalf("structured = %+v", v)
	}
}

func TestStatusToolSparse(t *testing.T) {
	st := app.StatusResult{Degraded: "process sampler failing", Scheduler: app.SchedulerStatus{Mode: scheduler.ModeOff, HeavyLimit: 2}}
	for i := range maxTextRows + 2 {
		st.Agents = append(st.Agents, process.AgentView{Key: key(1000 + i), Agent: "a"})
		st.Heavy = append(st.Heavy, process.WorkloadView{Key: key(2000 + i), Name: "go"})
	}
	r, sc := call(t, &fakeClient{status: st}, "throt_status", "{}")
	text := r.Content[0].Text
	for _, want := range []string{"pressure: unknown", "degraded: process sampler failing", "host: no sample yet", "... and 2 more"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(string(sc), `"host":null`) {
		t.Errorf("structured host = %s", sc)
	}

	st = app.StatusResult{Host: &domain.HostSample{Missing: []domain.Metric{domain.MetricCPU, domain.MetricSwap, domain.MetricLatency}}}
	r, _ = call(t, &fakeClient{status: st}, "throt_status", "")
	if !strings.Contains(r.Content[0].Text, "cpu: n/a") || !strings.Contains(r.Content[0].Text, "swap: n/a") {
		t.Errorf("missing metrics not n/a:\n%s", r.Content[0].Text)
	}
}

func TestCapacity(t *testing.T) {
	tests := []struct {
		name           string
		status         app.StatusResult
		args           string
		wantOK         bool
		wantRunning    int
		wantAgent      *int
		reason, suggst string
		keepQueued     bool // sampleStatus queues pid 300; most cases clear it
	}{
		{
			name:   "normal, slot free",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3),
			wantOK: true, wantRunning: 2, reason: "2 of 3 heavy slots in use", suggst: "ok to start",
		},
		{
			name:   "elevated is ok with caveat",
			status: sampleStatus(domain.PressureElevated, scheduler.ModeObserve, 3),
			wantOK: true, wantRunning: 2, reason: "elevated", suggst: "avoid starting several",
		},
		{
			name:   "high pressure",
			status: sampleStatus(domain.PressureHigh, scheduler.ModeEnforce, 3),
			wantOK: false, wantRunning: 2,
			reason: "pressure is high (memory_used>=0.9)",
			suggst: "pressure is high (memory_used>=0.9); wait or run a lighter task",
		},
		{
			name:   "critical pressure with scheduler off",
			status: sampleStatus(domain.PressureCritical, scheduler.ModeOff, 1),
			wantOK: false, wantRunning: 2, reason: "pressure is critical", suggst: "wait or run a lighter task",
		},
		{
			name:   "at limit, enforce",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 2),
			wantOK: false, wantRunning: 2, reason: "all heavy slots are in use (2 running, limit 2)", suggst: "queued (paused) by throtd",
		},
		{
			name:   "above limit, observe",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeObserve, 1),
			wantOK: false, wantRunning: 2, reason: "all heavy slots are in use", suggst: "wait for a running heavy job",
		},
		{
			name:   "scheduler off ignores limit",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeOff, 1),
			wantOK: true, wantRunning: 2, reason: "scheduler is off", suggst: "ok to start",
		},
		{
			name:   "empty mode means off",
			status: sampleStatus(domain.PressureNormal, "", 1),
			wantOK: true, wantRunning: 2, reason: "scheduler is off",
		},
		{
			name:   "unknown pressure fails open",
			status: sampleStatus(domain.PressureUnknown, scheduler.ModeEnforce, 3),
			wantOK: true, wantRunning: 2, reason: "unknown", suggst: "check again",
		},
		{
			name:   "agent filter counts only running work of that agent",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3),
			args:   `{"agent":"claude-1"}`,
			wantOK: true, wantRunning: 2, wantAgent: ptr(1), suggst: "ok to start",
		},
		{
			name:   "other agent",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3),
			args:   `{"agent":"codex-1"}`,
			wantOK: true, wantRunning: 2, wantAgent: ptr(1),
		},
		{
			name:   "unknown agent",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3),
			args:   `{"agent":"ghost"}`,
			wantOK: true, wantRunning: 2, wantAgent: ptr(0), suggst: `agent "ghost" is not currently detected`,
		},
		{
			name:   "queued work waits first",
			status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3), keepQueued: true,
			wantOK: false, wantRunning: 2, reason: "1 heavy jobs are already queued", suggst: "queue behind waiting ones",
		},
		{
			name: "agent at its budget",
			status: func() app.StatusResult {
				st := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3)
				st.Scheduler.PerAgentLimit = 1
				return st
			}(),
			args:   `{"agent":"claude-1"}`,
			wantOK: false, wantRunning: 2, wantAgent: ptr(1), reason: "per-agent limit 1", suggst: "budget",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.keepQueued {
				tt.status.Scheduler.Queued = nil
			}
			r, sc := call(t, &fakeClient{status: tt.status}, "throt_capacity", tt.args)
			if r.IsError {
				t.Fatalf("isError: %s", r.Content[0].Text)
			}
			var v capacityView
			if err := json.Unmarshal(sc, &v); err != nil {
				t.Fatal(err)
			}
			if v.OKToStart != tt.wantOK || v.RunningHeavy != tt.wantRunning || v.HeavyLimit != tt.status.Scheduler.HeavyLimit {
				t.Errorf("got %+v", v)
			}
			if !strings.Contains(v.Reason, tt.reason) || !strings.Contains(v.Suggestion, tt.suggst) {
				t.Errorf("reason %q / suggestion %q", v.Reason, v.Suggestion)
			}
			switch {
			case tt.wantAgent == nil && v.AgentRunningHeavy != nil:
				t.Errorf("unexpected agent count %d", *v.AgentRunningHeavy)
			case tt.wantAgent != nil && (v.AgentRunningHeavy == nil || *v.AgentRunningHeavy != *tt.wantAgent):
				t.Errorf("agent count = %v, want %d", v.AgentRunningHeavy, *tt.wantAgent)
			}
			text := r.Content[0].Text
			verdict := "ok to start a heavy job: NO"
			if tt.wantOK {
				verdict = "ok to start a heavy job: YES"
			}
			if !strings.Contains(text, verdict) || !strings.Contains(text, "advisory") {
				t.Errorf("text:\n%s", text)
			}
			if tt.wantAgent != nil && !strings.Contains(text, "running heavy for ") {
				t.Errorf("text lacks agent count:\n%s", text)
			}
		})
	}
}

func ptr(i int) *int { return &i }

func TestCapacityDefaultsToParentAgent(t *testing.T) {
	st := sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3)
	st.Scheduler.Queued = nil
	s := New(&fakeClient{status: st}, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.parent = st.Agents[0].Key.PID
	r := s.capacity(context.Background(), "")
	if !strings.Contains(r.Content[0].Text, "running heavy for "+st.Agents[0].Agent) {
		t.Fatalf("text:\n%s", r.Content[0].Text)
	}
}

func TestEventsTool(t *testing.T) {
	ts := time.Date(2026, 10, 28, 14, 2, 52, 0, time.UTC)
	events := []domain.Event{
		{Time: ts, Kind: domain.EventPressureChanged, Message: "pressure elevated -> high"},
		{Time: ts.Add(time.Second), Kind: "daemon_started"},
	}
	tests := []struct {
		name      string
		args      string
		events    []domain.Event
		wantLimit int
		want      []string
	}{
		{"default limit", "", events, 20, []string{"2026-10-28T14:02:52Z  pressure_changed  pressure elevated -> high", "2026-10-28T14:02:53Z  daemon_started"}},
		{"explicit limit trims to newest", `{"limit":1}`, events, 1, []string{"daemon_started"}},
		{"max limit", `{"limit":200}`, events, 200, []string{"pressure_changed"}},
		{"no events", `{}`, nil, 20, []string{"no events"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClient{events: tt.events}
			r, sc := call(t, c, "throt_events", tt.args)
			if r.IsError || c.lastLimit != tt.wantLimit {
				t.Fatalf("isError %v limit %d: %s", r.IsError, c.lastLimit, r.Content[0].Text)
			}
			for _, w := range tt.want {
				if !strings.Contains(r.Content[0].Text, w) {
					t.Errorf("text missing %q:\n%s", w, r.Content[0].Text)
				}
			}
			if tt.args == `{"limit":1}` && strings.Contains(r.Content[0].Text, "pressure_changed") {
				t.Errorf("limit not applied:\n%s", r.Content[0].Text)
			}
			var v struct{ Events []domain.Event }
			if err := json.Unmarshal(sc, &v); err != nil || v.Events == nil {
				t.Errorf("structured = %s (%v)", sc, err)
			}
		})
	}
}

func TestInvalidArguments(t *testing.T) {
	tests := []struct{ tool, args, want string }{
		{"throt_events", `{"limit":0}`, "between 1 and 200"},
		{"throt_events", `{"limit":201}`, "between 1 and 200"},
		{"throt_events", `{"limit":"5"}`, "invalid arguments"},
		{"throt_events", `{"count":5}`, "invalid arguments"},
		{"throt_capacity", `{"agent":"a","pid":1}`, "invalid arguments"},
		{"throt_status", `{"verbose":true}`, "invalid arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.tool+tt.args, func(t *testing.T) {
			r, _ := call(t, &fakeClient{}, tt.tool, tt.args)
			if !r.IsError || !strings.Contains(r.Content[0].Text, tt.want) {
				t.Fatalf("got isError %v: %s", r.IsError, r.Content[0].Text)
			}
		})
	}
	// null arguments are the same as none.
	if r, _ := call(t, &fakeClient{}, "throt_status", "null"); r.IsError {
		t.Fatalf("null arguments rejected: %s", r.Content[0].Text)
	}
}

func TestDaemonDown(t *testing.T) {
	down := errors.New("connect to throtd at /tmp/x/throtd.sock (is it running?): no such file")
	for _, name := range []string{"throt_status", "throt_capacity", "throt_events"} {
		t.Run(name, func(t *testing.T) {
			r, sc := call(t, &fakeClient{err: down}, name, "")
			if !r.IsError {
				t.Fatal("isError not set")
			}
			if !strings.Contains(r.Content[0].Text, "throtd &") || !strings.Contains(r.Content[0].Text, "is it running?") {
				t.Errorf("text = %s", r.Content[0].Text)
			}
			if len(sc) != 0 {
				t.Errorf("structuredContent on error: %s", sc)
			}
		})
	}
}

func TestBytesHuman(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{{0, "0 MiB"}, {512 << 20, "512 MiB"}, {1 << 30, "1.0 GiB"}, {3 << 29, "1.5 GiB"}}
	for _, tt := range tests {
		if got := bytesHuman(tt.in); got != tt.want {
			t.Errorf("bytesHuman(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
