package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

const testSocket = "/run/user/1000/throt/throtd.sock"

// fakeClient returns scripted results; it never touches a socket.
type fakeClient struct {
	status app.StatusResult
	events []domain.Event
	err    error

	// sinces records the seq of every events call.
	sinces []uint64
	// calls records pause and resume requests as "pause 2000".
	calls      []string
	keys       []domain.ProcessKey // full keys sent with those requests
	controlErr error
}

func (f *fakeClient) Status(context.Context) (app.StatusResult, error) {
	return f.status, f.err
}

// EventsSince mirrors the daemon: newest limit events for seq 0, else the
// oldest limit events after seq, or the newest when seq is ahead of them all.
func (f *fakeClient) EventsSince(_ context.Context, seq uint64, limit int) ([]domain.Event, error) {
	f.sinces = append(f.sinces, seq)
	if f.err != nil {
		return nil, f.err
	}
	newest := f.events[max(0, len(f.events)-limit):]
	if seq == 0 {
		return newest, nil
	}
	i := slices.IndexFunc(f.events, func(e domain.Event) bool { return e.Seq > seq })
	if i >= 0 {
		return f.events[i:min(len(f.events), i+limit)], nil
	}
	if len(f.events) > 0 && f.events[len(f.events)-1].Seq < seq {
		return newest, nil
	}
	return nil, nil
}

func (f *fakeClient) Pause(_ context.Context, k domain.ProcessKey) error {
	f.calls = append(f.calls, fmt.Sprint("pause ", k.PID))
	f.keys = append(f.keys, k)
	return f.controlErr
}

func (f *fakeClient) Resume(_ context.Context, k domain.ProcessKey) error {
	f.calls = append(f.calls, fmt.Sprint("resume ", k.PID))
	f.keys = append(f.keys, k)
	return f.controlErr
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]")

// view returns the rendered screen without ANSI styling.
func view(m Model) string { return ansiRE.ReplaceAllString(m.View().Content, "") }

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

// poll runs one poll command synchronously, as the program would, without
// any real tick.
func poll(t *testing.T, m Model) Model {
	t.Helper()
	return update(t, m, m.poll()())
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "ctrl+c":
			msg = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		m = update(t, m, msg)
	}
	return m
}

func newModel(t *testing.T, c *fakeClient, w, h int) Model {
	t.Helper()
	m := update(t, New(c, testSocket), tea.WindowSizeMsg{Width: w, Height: h})
	return poll(t, m)
}

func sampleStatus(agents, heavy int) app.StatusResult {
	start := time.Date(2026, 10, 28, 9, 30, 0, 0, time.Local)
	st := app.StatusResult{
		Pressure: domain.PressureHigh,
		Reason:   "memory used 0.91 >= 0.90",
		Host: &domain.HostSample{
			CPUUsage: 0.74, Load1: 0.85,
			MemTotal: 16 << 30, MemAvailable: 4 << 30,
			SwapTotal: 4 << 30, SwapUsed: 1 << 30,
		},
	}
	for i := range agents {
		st.Agents = append(st.Agents, process.AgentView{
			Key:   domain.ProcessKey{PID: 1000 + i, Start: start},
			Agent: fmt.Sprintf("claude-%d", i+1), Name: "claude",
			Children: i, CPU: 0.5, RSS: 1 << 30,
		})
	}
	for i := range heavy {
		st.Heavy = append(st.Heavy, process.WorkloadView{
			Key:  domain.ProcessKey{PID: 2000 + i, Start: start},
			Name: "gradle", Class: domain.ClassBuild,
			Owner: "claude-1", OwnerPID: 1000, CPU: 2, RSS: 2 << 30,
		})
	}
	return st
}

func sampleEvents() []domain.Event {
	at := time.Date(2026, 10, 28, 9, 31, 5, 0, time.Local)
	return []domain.Event{
		{Seq: 1, Time: at, Kind: domain.EventDaemonStarted, Message: "listening on " + testSocket},
		{Seq: 2, Time: at.Add(time.Second), Kind: domain.EventPressureChanged, Message: "pressure normal -> high"},
	}
}

