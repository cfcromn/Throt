package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
)

// lineWith returns the first screen line starting with prefix.
func lineWith(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting with %q:\n%s", prefix, out)
	return ""
}

func TestSparkline(t *testing.T) {
	ok := func(n int) []bool { return slices.Repeat([]bool{true}, n) }
	steps := []float64{0, 1.0 / 7, 2.0 / 7, 3.0 / 7, 4.0 / 7, 5.0 / 7, 6.0 / 7, 1}
	tests := []struct {
		name         string
		vals         []float64
		ok           []bool
		scale, floor float64
		width        int
		want         string
	}{
		{"every level", steps, ok(8), 1, 0, 8, "▁▂▃▄▅▆▇█"},
		{"clamped", []float64{-1, 2}, ok(2), 1, 0, 2, "▁█"},
		{"padded left", []float64{0.5}, ok(1), 1, 0, 4, "   ▅"},
		{"newest kept", steps, ok(8), 1, 0, 3, "▆▇█"},
		{"missing blank", []float64{1, 0, 1}, []bool{true, false, true}, 1, 0, 3, "█ █"},
		{"auto scale", []float64{1, 2, 4}, ok(3), 0, 1, 3, "▃▅█"},
		{"floor", []float64{1, 2}, ok(2), 0, 28, 2, "▁▂"},
	}
	for _, tt := range tests {
		if got := sparkline(tt.vals, tt.ok, tt.scale, tt.floor, tt.width); got != tt.want {
			t.Errorf("%s: sparkline = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// pollHosts polls once per host sample, the samples one second apart.
func pollHosts(t *testing.T, m Model, c *fakeClient, hosts ...domain.HostSample) Model {
	t.Helper()
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	for i, h := range hosts {
		h.Time = at.Add(time.Duration(m.hist.n+i) * time.Second)
		c.status.Host = &h
		m = poll(t, m)
	}
	return m
}

func TestSparklinesInView(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 0)}
	m := newModel(t, c, 120, 40)
	m.hist = history{} // drop newModel's sample so the input is exact
	base := *sampleStatus(0, 0).Host
	h := func(cpu float64, swap uint64, lat time.Duration) domain.HostSample {
		s := base
		s.CPUUsage, s.SwapUsed, s.Latency = cpu, swap, lat
		return s
	}
	m = pollHosts(t, m, c,
		h(0, 1<<30, 10*time.Millisecond),
		h(0.5, 1<<30+2<<20, 20*time.Millisecond),
		h(1, 1<<30+2<<20, 40*time.Millisecond),
	)
	out := view(m)
	// 16 cells, right-aligned: CPU 0, 0.5, 1 of one; swap growth unknown,
	// 2 MiB/s, 0; latency scaled to its 40ms maximum.
	assertContains(t, lineWith(t, out, "CPU "), "100%  "+strings.Repeat(" ", 13)+"▁▅█  load 0.85/cpu")
	assertContains(t, lineWith(t, out, "MEM "), "75%  "+strings.Repeat(" ", 13)+"▆▆▆  12.0 GiB")
	assertContains(t, lineWith(t, out, "SWAP "), "25%  "+strings.Repeat(" ", 14)+"█▁  1.0 GiB / 4.0 GiB")
	assertContains(t, lineWith(t, out, "SYSTEM"), "latency 40ms ▃▅█")
	assertFits(t, out, 120, 40)

	// A missing metric leaves a gap in its line.
	gap := h(0, 1<<30, 0)
	gap.Missing = []domain.Metric{domain.MetricCPU}
	m = pollHosts(t, m, c, gap, h(1, 1<<30, 0))
	assertContains(t, lineWith(t, view(m), "CPU "), strings.Repeat(" ", 11)+"▁▅█ █  load")

	// Losing the daemon drops the history with the rest of the data.
	c.err = errors.New("down")
	m = poll(t, m)
	if m.hist.n != 0 {
		t.Errorf("history kept %d samples while disconnected", m.hist.n)
	}
}

func TestHistoryBounded(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 0)}
	m := newModel(t, c, 120, 40)
	m.hist = history{}
	hosts := make([]domain.HostSample, historyLen+15)
	for i := range hosts {
		hosts[i] = *sampleStatus(0, 0).Host
		hosts[i].CPUUsage = float64(i) / 100
	}
	m = pollHosts(t, m, c, hosts...)
	if m.hist.n != historyLen {
		t.Fatalf("history holds %d samples, want %d", m.hist.n, historyLen)
	}
	newest := float64(historyLen+14) / 100
	vals, _ := m.hist.series(historyLen*2, cpuOf)
	if len(vals) != historyLen || vals[0] != 0.15 || vals[len(vals)-1] != newest {
		t.Fatalf("series = %d values from %v to %v; want the newest %d", len(vals), vals[0], vals[len(vals)-1], historyLen)
	}
	// An older model is unaffected by newer polls.
	old := m
	pollHosts(t, m, c, *sampleStatus(0, 0).Host)
	if v, _ := old.hist.series(1, cpuOf); v[0] != newest {
		t.Errorf("older model's newest sample changed to %v", v[0])
	}
}

