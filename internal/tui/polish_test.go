package tui

import (
	"errors"
	"image/color"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// env returns a getenv over vars.
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func newEnvModel(t *testing.T, c *fakeClient, getenv func(string) string, w, h int) Model {
	t.Helper()
	m := update(t, newWithEnv(c, testSocket, getenv), tea.WindowSizeMsg{Width: w, Height: h})
	return poll(t, m)
}

// richStatus exercises every styled element: pressure, reason, degraded,
// stopped orphans, an adaptive scheduler with queued work and leases, and
// a paused workload.
func richStatus() app.StatusResult {
	st := sampleStatus(3, 3)
	st.Pressure = domain.PressureCritical
	st.Degraded = "process sampler: ps timed out"
	st.OrphanCount = 2
	st.Paused = []control.PausedWorkload{{Key: st.Heavy[1].Key}}
	st.Scheduler = app.SchedulerStatus{
		Mode: scheduler.Mode("enforce"), HeavyLimit: 3,
		Adaptive: true, StartLimit: 2, MaxLimit: 8, PerAgentLimit: 2,
		Queued: []domain.ProcessKey{st.Heavy[0].Key},
		Leases: []app.Lease{{ID: "a"}, {ID: "b"}},
	}
	return st
}

func classEvents() []domain.Event {
	at := time.Date(2026, 10, 28, 9, 40, 0, 0, time.Local)
	kinds := []domain.EventKind{
		domain.EventDaemonStarted, domain.EventControlPaused, domain.EventSchedulerDecision,
		domain.EventSchedulerLimit, domain.EventControlFailed, domain.EventLeaseGranted,
		domain.EventPressureChanged, domain.EventConfigReloadFailed,
	}
	var out []domain.Event
	for i, k := range kinds {
		out = append(out, domain.Event{Seq: uint64(i + 1), Time: at.Add(time.Duration(i) * time.Second), Kind: k, Message: "msg " + string(k)})
	}
	return out
}

func TestColorEnabled(t *testing.T) {
	for _, tc := range []struct {
		vars map[string]string
		want bool
	}{
		{map[string]string{"TERM": "xterm-256color"}, true},
		{map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, false},
		// An empty NO_COLOR does not count (no-color.org).
		{map[string]string{"TERM": "xterm-256color", "NO_COLOR": ""}, true},
		{map[string]string{"TERM": "dumb"}, false},
	} {
		if got := colorEnabled(env(tc.vars)); got != tc.want {
			t.Errorf("colorEnabled(%v) = %v, want %v", tc.vars, got, tc.want)
		}
	}
}

func TestNoColorHasNoEscapes(t *testing.T) {
	for _, vars := range []map[string]string{{"NO_COLOR": "1", "TERM": "xterm"}, {"TERM": "dumb"}} {
		c := &fakeClient{status: richStatus(), events: classEvents()}
		m := newEnvModel(t, c, env(vars), 140, 50)
		if m.Init() == nil {
			t.Fatal("Init returned no poll")
		}
		screens := map[string]Model{
			"main":      m,
			"details":   press(t, m, "tab", "enter"),
			"help":      press(t, m, "?"),
			"drill":     press(t, m, "enter"),
			"confirm":   press(t, m, "tab", "p"),
			"filter":    press(t, m, "/", "s"),
			"too small": update(t, m, tea.WindowSizeMsg{Width: 30, Height: 5}),
		}
		// The banner of an unreachable daemon is styled too.
		down := &fakeClient{err: errors.New("connect: refused")}
		screens["down"] = newEnvModel(t, down, env(vars), 100, 30)
		for name, s := range screens {
			if out := s.View().Content; strings.ContainsRune(out, '\x1b') {
				t.Errorf("%v %s: view has an escape byte:\n%q", vars, name, out)
			}
		}
		out := m.View().Content
		// Every state stays readable as text.
		assertContains(t, out, "CRITICAL", "DEGRADED", "STOPPED  2 processes", "QUEUED", "PAUSED",
			"> ", "▸ AGENTS", "FAIL", "SCHED", "CTL")
	}
	// With color the same screen is styled.
	c := &fakeClient{status: richStatus(), events: classEvents()}
	m := newEnvModel(t, c, env(map[string]string{"TERM": "xterm-256color"}), 140, 50)
	if !strings.ContainsRune(m.View().Content, '\x1b') {
		t.Error("color view has no styling")
	}
}

func TestBackgroundColorSwitchesPalette(t *testing.T) {
	c := &fakeClient{status: richStatus(), events: classEvents()}
	m := newEnvModel(t, c, env(map[string]string{"TERM": "xterm-256color"}), 140, 50)
	dark := m.View().Content
	m = update(t, m, tea.BackgroundColorMsg{Color: color.White})
	if m.dark {
		t.Fatal("white background reported as dark")
	}
	light := m.View().Content
	if light == dark {
		t.Error("light background kept the dark palette")
	}
	if view(m) != ansiRE.ReplaceAllString(dark, "") {
		t.Error("palette change altered the text")
	}
	// Plain stays plain whatever the background.
	p := newEnvModel(t, c, env(map[string]string{"NO_COLOR": "1"}), 140, 50)
	p = update(t, p, tea.BackgroundColorMsg{Color: color.White})
	if strings.ContainsRune(p.View().Content, '\x1b') {
		t.Error("NO_COLOR view styled after a background report")
	}
}

func TestSchedulerLine(t *testing.T) {
	c := &fakeClient{status: richStatus(), events: classEvents()}
	// One column: the SCHEDULER line of the SYSTEM panel.
	m := newModel(t, c, 79, 50)
	line := strings.Join(panelBody(t, view(m), "SYSTEM"), " ")
	for _, want := range []string{"SCHEDULER  enforce, limit 3 (adaptive, start 2, max 8), per-agent 2,", "1 queued, 2 reserved"} {
		if !strings.Contains(line, want) {
			t.Errorf("scheduler line %q lacks %q", line, want)
		}
	}
	assertContains(t, view(m), "STOPPED  2 processes not paused by throtd (see throt doctor)")
	// The dashboard's SCHEDULER panel: the mode, then the items as fit.
	for w, limit := range map[int]string{140: "limit 3 (2–8)", 200: "limit 3 (adaptive, start 2, max 8)"} {
		body := strings.Join(panelBody(t, view(update(t, m, tea.WindowSizeMsg{Width: w, Height: 50})), "SCHEDULER"), "\n")
		assertContains(t, body, "ENFORCE\n"+limit, "1 queued", "per-agent 2", "2 reserved")
	}

	// A fixed limit shows no adaptive range; one orphan is singular.
	st := richStatus()
	st.Scheduler.Adaptive, st.Scheduler.StartLimit, st.Scheduler.MaxLimit, st.Scheduler.PerAgentLimit = false, 0, 0, 0
	st.Scheduler.Leases = nil
	st.OrphanCount = 1
	m = newModel(t, &fakeClient{status: st}, 79, 50)
	line = lineWith(t, view(m), "SCHEDULER")
	if strings.Contains(line, "adaptive") || strings.Contains(line, "per-agent") || !strings.Contains(line, "enforce, limit 3, 1 queued, 0 reserved") {
		t.Errorf("fixed scheduler line = %q", line)
	}
	assertContains(t, view(m), "STOPPED  1 process not")

	// Off: no scheduler line, and no orphan line without orphans; the
	// dashboard's panel says OFF.
	st.Scheduler = app.SchedulerStatus{Mode: scheduler.Mode("off")}
	st.OrphanCount = 0
	out := view(newModel(t, &fakeClient{status: st}, 79, 50))
	if strings.Contains(out, "SCHEDULER") || strings.Contains(out, "STOPPED") {
		t.Errorf("scheduler off or no orphans still shown:\n%s", out)
	}
	out = view(newModel(t, &fakeClient{status: st}, 140, 50))
	if body := panelBody(t, out, "SCHEDULER"); body[0] != "OFF" || strings.Contains(out, "STOPPED") {
		t.Errorf("scheduler off: panel %q", body)
	}
}

// timelineLines returns the event lines of the timeline.
func timelineLines(t *testing.T, out string) []string {
	t.Helper()
	var evs []string
	for _, l := range panelBody(t, out, "EVENTS") {
		if strings.TrimSpace(l) == "" {
			break
		}
		evs = append(evs, l)
	}
	return evs
}

func TestTimelineTags(t *testing.T) {
	m := newModel(t, &fakeClient{status: sampleStatus(1, 1), events: classEvents()}, 140, 60)
	want := map[domain.EventKind]string{
		domain.EventDaemonStarted: "     ", domain.EventPressureChanged: "     ",
		domain.EventControlPaused: "CTL  ", domain.EventSchedulerDecision: "SCHED",
		domain.EventSchedulerLimit: "SCHED", domain.EventLeaseGranted: "SCHED",
		domain.EventControlFailed: "FAIL ", domain.EventConfigReloadFailed: "FAIL ",
	}
	lines := timelineLines(t, view(m))
	if len(lines) != len(want) {
		t.Fatalf("timeline has %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		for k, tag := range want {
			if strings.Contains(l, " "+string(k)+" ") && l[10:10+eventTagWidth] != tag {
				t.Errorf("line %q: tag %q, want %q", l, l[10:10+eventTagWidth], tag)
			}
		}
	}
	if classify("future_thing_failed") != eventFailure || classify("future_thing") != eventOther {
		t.Error("unknown kinds misclassified")
	}
}

func TestTimelineFilter(t *testing.T) {
	m := newModel(t, &fakeClient{status: sampleStatus(1, 1), events: classEvents()}, 140, 60)
	m = press(t, m, "/")
	if !m.filtering {
		t.Fatal("/ did not start the filter")
	}
	// Keys type while filtering: q does not quit, s does not sort.
	m = press(t, m, "S", "c", "h", "q")
	if m.filter != "Schq" || m.sortBy[paneAgents] != sortAuto {
		t.Fatalf("filter = %q, sort %v", m.filter, m.sortBy[paneAgents])
	}
	m = update(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	assertContains(t, view(m), "filter: Sch_ (2 of 8)", "enter keep")
	m = press(t, m, "enter")
	if m.filtering || m.filter != "Sch" {
		t.Fatalf("enter: filtering %v filter %q", m.filtering, m.filter)
	}
	out := view(m)
	assertContains(t, out, "filter: Sch (2 of 8)", "scheduler_limit", "scheduler_decision", "/ filter")
	for _, gone := range []string{"control_paused", "daemon_started", "lease_granted"} {
		if strings.Contains(out, gone) {
			t.Errorf("filtered timeline shows %s", gone)
		}
	}
	// The filter survives polls.
	m = poll(t, m)
	assertContains(t, view(m), "filter: Sch (2 of 8)")

	// No match says so.
	m = press(t, m, "/", "z", "enter")
	assertContains(t, view(m), "(0 of 8)", "no event matches")

	// esc clears it, outside and inside the editor.
	m = press(t, m, "esc")
	if m.filter != "" || strings.Contains(view(m), "filter:") {
		t.Fatalf("esc kept filter %q", m.filter)
	}
	m = press(t, m, "/", "f", "a", "esc")
	if m.filter != "" || m.filtering {
		t.Fatalf("esc in the editor: filter %q filtering %v", m.filter, m.filtering)
	}
	// esc closes details before it clears the filter.
	m = press(t, m, "/", "f", "enter", "tab", "enter", "esc")
	if m.details || m.filter != "f" {
		t.Fatalf("esc: details %v filter %q", m.details, m.filter)
	}

	// The filter is bounded.
	m = press(t, m, "esc", "/")
	for range 3 * maxFilterLen {
		m = press(t, m, "x")
	}
	m = update(t, m, tea.KeyPressMsg{Code: 'y', Text: strings.Repeat("y", 100)})
	if n := len([]rune(m.filter)); n != maxFilterLen {
		t.Errorf("filter is %d runes, want %d", n, maxFilterLen)
	}
	assertFits(t, view(m), 140, 60)

	// ctrl+c still quits while typing.
	_, cmd := pressCmd(t, m, "ctrl+c")
	if cmd == nil {
		t.Fatal("ctrl+c while filtering returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c while filtering did not quit")
	}
	// / does nothing in help or the drilldown.
	m = press(t, newModel(t, &fakeClient{status: sampleStatus(1, 1)}, 100, 30), "?", "/")
	if m.filtering {
		t.Error("/ started a filter in help")
	}
}

// TestResizeSweep drives random window sizes from 1x1 to 300x100 through
// Update and View in every screen and checks that nothing panics and every
// line fits the window.
func TestResizeSweep(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	st := richStatus()
	st.Agents[0].Agent = strings.Repeat("long-agent-label-", 8)
	st.Heavy[0].Name = strings.Repeat("process", 20)
	st.Reason = strings.Repeat("why ", 50)
	ev := classEvents()
	ev[0].Message = strings.Repeat("a very long message ", 30)
	c := &fakeClient{status: st, events: ev}
	base := newModel(t, c, 120, 40)
	screens := map[string]Model{
		"main":    base,
		"details": press(t, base, "tab", "enter"),
		"help":    press(t, base, "?"),
		"drill":   press(t, base, "enter"),
		"confirm": press(t, base, "tab", "p"),
		"filter":  press(t, base, "/", "s", "c"),
		"down":    newModel(t, &fakeClient{err: errors.New("connect: refused")}, 120, 40),
	}
	sizes := [][2]int{{1, 1}, {300, 100}, {1, 100}, {300, 1}, {minWidth, minHeight}, {minWidth - 1, minHeight}, {minWidth, minHeight - 1}}
	for range 300 {
		sizes = append(sizes, [2]int{1 + r.IntN(300), 1 + r.IntN(100)})
	}
	for name, s := range screens {
		m := s
		for _, sz := range sizes {
			w, h := sz[0], sz[1]
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("%s at %dx%d: panic %v", name, w, h, p)
					}
				}()
				m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
				// Moving and polling at this size must keep fitting too.
				m = press(t, m, "j")
				out := view(m)
				assertFits(t, out, w, h)
				if (w < minWidth || h < minHeight) && !strings.HasPrefix(out, "terminal"[:min(8, w)]) {
					t.Errorf("%s at %dx%d: want the too-small message, got %q", name, w, h, out)
				}
			}()
			if t.Failed() {
				t.Fatalf("%s failed at %dx%d", name, w, h)
			}
		}
	}
}