func assertFits(t *testing.T, out string, w, h int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) > h {
		t.Errorf("view has %d lines, window is %d high", len(lines), h)
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("line %d is %d wide, window is %d: %q", i, lw, w, l)
		}
	}
}

func assertContains(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("view missing %q:\n%s", s, out)
		}
	}
}

func TestRenderWide(t *testing.T) {
	c := &fakeClient{status: sampleStatus(3, 2), events: sampleEvents()}
	m := newModel(t, c, 120, 40)
	out := view(m)
	assertFits(t, out, 120, 40)
	assertContains(t, out,
		"SYSTEM", "pressure HIGH", "memory used 0.91 >= 0.90",
		"CPU", "74%", "load 0.85/cpu",
		"MEM", "75%", "12.0 GiB / 16.0 GiB",
		"SWAP", "25%", "1.0 GiB / 4.0 GiB",
		"AGENTS (3)", "AGENT", "PID", "CHILDREN", "claude-1", "1000",
		"HEAVY WORKLOADS (2)", "OWNER", "PROCESS", "CLASS", "gradle", "build", "200%",
		"TIMELINE", "09:31:06", "pressure_changed", "pressure normal -> high",
		"q quit",
	)
	// Newest event first.
	if strings.Index(out, "pressure normal -> high") > strings.Index(out, "listening on") {
		t.Error("timeline is not newest first")
	}
}

func TestRenderNarrow(t *testing.T) {
	st := sampleStatus(3, 2)
	st.Agents[0].Agent = "a-very-long-agent-instance-label-that-cannot-fit"
	c := &fakeClient{status: st, events: sampleEvents()}
	m := newModel(t, c, 60, 20)
	out := view(m)
	assertFits(t, out, 60, 20)
	assertContains(t, out, "pressure HIGH", "AGENTS", "HEAVY WORKLOADS", "gradle", "a-very-long", "…", "q quit")
	// The heavy table drops PID, then CLASS, before STATE at this width.
	heavy := out[strings.Index(out, "HEAVY WORKLOADS"):]
	header := strings.Split(heavy, "\n")[1]
	if strings.Contains(header, "PID") || strings.Contains(header, "CLASS") || !strings.Contains(header, "STATE") {
		t.Errorf("narrow heavy header = %q, want STATE without PID or CLASS", header)
	}
}

func TestPausedState(t *testing.T) {
	st := sampleStatus(1, 1)
	st.Paused = []control.PausedWorkload{{Key: st.Heavy[0].Key}}
	m := newModel(t, &fakeClient{status: st, events: sampleEvents()}, 120, 40)
	assertContains(t, view(m), "PAUSED")

	st.Scheduler.Queued = []domain.ProcessKey{st.Heavy[0].Key}
	m = newModel(t, &fakeClient{status: st, events: sampleEvents()}, 120, 40)
	assertContains(t, view(m), "QUEUED")
}

func TestTooSmall(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 1)}
	for _, size := range [][2]int{{39, 30}, {80, 11}} {
		m := newModel(t, c, size[0], size[1])
		out := view(m)
		assertContains(t, out, "terminal too small")
		assertFits(t, out, size[0], size[1])
	}
	m := newModel(t, c, 40, 12)
	if out := view(m); strings.Contains(out, "too small") {
		t.Errorf("40x12 should render:\n%s", out)
	} else {
		assertFits(t, out, 40, 12)
	}
}

func TestDaemonDownThenRecovers(t *testing.T) {
	c := &fakeClient{err: errors.New("connect: no such file or directory")}
	m := newModel(t, c, 80, 24)
	out := view(m)
	assertContains(t, out, "cannot reach throtd at "+testSocket, "start it with `throtd`", "retrying", "no such file", "(no data)")
	assertFits(t, out, 80, 24)

	// A failed poll schedules the next one instead of giving up.
	if _, cmd := m.Update(c.pollResult()); cmd == nil {
		t.Fatal("no retry scheduled after a failed poll")
	}
	// A tick triggers a poll command.
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Fatal("tick did not start a poll")
	}

	c.err, c.status, c.events = nil, sampleStatus(1, 0), sampleEvents()
	m = poll(t, m)
	out = view(m)
	if strings.Contains(out, "cannot reach") {
		t.Errorf("banner still shown after recovery:\n%s", out)
	}
	assertContains(t, out, "pressure HIGH", "claude-1")

	// Going down again clears the data instead of showing it as current.
	c.err = errors.New("connection refused")
	m = poll(t, m)
	out = view(m)
	assertContains(t, out, "cannot reach throtd")
	if strings.Contains(out, "claude-1") {
		t.Errorf("stale agent shown while disconnected:\n%s", out)
	}
}