func TestExplainReason(t *testing.T) {
	tests := []struct {
		code, want string
		ok         bool
	}{
		{"memory_used>=0.9", "memory 90%+ used", true},
		{"memory_used>=0.955", "memory 95.5%+ used", true},
		{"cpu>=0.95", "CPU 95%+ busy", true},
		{"latency>=100ms", "foreground lag ≥100ms", true},
		{"swap_growth>=1048576", "swapping fast (≥1 MiB/s)", true},
		{"swap_growth>=524288", "swapping fast (≥512 KiB/s)", true},
		{"load>=1.5", "load ≥1.5 per CPU", true},
		{"kernel_memory_pressure>=warning", "kernel memory pressure warning", true},
		{"below_thresholds", "all signals below thresholds", true},
		{"recovered", "signals back below exit thresholds", true},
		{"no_signals", "no signals reported yet", true},
		// Unknown or malformed codes come back raw.
		{"gpu>=0.9", "gpu>=0.9", false},
		{"memory_used>=lots", "memory_used>=lots", false},
		{"memory_used>=NaN", "memory_used>=NaN", false},
		{"memory_used>=", "memory_used>=", false},
		{"something new", "something new", false},
	}
	for _, tt := range tests {
		got, ok := explainReason(tt.code)
		if got != tt.want || ok != tt.ok {
			t.Errorf("explainReason(%q) = %q, %v; want %q, %v", tt.code, got, ok, tt.want, tt.ok)
		}
	}
}

func TestReasonLine(t *testing.T) {
	st := sampleStatus(1, 0)
	st.Reason = "memory_used>=0.9"
	m := newModel(t, &fakeClient{status: st}, 80, 24)
	if lines := strings.Split(view(m), "\n"); lines[1] != "  why: memory 90%+ used  (memory_used>=0.9)" {
		t.Errorf("line under SYSTEM = %q", lines[1])
	}
	// At 40 columns the raw code gives way.
	m = newModel(t, &fakeClient{status: st}, 40, 12)
	if lines := strings.Split(view(m), "\n"); lines[1] != "  why: memory 90%+ used" {
		t.Errorf("narrow line under SYSTEM = %q", lines[1])
	}
	st.Reason = ""
	m = newModel(t, &fakeClient{status: st}, 80, 24)
	if strings.Contains(view(m), "why:") {
		t.Error("explanation shown without a reason")
	}
}

// drillStatus has claude-1 owning a queued, a paused and a running gradle
// (2000-2002) and claude-2 owning cargo (2003).
func drillStatus() app.StatusResult {
	st := sampleStatus(2, 4)
	st.Heavy[3].Owner, st.Heavy[3].OwnerPID, st.Heavy[3].Name = "claude-2", 1001, "cargo"
	st.Scheduler.Queued = []domain.ProcessKey{st.Heavy[0].Key}
	st.Paused = []control.PausedWorkload{{Key: st.Heavy[1].Key}}
	return st
}

