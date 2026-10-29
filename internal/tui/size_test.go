package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/transport"
)

// harnessWidths and harnessHeights are the sizes every screen is rendered
// at; 0 and 1 check the degenerate cases.
var (
	harnessWidths  = []int{0, 1, 40, 60, 80, 120, 200}
	harnessHeights = []int{0, 1, 10, 24, 50}
)

// wideStatus is richStatus with East Asian wide and emoji text in every
// free-form field, and work the scheduler would queue in observe mode.
func wideStatus() app.StatusResult {
	st := richStatus()
	st.Agents[0].Agent = "에이전트-빌드-작업자"
	st.Heavy[2].Name = "コンパイラ☺️テスト"
	st.Heavy[2].Owner = "エージェント"
	st.Degraded = "process sampler: 時間切れ (ps timed out after 2s)"
	return st
}

// wideEvents are natural-sentence events, some with wide characters.
func wideEvents() []domain.Event {
	at := time.Date(2026, 10, 28, 9, 50, 0, 0, time.Local)
	return []domain.Event{
		{Seq: 1, Time: at, Kind: domain.EventDaemonStarted, Message: "throtd started in enforce mode"},
		{Seq: 2, Time: at.Add(time.Second), Kind: domain.EventProcessDiscovered, Message: "discovered heavy workload コンパイラ☺️テスト (pid 2002)"},
		{Seq: 3, Time: at.Add(2 * time.Second), Kind: domain.EventSchedulerDecision, Message: "queued gradle (pid 2000): pressure is critical"},
		{Seq: 4, Time: at.Add(3 * time.Second), Kind: domain.EventControlFailed, Message: ""},
	}
}

// harnessScreens returns every screen of the TUI built from fixed fake
// data at a comfortable size; the harness then resizes each one.
func harnessScreens(t *testing.T) map[string]Model {
	t.Helper()
	base := newModel(t, &fakeClient{status: wideStatus(), events: wideEvents()}, 120, 40)
	observe := wideStatus()
	observe.Scheduler.Mode = scheduler.Mode("observe")
	observe.Scheduler.Queued = nil
	observe.Scheduler.WouldQueue = []domain.ProcessKey{observe.Heavy[0].Key}
	notRunning := &transport.NotRunningError{Socket: testSocket, Err: errors.New("no such file or directory")}
	return map[string]Model{
		"main":        base,
		"details":     press(t, base, "tab", "enter"),
		"help":        press(t, base, "?"),
		"drill":       press(t, base, "enter"),
		"confirm":     press(t, base, "tab", "p"),
		"filter":      press(t, base, "/", "q"),
		"observe":     newModel(t, &fakeClient{status: observe, events: wideEvents()}, 120, 40),
		"empty":       newModel(t, &fakeClient{}, 120, 40),
		"down":        newModel(t, &fakeClient{err: notRunning}, 120, 40),
		"unreachable": newModel(t, &fakeClient{err: errors.New("permission denied")}, 120, 40),
		"connecting":  update(t, New(&fakeClient{}, testSocket), tea.WindowSizeMsg{Width: 120, Height: 40}),
		"unsized":     poll(t, New(&fakeClient{status: wideStatus()}, testSocket)),
	}
}

// TestRenderAtEverySize renders every screen at every harness size and
// checks that nothing panics, no line is wider than the window (in display
// cells, so wide runes count twice), no screen is blank, and the key facts
// survive whenever the window is not too small.
func TestRenderAtEverySize(t *testing.T) {
	for name, s := range harnessScreens(t) {
		for _, w := range harnessWidths {
			for _, h := range harnessHeights {
				t.Run(fmt.Sprintf("%s/%dx%d", name, w, h), func(t *testing.T) {
					m := s
					if name != "unsized" {
						m = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
					}
					out := view(m)
					if name == "unsized" || w == 0 || h == 0 {
						if out == "" {
							t.Fatal("blank screen")
						}
						return
					}
					assertFits(t, out, w, h)
					if strings.TrimSpace(out) == "" {
						t.Fatalf("blank screen")
					}
					if w < minWidth || h < minHeight {
						return
					}
					checkFacts(t, name, out, w)
				})
			}
		}
	}
}

