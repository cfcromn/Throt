package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// busyStatus is a Mac with 53 agents under kernel memory pressure: three
// busy agents, 50 idle ones, two builds, and most memory outside agents.
func busyStatus() app.StatusResult {
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	st := app.StatusResult{
		Pressure: domain.PressureElevated,
		Reason:   "kernel_memory_pressure>=warning",
		Host: &domain.HostSample{
			CPUUsage: 0.46, Load1: 0.62, Latency: 6 * time.Millisecond,
			MemTotal: 24 << 30, MemAvailable: 3 << 30,
			SwapTotal: 12 << 30, SwapUsed: 10700 << 20,
			MemoryPressure: domain.MemoryPressureWarning,
		},
		UnattributedRSS: 10300 << 20,
		UnattributedMemory: []process.NameUsage{
			{Name: "com.apple.Virtualization.VirtualMachine", Processes: 1, RSS: 4300 << 20},
			{Name: "Google Chrome Helper (Renderer)", Processes: 31, RSS: 2600 << 20},
			{Name: "WindowServer", Processes: 1, RSS: 1100 << 20},
			{Name: "Slack Helper", Processes: 4, RSS: 610 << 20},
			{Name: "kernel_task", Processes: 1, RSS: 480 << 20},
		},
	}
	big := []struct {
		cpu float64
		rss uint64
		ch  int
	}{{1.9, 2300 << 20, 14}, {0.42, 1100 << 20, 6}, {0.08, 640 << 20, 3}}
	for i := range 53 {
		a := process.AgentView{
			Key:   domain.ProcessKey{PID: 41000 + i*7, Start: start},
			Agent: fmt.Sprintf("claude-%d", i+1), Name: "claude",
			Children: 2, CPU: 0.01 + float64(i%3)/100, RSS: (96 + uint64(i%5)*4) << 20,
		}
		if i >= 50 {
			b := big[i-50]
			a.Agent, a.CPU, a.RSS, a.Children = fmt.Sprintf("codex-%d", i-49), b.cpu, b.rss, b.ch
		}
		st.Agents = append(st.Agents, a)
	}
	st.Heavy = []process.WorkloadView{
		{Key: domain.ProcessKey{PID: 52011, Start: start}, Name: "cargo", Class: domain.ClassBuild, Owner: "codex-1", OwnerPID: 41350, CPU: 1.7, RSS: 1800 << 20},
		{Key: domain.ProcessKey{PID: 52230, Start: start}, Name: "vitest", Class: domain.ClassTest, Owner: "codex-2", OwnerPID: 41357, CPU: 0.35, RSS: 700 << 20},
	}
	return st
}

// burstEvents is a startup: throtd starts, then discovers 53 agents and
// two workloads within one second, then pressure rises.
func burstEvents() []domain.Event {
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local)
	ev := []domain.Event{{Seq: 1, Time: t0, Kind: domain.EventDaemonStarted, Message: "throtd started"}}
	add := func(at time.Time, k domain.EventKind, msg string, attrs map[string]string) {
		ev = append(ev, domain.Event{Seq: uint64(len(ev) + 1), Time: at, Kind: k, Message: msg, Attrs: attrs})
	}
	for i := range 53 {
		name := fmt.Sprintf("claude-%d", i+1)
		add(t0.Add(2*time.Second+time.Duration(i)*time.Millisecond), domain.EventAgentDiscovered,
			"agent "+name+" started", map[string]string{"agent": name, "name": "claude"})
	}
	add(t0.Add(2500*time.Millisecond), domain.EventProcessDiscovered, "discovered heavy workload cargo (pid 52011)", map[string]string{"name": "cargo"})
	add(t0.Add(2600*time.Millisecond), domain.EventProcessDiscovered, "discovered heavy workload vitest (pid 52230)", map[string]string{"name": "vitest"})
	add(t0.Add(9*time.Second), domain.EventPressureChanged, "pressure normal -> elevated", nil)
	add(t0.Add(9*time.Second), domain.EventSchedulerDecision, "queued gradle (pid 1)", nil)
	add(t0.Add(9*time.Second), domain.EventSchedulerDecision, "queued gradle (pid 2)", nil)
	return ev
}