func (f *fakeClient) pollResult() pollMsg {
	st, err := f.Status(context.Background())
	return pollMsg{status: st, statusErr: err}
}

func TestDegradedShown(t *testing.T) {
	st := sampleStatus(1, 0)
	st.Degraded = "processes: ps exited 1"
	m := newModel(t, &fakeClient{status: st}, 80, 24)
	assertContains(t, view(m), "DEGRADED", "processes: ps exited 1")
}

func TestMissingMetricsShowNA(t *testing.T) {
	st := sampleStatus(1, 0)
	st.Host.Missing = []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricSwap}
	st.Pressure = domain.PressureUnknown
	m := newModel(t, &fakeClient{status: st}, 80, 24)
	out := view(m)
	for _, label := range []string{"CPU", "SWAP"} {
		if !regexp.MustCompile(`(?m)^` + label + `\s+n/a$`).MatchString(out) {
			t.Errorf("%s not shown as n/a:\n%s", label, out)
		}
	}
	assertContains(t, out, "12.0 GiB / 16.0 GiB", "pressure UNKNOWN")
	if regexp.MustCompile(`load [0-9]`).MatchString(out) {
		t.Errorf("missing load rendered:\n%s", out)
	}

	st.Host = nil // no sample yet
	m = newModel(t, &fakeClient{status: st}, 80, 24)
	if n := strings.Count(view(m), "n/a"); n != 3 {
		t.Errorf("no host sample: %d n/a metrics, want 3", n)
	}
}

func TestKernelMemoryPressureShown(t *testing.T) {
	st := sampleStatus(1, 0)
	m := newModel(t, &fakeClient{status: st}, 100, 24)
	if out := view(m); strings.Contains(out, "kernel:") {
		t.Errorf("kernel verdict shown without one:\n%s", out)
	}

	st.Host.MemoryPressure = domain.MemoryPressureWarning
	m = newModel(t, &fakeClient{status: st}, 100, 24)
	out := view(m)
	if !regexp.MustCompile(`(?m)^MEM .*12\.0 GiB / 16\.0 GiB  kernel: warning`).MatchString(out) {
		t.Errorf("kernel verdict not on the MEM line:\n%s", out)
	}

	st.Host.Missing = []domain.Metric{domain.MetricMemory}
	m = newModel(t, &fakeClient{status: st}, 100, 24)
	if out := view(m); !regexp.MustCompile(`(?m)^MEM\s+n/a  kernel: warning$`).MatchString(out) {
		t.Errorf("kernel verdict not shown with memory missing:\n%s", out)
	}

	st.Host.Missing = []domain.Metric{domain.MetricMemoryPressure}
	m = newModel(t, &fakeClient{status: st}, 100, 24)
	if out := view(m); strings.Contains(out, "kernel:") {
		t.Errorf("kernel verdict shown while missing:\n%s", out)
	}
}

