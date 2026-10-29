package tui

import (
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// ownerStatus is the machine of the owner's report: 53 agents, memory 88%
// used, swap 91%, a kernel memory pressure warning, no heavy workloads, and
// half the memory outside every agent.
func ownerStatus() app.StatusResult {
	st := busyStatus()
	st.Heavy = nil
	st.Host.CPUUsage = 0.31
	st.Host.MemTotal, st.Host.MemAvailable = 24<<30, 2970<<20
	st.Host.SwapTotal, st.Host.SwapUsed = 18<<30, 16794<<20
	st.UnattributedRSS = 10137 << 20
	st.UnattributedMemory = []process.NameUsage{
		{Name: "com.apple.Virtualization.VirtualMachine", Processes: 1, RSS: 2250 << 20},
		{Name: "Browser", Processes: 19, RSS: 744 << 20},
		{Name: "Orca", Processes: 13, RSS: 650 << 20},
		{Name: "WindowServer", Processes: 1, RSS: 610 << 20},
		{Name: "Slack Helper", Processes: 4, RSS: 420 << 20},
	}
	st.Scheduler = app.SchedulerStatus{Mode: scheduler.Mode("observe"), HeavyLimit: 3, Adaptive: true, StartLimit: 2, MaxLimit: 10}
	return st
}

// ownerModel is ownerStatus with five minutes of made-up history: CPU
// waving around a third, memory filling up, swap growing, and pressure
// turning elevated a minute and a half ago.
func ownerModel(t *testing.T, w, h int) Model {
	t.Helper()
	c := &fakeClient{status: ownerStatus(), events: burstEvents()}
	m := newModel(t, c, w, h)
	m.hist = history{}
	base := *c.status.Host
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	for i := range historyLen {
		s, x := base, float64(i)
		s.Time = at.Add(time.Duration(i) * time.Second)
		s.CPUUsage = 0.35 + 0.25*math.Sin(x/9) + 0.1*math.Sin(x/2.3)
		s.MemAvailable = uint64(float64(base.MemAvailable) * (1.6 - 0.6*x/historyLen))
		s.SwapUsed = base.SwapUsed - uint64(historyLen-i)<<22
		s.Latency = time.Duration(4+3*math.Abs(math.Sin(x/5))) * time.Millisecond
		level := domain.PressureNormal
		if i >= historyLen-90 {
			level = domain.PressureElevated
		}
		m.hist.add(s, level)
	}
	return m
}

// TestDashboardFillsWindow checks the grid on a large and a medium
// terminal: panels cover the whole window, every stat has a panel, the
// time series, the memory gauge and the tables share rows, and the
// focused panel has the heavy border.
func TestDashboardFillsWindow(t *testing.T) {
	m := ownerModel(t, 200, 55)
	out := view(m)
	t.Logf("200x55:\n%s", out)
	lines := strings.Split(out, "\n")
	if len(lines) != 55 {
		t.Fatalf("%d lines, want 55", len(lines))
	}
	for i, l := range lines[:len(lines)-1] {
		if w := lipgloss.Width(l); w != 200 {
			t.Errorf("line %d is %d wide, want the full 200: %q", i, w, l)
		}
	}
	// Row 1: one stat panel each, in this order.
	at := 0
	for _, title := range []string{"PRESSURE", "CPU", "MEMORY", "SWAP", "LATENCY", "SCHEDULER"} {
		i := strings.Index(lines[0][at:], "─ "+title+" ─")
		if i < 0 {
			t.Fatalf("stat row lacks %s after %d: %q", title, at, lines[0])
		}
		at += i
	}
	// Big numbers, the detail under them and the sparkline last.
	mem := panelBody(t, out, "MEMORY")
	if mem[0] != "█▀█ █▀█ █ ▄▀" || mem[3] != "21.1/24.0 GiB" || mem[4] != "kernel: warning" || !strings.HasSuffix(mem[5], "▇▇▇") {
		t.Errorf("MEMORY panel = %q", mem)
	}
	assertContains(t, strings.Join(panelBody(t, out, "PRESSURE"), "\n"), "ELEVATED\nwhy: kernel memory pressure warning\n")
	assertContains(t, strings.Join(panelBody(t, out, "SCHEDULER"), "\n"), "OBSERVE\nlimit 3 (adaptive, start 2, max 10)\n0 would queue · 0 reserved")
	// Row 2: the time series, five minutes each on a 0–100% axis.
	for _, title := range []string{"CPU % · last 5m", "MEMORY % · last 5m", "SWAP % · last 5m"} {
		body := panelBody(t, out, title)
		if !strings.HasPrefix(body[0], "100%┤") || !strings.HasPrefix(body[len(body)-1], "  0%┤") ||
			!strings.ContainsAny(body[len(body)-1], "⣿⣇⣆⣄") {
			t.Errorf("%s chart = %q", title, body)
		}
	}
	// Row 3: the gauge, the agents and the heavy workloads side by side.
	row := lineContaining(t, out, "MEMORY BY OWNER")
	assertContains(t, row, "▸ AGENTS (53)", "HEAVY WORKLOADS")
	gauge := panelBody(t, out, "MEMORY BY OWNER")
	if gauge[0] != "● agents 9.0 GiB  ○ outside 9.9 GiB" || !strings.HasPrefix(gauge[1], "● codex-1") ||
		!strings.HasPrefix(gauge[2], "○ com.apple.Virtualization.…") || !strings.HasSuffix(gauge[2], "2.2 GiB") {
		t.Errorf("gauge = %q", gauge)
	}
	if last := gauge[len(gauge)-1]; last != "+49 agents 5.0 GiB · ○ other 5.3 GiB" {
		t.Errorf("gauge summary = %q", last)
	}
	// Row 4: the events fill the rest, down to the footer.
	if !strings.HasPrefix(lines[len(lines)-2], "╰") || !strings.Contains(lineContaining(t, out, "─ EVENTS ─"), "╭─ EVENTS") {
		t.Errorf("events do not reach the footer:\n%s", out)
	}
	// Focus moves the heavy border and the marker.
	tabbed := view(press(t, m, "tab"))
	assertContains(t, tabbed, "┏━ ▸ HEAVY WORKLOADS", "╭─ AGENTS (53)")

	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	out = view(m)
	t.Logf("100x30:\n%s", out)
	lines = strings.Split(out, "\n")
	for i, l := range lines[:len(lines)-1] {
		if w := lipgloss.Width(l); w != 100 {
			t.Errorf("line %d is %d wide, want the full 100: %q", i, w, l)
		}
	}
	if strings.Contains(lines[0], "LATENCY") || !strings.Contains(lines[0], "SCHEDULER") {
		t.Errorf("medium stat row = %q", lines[0])
	}
	assertContains(t, strings.Join(panelBody(t, out, "PRESSURE"), "\n"), "ELEVATED\nwhy: kernel memo…\nlatency 6ms")
	assertContains(t, out, "CPU % · last 5m", "EVENTS", "+48 more")
	assertContains(t, lineContaining(t, out, "MEMORY BY OWNER"), "▸ AGENTS (53)")
}