func TestOverviewUnderMemoryPressure(t *testing.T) {
	m := newModel(t, &fakeClient{status: busyStatus(), events: burstEvents()}, 120, 40)
	out := view(m)
	t.Logf("120x40:\n%s", out)
	assertFits(t, out, 120, 40)
	assertContains(t, strings.Join(panelBody(t, out, "PRESSURE"), " "), "ELEVATED", "why: kernel memory pressure warning")
	// Who is eating memory: one bar per owner, agents (●) and outside
	// groups (○) together, largest first, then what did not fit.
	gauge := panelBody(t, out, "MEMORY BY OWNER")
	for i, want := range []string{
		"● agents 9.0 GiB  ○ outside 10.1 GiB",
		"○ com.apple.Virtual… ██████  4.2 GiB",
		"○ Google Chrome Hel… ███▋░░  2.5 GiB",
		"● codex-1            ███▎░░  2.2 GiB",
		"● codex-2            █▌░░░░  1.1 GiB",
	} {
		if gauge[i] != want {
			t.Errorf("gauge row %d = %q, want %q", i, gauge[i], want)
		}
	}
	if last := gauge[len(gauge)-1]; last != "+44 agents 4.4 GiB · ○ other 1.2 GiB" {
		t.Errorf("gauge summary = %q", last)
	}
	assertContains(t, out,
		"AGENTS (53)  total 339% CPU, 9.0 GiB",
		"MEM▼",
		"+44 more (each ≤3% CPU, ≤112 MiB; 87% CPU, 4.4 GiB together)",
		"53 agents started: claude-1, claude-2",
		"discovered 2 heavy workloads: cargo, vitest",
	)
	// Memory pressure sorts by memory: the biggest agents come first.
	if m.agents[0].Agent != "codex-1" || m.agents[1].Agent != "codex-2" {
		t.Errorf("auto sort under memory pressure: %s, %s first", m.agents[0].Agent, m.agents[1].Agent)
	}
	// Scheduler decisions are never coalesced.
	if n := strings.Count(out, "queued gradle"); n != 2 {
		t.Errorf("%d queued gradle lines, want 2", n)
	}
	// At 80 columns nothing wraps or overflows.
	narrow := view(update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24}))
	t.Logf("80x24:\n%s", narrow)
	assertFits(t, narrow, 80, 24)
}

func TestAutoSortFollowsReason(t *testing.T) {
	st := busyStatus()
	st.Reason = "cpu>=0.9"
	m := newModel(t, &fakeClient{status: st}, 120, 40)
	if m.agents[0].Agent != "codex-1" || m.agents[2].Agent != "codex-3" {
		t.Errorf("cpu order starts %s, %s, %s", m.agents[0].Agent, m.agents[1].Agent, m.agents[2].Agent)
	}
	assertContains(t, headerLine(t, view(m), "AGENTS ("), "CPU▼")
	for code, want := range map[string]bool{
		"memory_used>=0.9": true, "swap_growth>=1048576": true, "memory_stall_some>=0.1": true,
		"kernel_memory_pressure>=warning": true, "cpu>=0.9": false, "load>=1.5": false, "": false,
	} {
		if got := memoryReason(code); got != want {
			t.Errorf("memoryReason(%q) = %v", code, got)
		}
	}
}