func TestPSIShown(t *testing.T) {
	st := sampleStatus(1, 0)
	m := newModel(t, &fakeClient{status: st}, 120, 24)
	if out := view(m); strings.Contains(out, "PSI") {
		t.Errorf("PSI shown without readings:\n%s", out)
	}

	st.Host.MemoryStall = &domain.Stall{Some: 0.031, Full: 0.01}
	st.Host.IOStall = &domain.Stall{Some: 0, Full: 0}
	m = newModel(t, &fakeClient{status: st}, 120, 24)
	if out := view(m); !regexp.MustCompile(`(?m)^SYSTEM .*pressure .*  PSI mem 3% io 0%$`).MatchString(out) {
		t.Errorf("PSI not on the SYSTEM line:\n%s", out)
	}

	st.Host.CPUStall = &domain.Stall{Some: 0.6}
	st.Host.Missing = []domain.Metric{domain.MetricIOStall}
	m = newModel(t, &fakeClient{status: st}, 120, 24)
	if out := view(m); !strings.Contains(out, "PSI mem 3% cpu 60%") {
		t.Errorf("missing io stall rendered or cpu stall absent:\n%s", out)
	}
}

func selectedLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "> ") {
			return l
		}
	}
	t.Fatalf("no selected row:\n%s", out)
	return ""
}

func TestSelectionAndScrolling(t *testing.T) {
	c := &fakeClient{status: sampleStatus(30, 2)}
	m := newModel(t, c, 80, 24)
	rows := m.layout().agentRows
	if rows >= 30 {
		t.Fatalf("agents pane has %d rows; test needs it to scroll", rows)
	}

	m = press(t, m, "k", "up") // already at the top
	if m.sel[paneAgents] != 0 || m.off[paneAgents] != 0 {
		t.Fatalf("sel/off = %d/%d, want 0/0", m.sel[paneAgents], m.off[paneAgents])
	}
	assertContains(t, selectedLine(t, view(m)), "claude-1 ")

	for range rows {
		m = press(t, m, "j")
	}
	if m.sel[paneAgents] != rows || m.off[paneAgents] != 1 {
		t.Fatalf("after %d downs sel/off = %d/%d, want %d/1", rows, m.sel[paneAgents], m.off[paneAgents], rows)
	}
	out := view(m)
	assertContains(t, out, fmt.Sprintf("AGENTS 2-%d of 30", rows+1))
	assertContains(t, selectedLine(t, out), fmt.Sprintf("claude-%d ", rows+1))

	for range 100 {
		m = press(t, m, "down")
	}
	if m.sel[paneAgents] != 29 || m.off[paneAgents] != 30-rows {
		t.Fatalf("at bottom sel/off = %d/%d, want 29/%d", m.sel[paneAgents], m.off[paneAgents], 30-rows)
	}
	assertContains(t, selectedLine(t, view(m)), "claude-30")

	// Tab moves focus and the selection marker to the heavy table.
	m = press(t, m, "tab", "j", "j", "j")
	if m.focus != paneHeavy || m.sel[paneHeavy] != 1 {
		t.Fatalf("heavy focus/sel = %d/%d, want %d/1", m.focus, m.sel[paneHeavy], paneHeavy)
	}
	assertContains(t, selectedLine(t, view(m)), "2001")
	m = press(t, m, "tab")
	if m.focus != paneAgents {
		t.Fatal("tab did not cycle back to agents")
	}

	// The selection follows its process when the list shrinks and reorders.
	c.status = sampleStatus(30, 2)
	c.status.Agents = c.status.Agents[25:]
	m = poll(t, m)
	if m.sel[paneAgents] != 4 {
		t.Errorf("after reorder sel = %d, want 4 (claude-30)", m.sel[paneAgents])
	}
	c.status.Agents = c.status.Agents[:2]
	m = poll(t, m)
	if m.sel[paneAgents] != 1 || m.off[paneAgents] != 0 {
		t.Errorf("after shrink sel/off = %d/%d, want 1/0", m.sel[paneAgents], m.off[paneAgents])
	}
	assertFits(t, view(m), 80, 24)
}

func TestDetailsToggle(t *testing.T) {
	c := &fakeClient{status: sampleStatus(2, 1)}
	m := newModel(t, c, 80, 24)
	if strings.Contains(view(m), "DETAILS") {
		t.Fatal("details shown before enter")
	}
	m = press(t, m, "j", "enter")
	out := view(m)
	assertContains(t, out, "DETAILS  agent claude-2", "executable claude", "pid 1001", "started 2026-10-28 09:30:00", "children 1")
	assertFits(t, out, 80, 24)

	m = press(t, m, "tab")
	assertContains(t, view(m), "DETAILS  workload gradle", "class build", "owner claude-1", "owner pid 1000")

	m = press(t, m, "enter")
	if strings.Contains(view(m), "DETAILS") {
		t.Error("enter did not hide details")
	}
}