func TestDrilldown(t *testing.T) {
	c := &fakeClient{status: drillStatus(), events: sampleEvents()}
	m := press(t, newModel(t, c, 60, 20), "enter")
	if !m.drill {
		t.Fatal("enter on the agents pane did not open the drilldown")
	}
	out := view(m)
	assertFits(t, out, 60, 20)
	assertContains(t, out, "AGENT claude-1", "pid 1000", "cpu 50%", "mem 1.0 GiB", "children 0",
		"HEAVY WORKLOADS (3)", "QUEUED", "PAUSED", "running", "esc back")
	for _, gone := range []string{"2003", "cargo", "TIMELINE", "AGENTS"} {
		if strings.Contains(out, gone) {
			t.Errorf("drilldown shows %q:\n%s", gone, out)
		}
	}

	// tab stays in the drilldown; j moves within it; p uses the confirm flow.
	m = press(t, m, "tab", "j", "p")
	if !m.drill {
		t.Fatal("tab left the drilldown")
	}
	assertContains(t, footerLine(m), "pause gradle (pid 2001)? y/n")
	m, cmd := pressCmd(t, m, "y")
	m = update(t, m, cmd())
	if want := []string{"pause 2001"}; !slices.Equal(c.calls, want) {
		t.Fatalf("calls = %v, want %v", c.calls, want)
	}
	assertContains(t, footerLine(m), "paused gradle (pid 2001)")
	m = press(t, m, "r")
	assertContains(t, footerLine(m), "resume gradle (pid 2001)? y/n")
	m = press(t, m, "n")
	if len(c.calls) != 1 {
		t.Fatalf("r then n sent %v", c.calls)
	}

	// esc closes help first, then the drilldown.
	m = press(t, m, "?", "esc")
	if !m.drill || m.help {
		t.Fatalf("esc from help: drill %v help %v; want the drilldown back", m.drill, m.help)
	}
	m = press(t, m, "esc")
	if m.drill {
		t.Fatal("esc did not close the drilldown")
	}
	out = view(m)
	assertContains(t, out, "AGENTS (2)", "HEAVY WORKLOADS (4)", "TIMELINE")
	assertContains(t, selectedLine(t, out), "claude-1")

	// The second agent sees only its own workload.
	m = press(t, m, "j", "enter")
	out = view(m)
	assertContains(t, out, "AGENT claude-2", "HEAVY WORKLOADS (1)", "2003", "cargo")
	if strings.Contains(out, "gradle") {
		t.Errorf("claude-2 drilldown shows claude-1's workloads:\n%s", out)
	}

	// An agent that exits while open is reported, not silently replaced.
	c.status.Agents = c.status.Agents[:1]
	c.status.Heavy = c.status.Heavy[:3]
	m = poll(t, m)
	assertContains(t, view(m), "AGENT claude-2", "pid 1001 is no longer reported", "(no heavy workloads)")
	m = press(t, m, "p")
	if m.confirm != nil {
		t.Error("p prompted with nothing to act on")
	}
}

func TestDrilldownNeedsAgent(t *testing.T) {
	m := press(t, newModel(t, &fakeClient{status: sampleStatus(0, 1)}, 80, 24), "enter")
	if m.drill {
		t.Error("enter opened a drilldown with no agents")
	}
	m = press(t, m, "tab", "enter")
	if m.drill || !m.details {
		t.Errorf("enter on heavy: drill %v details %v; want details", m.drill, m.details)
	}
}

// headerLine returns the column header of the table titled title.
func headerLine(t *testing.T, out, title string) string {
	t.Helper()
	i := strings.Index(out, title)
	if i < 0 {
		t.Fatalf("no %q table:\n%s", title, out)
	}
	return strings.Split(out[i:], "\n")[1]
}