// checkFacts asserts what each screen must always show at a usable size.
func checkFacts(t *testing.T, name, out string, w int) {
	t.Helper()
	switch name {
	case "down":
		assertContains(t, out, "throtd is not running")
		assertContains(t, out, "throtd`")
	case "unreachable":
		assertContains(t, out, "cannot reach throtd")
	case "connecting":
		assertContains(t, out, "connecting to throtd")
	case "empty":
		assertContains(t, out, "UNKNOWN")
	case "observe":
		assertContains(t, strings.ToLower(cells(out)), "critical", "observe", "limit 3 (", "1 would queue")
	default:
		// A narrow SCHEDULER panel shortens the adaptive range to (2–8).
		assertContains(t, cells(out), "CRITICAL", "limit 3 (", "1 queued")
	}
	if strings.Contains(out, "╭") && !strings.Contains(out, "PRESSURE") && !strings.Contains(out, "SYSTEM") {
		t.Errorf("bordered screen without a PRESSURE or SYSTEM panel:\n%s", out)
	}
	if name == "confirm" {
		// The question is the one thing to answer; it must be whole.
		assertContains(t, out, "y/n")
	}
}

// TestTruncateGraphemes checks that truncation counts display cells per
// grapheme cluster, so wide runes and emoji with variation selectors never
// overflow and are never split.
func TestTruncateGraphemes(t *testing.T) {
	for _, s := range []string{"コンパイラ☺️テスト", "에이전트-빌드", "e\u0301e\u0301e\u0301e\u0301", "👩‍💻👩‍💻👩‍💻", strings.Repeat("ab", 20)} {
		for w := range 12 {
			got := truncate(s, w)
			if gw := lipgloss.Width(got); gw > w {
				t.Errorf("truncate(%q, %d) = %q, %d wide", s, w, got, gw)
			}
			if got != s && w > 0 && !strings.HasSuffix(got, "…") {
				t.Errorf("truncate(%q, %d) = %q, want a trailing …", s, w, got)
			}
			if strings.ContainsRune(got, '\u200d') && !strings.Contains(s, got[:len(got)-len("…")]) {
				t.Errorf("truncate(%q, %d) split a cluster: %q", s, w, got)
			}
		}
	}
	// A styled string keeps its text when it fits after stripping.
	if got := truncate("\x1b[1mbold\x1b[0m", 4); stripStyles(got) != "bold" {
		t.Errorf("styled fit = %q", got)
	}
}

// TestObserveState checks that work observe mode would queue reads as the
// CLI says it, in the heavy table and the drilldown.
func TestObserveState(t *testing.T) {
	st := sampleStatus(1, 2)
	st.Scheduler = app.SchedulerStatus{Mode: scheduler.Mode("observe"), HeavyLimit: 1,
		Queued: []domain.ProcessKey{}, WouldQueue: []domain.ProcessKey{st.Heavy[1].Key}}
	m := newModel(t, &fakeClient{status: st}, 140, 40)
	out := view(m)
	if l := lineContaining(t, out, "2001"); !strings.Contains(l, "running (would queue)") {
		t.Errorf("would-queue row = %q", l)
	}
	if b := strings.Join(panelBody(t, out, "SCHEDULER"), "\n"); !strings.Contains(b, "OBSERVE\nlimit 1 · 1 would queue") {
		t.Errorf("observe scheduler panel = %q", b)
	}
	d := view(press(t, m, "enter"))
	if l := lineContaining(t, d, "2001"); !strings.Contains(l, "running (would queue)") {
		t.Errorf("drilldown would-queue row = %q", l)
	}
}