func TestHelpAndQuit(t *testing.T) {
	m := newModel(t, &fakeClient{status: sampleStatus(1, 0)}, 80, 24)
	m = press(t, m, "?")
	assertContains(t, view(m), "HELP", "throtd keeps running")
	m = press(t, m, "?")
	if strings.Contains(view(m), "HELP") {
		t.Error("? did not close help")
	}
	for _, k := range []string{"q", "ctrl+c"} {
		_, cmd := m.Update(func() tea.Msg {
			if k == "q" {
				return tea.KeyPressMsg{Code: 'q', Text: "q"}
			}
			return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
		}())
		if cmd == nil {
			t.Fatalf("%s returned no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s did not quit", k)
		}
	}
}

func TestFitColumns(t *testing.T) {
	cols := []column{
		{title: "NAME", min: 8},
		{title: "A", width: 10, drop: 1},
		{title: "B", width: 10, drop: 2},
	}
	tests := []struct {
		avail int
		want  []int
	}{
		{100, []int{32, 10, 10}},
		{32, []int{8, 10, 10}},
		{31, []int{19, 10, 0}},
		{10, []int{10, 0, 0}},
		{4, []int{8, 0, 0}},
	}
	for _, tt := range tests {
		got := fitColumns(cols, tt.avail)
		if fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("fitColumns(%d) = %v, want %v", tt.avail, got, tt.want)
		}
	}
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
}

func TestTimelineIncremental(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 0), events: sampleEvents()}
	m := newModel(t, c, 120, 40)
	at := c.events[1].Time
	c.events = append(c.events, domain.Event{Seq: 3, Time: at.Add(time.Second), Kind: domain.EventAgentDiscovered, Message: "third event"})
	m = poll(t, m)
	if fmt.Sprint(c.sinces) != "[0 2]" {
		t.Fatalf("since values = %v, want [0 2]", c.sinces)
	}
	if len(m.events) != 3 || m.lastSeq != 3 {
		t.Fatalf("after incremental poll: %d events, lastSeq %d; want 3, 3", len(m.events), m.lastSeq)
	}
	assertContains(t, view(m), "third event", "listening on")

	// History stays bounded as events keep arriving.
	for i := range eventLimit + 20 {
		c.events = append(c.events, domain.Event{Seq: uint64(4 + i), Time: at, Kind: domain.EventAgentDiscovered, Message: fmt.Sprint("e", 4+i)})
	}
	m = poll(t, m)
	m = poll(t, m)
	if len(m.events) != eventLimit || m.lastSeq != uint64(3+eventLimit+20) {
		t.Fatalf("bounded history: %d events, lastSeq %d; want %d, %d", len(m.events), m.lastSeq, eventLimit, 3+eventLimit+20)
	}

	// A restarted daemon numbers from 1 again; the history is replaced.
	c.events = []domain.Event{{Seq: 1, Time: at, Kind: domain.EventDaemonStarted, Message: "restarted daemon"}}
	m = poll(t, m)
	if len(m.events) != 1 || m.lastSeq != 1 {
		t.Fatalf("after restart: %d events, lastSeq %d; want 1, 1", len(m.events), m.lastSeq)
	}
	out := view(m)
	assertContains(t, out, "restarted daemon")
	if strings.Contains(out, "third event") {
		t.Errorf("events from the old daemon still shown:\n%s", out)
	}
}

// pressCmd presses one key and returns the model and the command it produced.
func pressCmd(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
	return next.(Model), cmd
}

func footerLine(m Model) string {
	lines := strings.Split(view(m), "\n")
	return lines[len(lines)-1]
}