func TestSortCycling(t *testing.T) {
	st := sampleStatus(3, 2)
	for i, a := range []struct {
		name string
		cpu  float64
		rss  uint64
	}{{"b", 0.1, 3 << 30}, {"c", 0.9, 1 << 30}, {"a", 0.5, 2 << 30}} {
		st.Agents[i].Agent, st.Agents[i].CPU, st.Agents[i].RSS = a.name, a.cpu, a.rss
	}
	st.Heavy[1].CPU = 3
	c := &fakeClient{status: st}
	m := newModel(t, c, 120, 40)
	order := func(m Model) string {
		var s string
		for _, a := range m.agents {
			s += a.Agent
		}
		return s
	}
	for _, s := range []struct {
		order, indicator string
	}{
		{"cab", "CPU▼"},
		{"bac", "MEM▼"},
		{"abc", "AGENT▲"},
		{"bca", ""}, // back to daemon order
	} {
		m = press(t, m, "s")
		out := view(m)
		header := headerLine(t, out, "AGENTS")
		if got := order(m); got != s.order {
			t.Errorf("sort %v: order %q, want %q", m.sortBy[paneAgents], got, s.order)
		}
		if s.indicator != "" && !strings.Contains(header, s.indicator) {
			t.Errorf("header %q lacks %q", header, s.indicator)
		}
		if s.indicator == "" && strings.ContainsAny(header, "▲▼") {
			t.Errorf("daemon order header %q has an indicator", header)
		}
		// The selection stays on agent b, whatever the order.
		assertContains(t, selectedLine(t, out), "> b ")
		assertFits(t, out, 120, 40)
	}
	if got := c.status.Agents[0].Agent + c.status.Agents[1].Agent + c.status.Agents[2].Agent; got != "bca" {
		t.Errorf("sorting modified the client's status: %q", got)
	}

	// Each table sorts on its own, and the order survives a poll.
	m = press(t, m, "tab", "s")
	out := view(m)
	assertContains(t, headerLine(t, out, "HEAVY WORKLOADS"), "CPU▼")
	if strings.ContainsAny(headerLine(t, out, "AGENTS"), "▲▼") {
		t.Error("sorting heavy workloads changed the agents header")
	}
	m = poll(t, m)
	if m.heavy[0].Key.PID != 2001 {
		t.Errorf("after poll heavy[0] = %d, want 2001 (most CPU)", m.heavy[0].Key.PID)
	}
	// With help open, s does nothing.
	m = press(t, m, "?", "s")
	if m.sortBy[paneHeavy] != sortCPU {
		t.Errorf("s under help changed the sort to %v", m.sortBy[paneHeavy])
	}
}

func TestDrilldownSort(t *testing.T) {
	st := drillStatus()
	st.Heavy[2].CPU = 5
	m := press(t, newModel(t, &fakeClient{status: st}, 120, 40), "enter", "s")
	out := view(m)
	assertContains(t, headerLine(t, out, "HEAVY WORKLOADS"), "CPU▼")
	// The selection stays on 2000, now below 2002.
	assertContains(t, selectedLine(t, out), "2000")
	if ws := m.workloads(paneDrill); ws[0].Key.PID != 2002 {
		t.Errorf("drilldown first row = %d, want 2002", ws[0].Key.PID)
	}
	// The drilldown shares the heavy table's sort.
	m = press(t, m, "esc")
	assertContains(t, headerLine(t, view(m), "HEAVY WORKLOADS"), "CPU▼")
}

func TestNarrowDropOrder(t *testing.T) {
	st := sampleStatus(3, 2)
	st.Reason = "memory_used>=0.9"
	sparks := func(line string) int {
		n := 0
		for _, r := range line {
			if slices.Contains(sparkBlocks[:7], r) { // █ is also the bar
				n++
			}
		}
		return n
	}
	for _, tt := range []struct {
		width, sparkCells int
		raw               bool
	}{
		{120, maxSparkWidth, true},
		{60, 13, true},
		{55, minSparkWidth, true},
		{54, 0, true},
		{40, 0, false},
	} {
		c := &fakeClient{status: st}
		m := newModel(t, c, tt.width, 20)
		m.hist = history{}
		m = pollHosts(t, m, c, slices.Repeat([]domain.HostSample{*st.Host}, historyLen)...)
		out := view(m)
		assertFits(t, out, tt.width, 20)
		cpu := lineWith(t, out, "CPU ")
		if got := sparks(cpu); got != tt.sparkCells {
			t.Errorf("width %d: %d sparkline cells, want %d: %q", tt.width, got, tt.sparkCells, cpu)
		}
		// The bar and percentage outlive the sparkline.
		if !strings.Contains(cpu, "█") || !strings.Contains(cpu, "74%") {
			t.Errorf("width %d: CPU bar lost: %q", tt.width, cpu)
		}
		if got := strings.Contains(out, "(memory_used>=0.9)"); got != tt.raw {
			t.Errorf("width %d: raw reason shown %v, want %v", tt.width, got, tt.raw)
		}
		assertContains(t, out, "why: memory 90%+ used")
	}
}

func TestDrilldownSmall(t *testing.T) {
	m := press(t, newModel(t, &fakeClient{status: drillStatus()}, minWidth, minHeight), "enter")
	out := view(m)
	assertFits(t, out, minWidth, minHeight)
	// The agent summary shrinks so a workload row stays visible.
	assertContains(t, out, "AGENT claude-1", "HEAVY WORKLOADS 1-1 of 3", "STATE")
	assertContains(t, selectedLine(t, out), "gradle")
}