// TestBackgroundState checks that a workload throtd keeps at background
// priority reads "bg" while it runs, and QUEUED once queued.
func TestBackgroundState(t *testing.T) {
	st := sampleStatus(1, 2)
	st.Scheduler = app.SchedulerStatus{Mode: scheduler.Mode("enforce"), HeavyLimit: 2,
		Queued: []domain.ProcessKey{st.Heavy[0].Key}, WouldQueue: []domain.ProcessKey{}}
	st.Heavy[0].Background, st.Heavy[1].Background = true, true
	out := view(newModel(t, &fakeClient{status: st}, 140, 40))
	if l := lineContaining(t, out, "2001"); !strings.Contains(l, " bg") {
		t.Errorf("background row = %q", l)
	}
	if l := lineContaining(t, out, "2000"); !strings.Contains(l, "QUEUED") {
		t.Errorf("queued background row = %q", l)
	}
}

// TestDaemonDownMessage checks the not-running banner and that polling
// keeps going, so the TUI reconnects by itself.
func TestDaemonDownMessage(t *testing.T) {
	c := &fakeClient{err: &transport.NotRunningError{Socket: testSocket, Err: errors.New("no such file")}}
	m := update(t, newWithEnv(c, testSocket, os.Getenv), tea.WindowSizeMsg{Width: 100, Height: 30})
	next, cmd := m.Update(m.poll()())
	m = next.(Model)
	out := view(m)
	assertContains(t, out, "throtd is not running — start it with `throtd`", "retrying every 1s")
	if strings.Contains(out, "last error: throtd is not running") {
		t.Errorf("not-running error repeated:\n%s", out)
	}
	if cmd == nil {
		t.Fatal("no next poll scheduled while the daemon is down")
	}
	c.err, c.status = nil, sampleStatus(1, 0)
	m = poll(t, m)
	if out := view(m); strings.Contains(out, "not running") || !strings.Contains(out, "claude-1") {
		t.Errorf("did not recover:\n%s", out)
	}
}

// TestTimelineShowsSentences checks that event messages, which are whole
// sentences, are not prefixed with their kind, and that the kind stands in
// only for an event without a message. The filter matches either.
func TestTimelineShowsSentences(t *testing.T) {
	m := newModel(t, &fakeClient{status: sampleStatus(1, 1), events: wideEvents()}, 120, 40)
	lines := timelineLines(t, view(m))
	for _, l := range lines {
		if strings.Contains(l, string(domain.EventProcessDiscovered)) || strings.Contains(l, string(domain.EventSchedulerDecision)) {
			t.Errorf("timeline line repeats the kind: %q", l)
		}
	}
	assertContains(t, strings.Join(lines, "\n"), "SCHED  queued gradle (pid 2000)", "FAIL   "+string(domain.EventControlFailed))
	f := press(t, m, "/", "g", "r", "a", "d", "l", "e", "enter")
	if got := timelineLines(t, view(f)); len(got) != 1 || !strings.Contains(got[0], "queued gradle") {
		t.Errorf("message filter = %q", got)
	}
	f = press(t, m, "/", "d", "a", "e", "m", "o", "n", "enter")
	if got := timelineLines(t, view(f)); len(got) != 1 || !strings.Contains(got[0], "throtd started") {
		t.Errorf("kind filter = %q", got)
	}
}

// helpKeys are the keys the help lists, as pressed, with what each must
// visibly change. Every key handleKey acts on is here, and help lists each.
var helpKeys = []struct {
	key  string // as in press
	help string // as the help shows it
}{
	{"q", "q, ctrl+c"}, {"ctrl+c", "q, ctrl+c"},
	{"tab", "tab"}, {"j", "j/k, ↑/↓"}, {"k", "j/k, ↑/↓"}, {"down", "j/k, ↑/↓"}, {"up", "j/k, ↑/↓"},
	{"enter", "enter"}, {"esc", "esc"}, {"/", "/"}, {"p", "p / r"}, {"r", "p / r"},
	{"s", "s"}, {"?", "?"},
}