func TestAgentsCollapseAndScroll(t *testing.T) {
	m := newModel(t, &fakeClient{status: busyStatus()}, 120, 40)
	rows := m.layout().agentRows
	if rows < 3 || rows >= 53 {
		t.Fatalf("agent rows = %d", rows)
	}
	assertContains(t, view(m), fmt.Sprintf("+%d more", 53-(rows-1)))
	// Moving past the first rows scrolls through the rest.
	for range rows - 1 {
		m = press(t, m, "j")
	}
	out := view(m)
	if strings.Contains(out, " more (each") {
		t.Errorf("summary shown while scrolled:\n%s", out)
	}
	assertContains(t, out, fmt.Sprintf("AGENTS 1-%d of 53", rows))
	m = press(t, m, "j")
	assertContains(t, view(m), fmt.Sprintf("AGENTS 2-%d of 53", rows+1))
	// Back among the first rows, the rest folds again.
	m = press(t, m, "k", "k")
	assertContains(t, view(m), fmt.Sprintf("+%d more", 53-(rows-1)), "AGENTS (53)")
}

func TestSplash(t *testing.T) {
	c := &fakeClient{status: busyStatus()}
	m := update(t, New(c, testSocket), tea.WindowSizeMsg{Width: 120, Height: 40})
	out := view(m)
	t.Logf("splash 120x40:\n%s", out)
	assertFits(t, out, 120, 40)
	assertContains(t, out, "████████╗██╗  ██╗██████╗", "Throt: "+tagline, "version "+app.Version,
		"connecting to throtd at "+testSocket, "q quit")

	// The first poll alone does not hide it; with splashMin passed it does.
	m = poll(t, m)
	assertContains(t, view(m), "● connected to throtd")
	m = update(t, m, splashDoneMsg{})
	if m.splash || !strings.Contains(view(m), "AGENTS") {
		t.Fatalf("splash still shown after poll and timeout:\n%s", view(m))
	}

	// splashMin first, then the poll.
	m = update(t, New(c, testSocket), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = update(t, m, splashDoneMsg{})
	if !m.splash {
		t.Fatal("splash hid before any poll")
	}
	if m = poll(t, m); m.splash {
		t.Fatal("splash shown after timeout and poll")
	}

	// A key dismisses it without acting; q quits.
	m = update(t, New(c, testSocket), tea.WindowSizeMsg{Width: 120, Height: 40})
	if d := press(t, m, "j"); d.splash || d.sel[paneAgents] != 0 {
		t.Errorf("j: splash %v, selection %d", d.splash, d.sel[paneAgents])
	}
	if _, cmd := m.Update(keyMsg("q")); cmd == nil {
		t.Error("q did not quit from the splash")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q did not quit from the splash")
	}

	// A daemon that is down shows as waiting.
	down := update(t, New(&fakeClient{err: fmt.Errorf("refused")}, testSocket), tea.WindowSizeMsg{Width: 120, Height: 40})
	assertContains(t, view(poll(t, down)), "○ waiting for throtd at "+testSocket)

	// Too small for the art: a one-line wordmark.
	small := update(t, m, tea.WindowSizeMsg{Width: 40, Height: 12})
	out = view(small)
	assertFits(t, out, 40, 12)
	if strings.Contains(out, "█") || !strings.Contains(out, "THROT") {
		t.Errorf("narrow splash:\n%s", out)
	}
}

func TestSplashColor(t *testing.T) {
	c := &fakeClient{status: busyStatus()}
	for _, vars := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}} {
		m := newWithEnv(c, testSocket, env(vars))
		m.splash = true
		m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
		if out := m.View().Content; strings.ContainsRune(out, '\x1b') || !strings.Contains(out, "████████╗") {
			t.Errorf("%v: splash has escapes or lacks the art:\n%q", vars, out)
		}
	}
	m := newWithEnv(c, testSocket, env(map[string]string{"TERM": "xterm-256color"}))
	m.splash = true
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	dark := m.View().Content
	if !strings.ContainsRune(dark, '\x1b') {
		t.Error("colored splash has no color")
	}
	light := update(t, m, tea.BackgroundColorMsg{Color: color.White})
	if light.View().Content == dark {
		t.Error("splash ignores a light background")
	}
}