func TestControlConfirm(t *testing.T) {
	for _, tt := range []struct {
		key, verb string
	}{{"p", "pause"}, {"r", "resume"}} {
		c := &fakeClient{status: sampleStatus(1, 2)}
		m := press(t, newModel(t, c, 120, 40), "tab", "j")
		m = press(t, m, tt.key)
		assertContains(t, footerLine(m), tt.verb+" gradle (pid 2001)? y/n")
		if len(c.calls) != 0 {
			t.Fatalf("%s sent before confirmation: %v", tt.key, c.calls)
		}
		m, cmd := pressCmd(t, m, "y")
		if cmd == nil {
			t.Fatalf("y after %s returned no command", tt.key)
		}
		m = update(t, m, cmd())
		if want := []string{tt.verb + " 2001"}; !slices.Equal(c.calls, want) {
			t.Fatalf("calls = %v, want %v", c.calls, want)
		}
		assertContains(t, footerLine(m), tt.verb+"d gradle (pid 2001)")
		// The status line clears itself when its timer fires.
		m = update(t, m, clearNoticeMsg{id: m.noticeID})
		assertContains(t, footerLine(m), "q quit")
	}
}

func TestControlCancel(t *testing.T) {
	for _, key := range []string{"n", "q", "p", "j"} {
		c := &fakeClient{status: sampleStatus(1, 1)}
		m := press(t, newModel(t, c, 120, 40), "tab", "p")
		// The cancel notice proves the key was consumed by the prompt (q did
		// not quit, j did not move); its command is only the clear timer.
		m = press(t, m, key)
		if m.confirm != nil || len(c.calls) != 0 {
			t.Fatalf("%q: confirm = %v, calls = %v; want cancelled with no call", key, m.confirm, c.calls)
		}
		assertContains(t, footerLine(m), "pause cancelled")
	}
}

func TestControlError(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 1), controlErr: errors.New("pause refused: workload is gone")}
	m := press(t, newModel(t, c, 120, 40), "tab", "p")
	m, cmd := pressCmd(t, m, "y")
	m = update(t, m, cmd())
	assertContains(t, footerLine(m), "pause gradle (pid 2000) failed: pause refused: workload is gone")

	// An older timer does not clear a newer notice.
	m = press(t, m, "r", "n")
	m = update(t, m, clearNoticeMsg{id: m.noticeID - 1})
	assertContains(t, footerLine(m), "resume cancelled")
}

func TestControlKeysNeedHeavyFocus(t *testing.T) {
	c := &fakeClient{status: sampleStatus(2, 2)}
	m := newModel(t, c, 120, 40)
	if m.focus != paneAgents {
		t.Fatal("agents pane should have focus initially")
	}
	for _, key := range []string{"p", "r"} {
		m = press(t, m, key, "y")
		if m.confirm != nil || len(c.calls) != 0 || m.notice != "" {
			t.Fatalf("%s with agents focused: confirm = %v, calls = %v, notice = %q", key, m.confirm, c.calls, m.notice)
		}
	}
	// With no heavy workloads there is nothing to act on.
	c.status = sampleStatus(1, 0)
	m = press(t, poll(t, m), "tab", "p")
	if m.confirm != nil {
		t.Error("p prompted with no heavy workloads")
	}
	m = press(t, m, "?")
	assertContains(t, view(m), "p / r", "pause / resume the selected heavy workload")
}

func TestTimelineResetsOnDaemonRestart(t *testing.T) {
	m := Model{}
	m.addEvents([]domain.Event{{Seq: 1, Instance: "a"}, {Seq: 2, Instance: "a"}})
	// The new daemon is already past seq 2, so only the instance shows it.
	m.addEvents([]domain.Event{{Seq: 3, Instance: "b"}})
	if m.lastSeq != 0 || len(m.events) != 0 || m.instance != "b" {
		t.Fatalf("after restart: lastSeq %d, %d events, instance %q; want a reset to b", m.lastSeq, len(m.events), m.instance)
	}
	m.addEvents([]domain.Event{{Seq: 1, Instance: "b"}, {Seq: 2, Instance: "b"}, {Seq: 3, Instance: "b"}})
	if m.lastSeq != 3 || len(m.events) != 3 {
		t.Fatalf("after refetch: lastSeq %d, %d events", m.lastSeq, len(m.events))
	}
}