// TestHelpListsRealKeys checks the help and the key handling agree: each
// listed key does something on a screen where it applies, and no key that
// does something is missing from the help or the footers.
func TestHelpListsRealKeys(t *testing.T) {
	c := &fakeClient{status: sampleStatus(3, 3), events: sampleEvents()}
	base := newModel(t, c, 140, 50)
	help := "\n" + cells(view(press(t, base, "?")))
	for _, k := range helpKeys {
		if !strings.Contains(help, "\n"+k.help+" ") {
			t.Errorf("help does not list %q:\n%s", k.help, help)
		}
	}
	// Each key changes the screen, or quits, from a state where it applies.
	from := map[string]Model{
		"esc": press(t, base, "?"),
		"k":   press(t, base, "j"), "up": press(t, base, "j"),
		"p": press(t, base, "tab"), "r": press(t, base, "tab"),
	}
	for _, k := range helpKeys {
		m, ok := from[k.key]
		if !ok {
			m = base
		}
		before := view(m)
		next, cmd := m.Update(keyMsg(k.key))
		if cmd != nil {
			if _, quit := cmd().(tea.QuitMsg); quit {
				continue
			}
		}
		if view(next.(Model)) == before {
			t.Errorf("key %q changed nothing", k.key)
		}
	}
	// Keys that do nothing are not listed: x is not a key.
	if next, _ := base.Update(keyMsg("x")); view(next.(Model)) != view(base) {
		t.Error("x changed the screen")
	}
	for _, f := range []string{view(base), view(press(t, base, "enter"))} {
		footer := f[strings.LastIndex(f, "\n")+1:]
		if !strings.Contains(footer, "? help") || !strings.Contains(footer, "q quit") {
			t.Errorf("footer lacks ? help or q quit: %q", footer)
		}
	}
	if footer := lastLine(view(press(t, base, "tab"))); !strings.Contains(footer, "p/r pause/resume") {
		t.Errorf("heavy footer lacks p/r: %q", footer)
	}
}

// lineContaining returns the first line of out containing s.
func lineContaining(t *testing.T, out, s string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	t.Fatalf("no line contains %q:\n%s", s, out)
	return ""
}

func lastLine(s string) string { return s[strings.LastIndex(s, "\n")+1:] }

// keyMsg builds the key press for a key name as press uses it.
func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: rune(k[0]), Text: k}
}

// blockingClient blocks every call until its context ends.
type blockingClient struct{ fakeClient }

func (b *blockingClient) Status(ctx context.Context) (app.StatusResult, error) {
	<-ctx.Done()
	return app.StatusResult{}, ctx.Err()
}

// TestQuitStopsBackgroundWork checks that quitting ends an in-flight poll
// and a pending tick at once, so no goroutine outlives the program by a
// poll timeout or a tick.
func TestQuitStopsBackgroundWork(t *testing.T) {
	m := update(t, New(&blockingClient{}, testSocket), tea.WindowSizeMsg{Width: 100, Height: 30})
	pollCmd, tickCmd := m.poll(), m.tick(time.Hour, tickMsg{})
	next, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q did not quit")
	}
	_ = next
	done := make(chan tea.Msg, 2)
	go func() { done <- pollCmd() }()
	go func() { done <- tickCmd() }()
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("background work kept running after quit")
		}
	}
}

// TestPollCadence checks that exactly one poll is in flight: a poll result
// schedules one tick, a tick one poll, and nothing else schedules either.
func TestPollCadence(t *testing.T) {
	c := &fakeClient{status: sampleStatus(1, 1), events: sampleEvents()}
	m := newModel(t, c, 100, 30)
	for _, msg := range []tea.Msg{tea.WindowSizeMsg{Width: 90, Height: 30}, keyMsg("j"), keyMsg("?"), clearNoticeMsg{}} {
		if _, cmd := m.Update(msg); cmd != nil {
			t.Errorf("%T scheduled a command: polls must come only from ticks", msg)
		}
	}
	if pollInterval < time.Second {
		t.Errorf("poll interval %s hammers the daemon", pollInterval)
	}
}
