// Package tui implements throt's terminal UI. It is a client of throtd: it
// polls status and events and renders them, and it never makes scheduling
// decisions. Its only control is sending a pause or resume the user asked
// for and confirmed; the daemon decides whether to allow it. Closing it
// leaves the daemon untouched.
package tui

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

// Client is the daemon API the TUI needs.
type Client interface {
	Status(ctx context.Context) (app.StatusResult, error)
	// EventsSince returns up to limit events with a seq greater than seq,
	// oldest first, or the newest limit events when seq is 0.
	EventsSince(ctx context.Context, seq uint64, limit int) ([]domain.Event, error)
	// Pause and Resume send the privileged pause and resume methods.
	// Pause and Resume send the key, not just the PID, so a stale view never
	// acts on a process that reused the PID.
	Pause(ctx context.Context, key domain.ProcessKey) error
	Resume(ctx context.Context, key domain.ProcessKey) error
}

const (
	pollInterval = time.Second
	pollTimeout  = 2 * time.Second
	// eventLimit bounds how many events one poll fetches and keeps; the
	// timeline never shows more than a screenful.
	eventLimit = 100
	// controlTimeout bounds one pause or resume request.
	controlTimeout = 5 * time.Second
	// noticeTTL is how long a control result stays in the status line.
	noticeTTL = 5 * time.Second
	// maxFilterLen bounds the timeline filter, in runes.
	maxFilterLen = 32
)

// action is a pause or resume the user asked for, awaiting y/n.
type action struct {
	verb string // "pause" or "resume"
	key  domain.ProcessKey
	name string
}

// controlMsg carries the result of a pause or resume request.
type controlMsg struct {
	action
	err error
}

// clearNoticeMsg clears the status line if it still shows notice id.
type clearNoticeMsg struct{ id int }

// pane identifies a selectable table.
type pane int

const (
	paneAgents pane = iota
	paneHeavy
	// paneDrill is the workload table of the agent drilldown.
	paneDrill
	paneCount
)

// tablePanes is how many panes tab cycles through: the drilldown is
// entered with enter, not tab.
const tablePanes = paneDrill

// sortKey is a table's sort column. sortAuto, the default, sorts by memory
// while the pressure reason is a memory signal and by CPU otherwise;
// sortDaemon keeps throtd's order.
type sortKey int

const (
	sortAuto sortKey = iota
	sortCPU
	sortMem
	sortName
	sortDaemon
	sortCount
)

func (k sortKey) String() string {
	return [...]string{"auto", "cpu", "mem", "name", "daemon order"}[k]
}

// memoryReason reports whether a pressure reason code names a memory or
// swap signal (memory_used, memory_stall_*, kernel_memory_pressure,
// swap_growth, ...).
func memoryReason(code string) bool {
	name, _, _ := strings.Cut(code, ">=")
	return strings.Contains(name, "memory") || strings.HasPrefix(name, "swap")
}

// effectiveSort resolves sortAuto against the current pressure reason.
func (m Model) effectiveSort(k sortKey) sortKey {
	if k != sortAuto {
		return k
	}
	if memoryReason(m.status.Reason) {
		return sortMem
	}
	return sortCPU
}

// tickMsg asks for the next poll.
type tickMsg struct{}

// pollMsg carries one poll's results.
type pollMsg struct {
	status    app.StatusResult
	statusErr error
	events    []domain.Event
	eventsErr error
}

// Model is the bubbletea model for the TUI.
type Model struct {
	client Client
	socket string

	width, height int

	// connected is false until the first successful poll and after any
	// failed status call.
	connected bool
	polled    bool
	err       error
	status    app.StatusResult
	// agents and heavy are status.Agents and status.Heavy in display order.
	agents    []process.AgentView
	heavy     []process.WorkloadView
	events    []domain.Event
	eventsErr error
	// hist holds the newest host samples for the sparklines.
	hist history
	// lastSeq is the seq of the newest event held; polls ask only for
	// newer ones. 0 means fetch the newest eventLimit.
	lastSeq uint64
	// instance is the daemon run lastSeq belongs to.
	instance string

	// confirm is the action awaiting y/n, or nil.
	confirm *action
	// notice is the status line text; noticeID ties it to its clear timer.
	notice   string
	noticeID int

	focus pane
	// sel and off are the selected row and first visible row per pane.
	sel [paneCount]int
	off [paneCount]int
	// selKey keeps the selection on the same process when rows reorder.
	selKey [paneCount]domain.ProcessKey
	// sortBy is the sort column of the agents and heavy tables; the
	// drilldown uses the heavy table's.
	sortBy [tablePanes]sortKey

	// drill is true while the drilldown of agent drillKey is open. The
	// label selects its workloads, as status.Heavy reports owners by label.
	drill      bool
	drillKey   domain.ProcessKey
	drillLabel string

	details bool
	help    bool
	// splash is true while the startup splash shows; splashDone once
	// splashMin has passed. It hides after both that and the first poll,
	// or at the first key.
	splash, splashDone bool

	// filter narrows the timeline to events whose kind or message contains
	// it (case insensitive); filtering is true while the user types it after /.
	filter    string
	filtering bool

	// color is false under NO_COLOR or TERM=dumb; dark is the terminal
	// background as last reported, assumed dark until then. st follows both.
	color, dark bool
	st          styles

	// ctx ends background polls and ticks; stop cancels it when the user
	// quits, so none of them outlives the program. A confirmed pause or
	// resume is not tied to it: once sent, it runs to its own timeout.
	ctx  context.Context
	stop context.CancelFunc
}

// New returns a model that polls client and opens with the startup
// splash. socket is shown in the unreachable-daemon banner. NO_COLOR and
// TERM are read here.
func New(client Client, socket string) Model {
	m := newWithEnv(client, socket, os.Getenv)
	m.splash = true
	return m
}

// newWithEnv is New reading the environment through getenv.
func newWithEnv(client Client, socket string, getenv func(string) string) Model {
	color := colorEnabled(getenv)
	ctx, stop := context.WithCancel(context.Background())
	return Model{client: client, socket: socket, color: color, dark: true, st: newStyles(color, true), ctx: ctx, stop: stop}
}

// context returns the model's background context; a zero Model has none.
func (m Model) context() context.Context {
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

// quit stops background work and ends the program.
func (m Model) quit() tea.Cmd {
	if m.stop != nil {
		m.stop()
	}
	return tea.Quit
}

// Init starts polling immediately and, with color on, asks the terminal for
// its background so the colors keep their contrast on light themes too.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.poll()}
	if m.splash {
		cmds = append(cmds, m.tick(splashMin, splashDoneMsg{}))
	}
	if m.color {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

// poll fetches status and events under a timeout so a stuck daemon never
// blocks the UI. Only one poll is in flight at a time: the next tick is
// scheduled when its result arrives.
func (m Model) poll() tea.Cmd {
	c, since, parent := m.client, m.lastSeq, m.context()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, pollTimeout)
		defer cancel()
		var r pollMsg
		r.status, r.statusErr = c.Status(ctx)
		if r.statusErr == nil {
			r.events, r.eventsErr = c.EventsSince(ctx, since, eventLimit)
		}
		return r
	}
}

// send performs a confirmed action. The daemon checks ownership and
// identity; the TUI only reports the outcome.
func (m Model) send(a action) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
		defer cancel()
		var err error
		if a.verb == "pause" {
			err = c.Pause(ctx, a.key)
		} else {
			err = c.Resume(ctx, a.key)
		}
		return controlMsg{action: a, err: err}
	}
}

// setNotice shows s in the status line and schedules its removal.
func (m *Model) setNotice(s string) tea.Cmd {
	m.noticeID++
	m.notice = s
	return m.tick(noticeTTL, clearNoticeMsg{id: m.noticeID})
}

// tick delivers msg after d, or nothing once the user has quit. Unlike
// tea.Tick it does not hold a goroutine past quitting.
func (m Model) tick(d time.Duration, msg tea.Msg) tea.Cmd {
	ctx := m.context()
	return func() tea.Msg {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

// Update handles input, window resizes, and poll results.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(0, msg.Width), max(0, msg.Height)
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.st = newStyles(m.color, m.dark)
	case tickMsg:
		cmd = m.poll()
	case pollMsg:
		m.applyPoll(msg)
		m.splash = m.splash && !m.splashDone
		cmd = m.tick(pollInterval, tickMsg{})
	case splashDoneMsg:
		m.splashDone = true
		m.splash = m.splash && !m.polled
	case tea.KeyPressMsg:
		if m.splash {
			// Any key dismisses the splash; only the quit keys do more.
			m.splash = false
			if s := msg.String(); s != "q" && s != "ctrl+c" {
				break
			}
		}
		cmd = m.handleKey(msg)
	case controlMsg:
		target := fmt.Sprintf("%s (pid %d)", msg.name, msg.key.PID)
		if msg.err != nil {
			cmd = m.setNotice(msg.verb + " " + target + " failed: " + msg.err.Error())
		} else {
			cmd = m.setNotice(msg.verb + "d " + target)
		}
	case clearNoticeMsg:
		if msg.id == m.noticeID {
			m.notice = ""
		}
	}
	m.clampScroll()
	return m, cmd
}

func (m *Model) applyPoll(r pollMsg) {
	m.polled = true
	if r.statusErr != nil {
		// Drop the old data rather than show stale numbers as current.
		m.connected, m.err = false, r.statusErr
		m.status, m.events, m.eventsErr, m.lastSeq = app.StatusResult{}, nil, nil, 0
		m.hist = history{}
	} else {
		m.connected, m.err = true, nil
		m.status = r.status
		m.eventsErr = r.eventsErr
		if r.eventsErr == nil {
			m.addEvents(r.events)
		}
		if r.status.Host != nil {
			m.hist.add(*r.status.Host)
		}
	}
	m.resort()
}

// resort rebuilds the display order of both tables and keeps each
// selection on its process.
func (m *Model) resort() {
	m.agents = sortedAgents(m.status.Agents, m.effectiveSort(m.sortBy[paneAgents]))
	m.heavy = sortedHeavy(m.status.Heavy, m.effectiveSort(m.sortBy[paneHeavy]))
	for p := range paneCount {
		keys := m.keys(p)
		for i, k := range keys {
			if k == m.selKey[p] {
				m.sel[p] = i
				break
			}
		}
		m.setSel(p, m.sel[p])
	}
}

// sortedAgents returns agents ordered by k: CPU and memory descending, name
// ascending. Ties keep throtd's order. The input is never modified.
func sortedAgents(agents []process.AgentView, k sortKey) []process.AgentView {
	if k == sortDaemon {
		return agents
	}
	out := slices.Clone(agents)
	slices.SortStableFunc(out, func(a, b process.AgentView) int {
		switch k {
		case sortCPU:
			return cmp.Compare(b.CPU, a.CPU)
		case sortMem:
			return cmp.Compare(b.RSS, a.RSS)
		}
		return cmp.Compare(a.Agent, b.Agent)
	})
	return out
}

// sortedHeavy is sortedAgents for workloads; name is the process name.
func sortedHeavy(heavy []process.WorkloadView, k sortKey) []process.WorkloadView {
	if k == sortDaemon {
		return heavy
	}
	out := slices.Clone(heavy)
	slices.SortStableFunc(out, func(a, b process.WorkloadView) int {
		switch k {
		case sortCPU:
			return cmp.Compare(b.CPU, a.CPU)
		case sortMem:
			return cmp.Compare(b.RSS, a.RSS)
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return out
}

// addEvents merges one poll's events into the bounded history. A batch that
// does not continue after lastSeq means the daemon restarted (or does not
// number events), so it replaces the history.
func (m *Model) addEvents(events []domain.Event) {
	if len(events) == 0 {
		return
	}
	if m.instance != "" && events[0].Instance != m.instance {
		// throtd restarted; the next poll fetches the new run's newest events.
		m.instance, m.lastSeq, m.events = events[0].Instance, 0, nil
		return
	}
	m.instance = events[0].Instance
	if m.lastSeq == 0 || events[0].Seq <= m.lastSeq {
		m.events = slices.Clone(events)
	} else {
		// Clip so appending never writes into a slice an older model holds.
		m.events = append(slices.Clip(m.events), events...)
	}
	if len(m.events) > eventLimit {
		m.events = slices.Clone(m.events[len(m.events)-eventLimit:])
	}
	m.lastSeq = m.events[len(m.events)-1].Seq
}

func (m *Model) handleKey(k tea.KeyPressMsg) tea.Cmd {
	if a := m.confirm; a != nil {
		m.confirm = nil
		switch k.String() {
		case "y":
			return m.send(*a)
		case "ctrl+c":
			return m.quit()
		}
		return m.setNotice(a.verb + " cancelled")
	}
	if m.filtering {
		if k.String() == "ctrl+c" {
			return m.quit()
		}
		m.filterKey(k)
		return nil
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m.quit()
	case "p", "r":
		p := m.active()
		rows := m.workloads(p)
		if m.help || len(rows) == 0 {
			return nil
		}
		h := rows[m.sel[p]]
		verb := "pause"
		if k.String() == "r" {
			verb = "resume"
		}
		m.confirm = &action{verb: verb, key: h.Key, name: h.Name}
	case "?":
		m.help = !m.help
	case "/":
		if !m.help && !m.drill {
			m.filtering = true
		}
	case "esc":
		switch {
		case m.drill && !m.help:
			m.drill = false
		case m.help || m.details:
			m.help, m.details = false, false
		default:
			m.filter = ""
		}
	case "tab", "shift+tab":
		if !m.drill {
			m.focus = (m.focus + 1) % tablePanes
		}
	case "j", "down":
		p := m.active()
		m.setSel(p, m.sel[p]+1)
	case "k", "up":
		p := m.active()
		m.setSel(p, m.sel[p]-1)
	case "s":
		if m.help {
			return nil
		}
		t := m.sortPane(m.active())
		m.sortBy[t] = (m.sortBy[t] + 1) % sortCount
		m.resort()
	case "enter":
		switch {
		case m.help || m.drill:
		case m.focus == paneAgents:
			if len(m.agents) > 0 {
				a := m.agents[m.sel[paneAgents]]
				m.drill, m.drillKey, m.drillLabel = true, a.Key, a.Agent
				m.off[paneDrill] = 0
				m.setSel(paneDrill, 0)
			}
		default:
			m.details = !m.details
		}
	}
	return nil
}

// filterKey edits the timeline filter: text is appended up to
// maxFilterLen runes, backspace deletes, enter keeps the filter and esc
// clears it. Other keys are ignored while typing.
func (m *Model) filterKey(k tea.KeyPressMsg) {
	switch k.String() {
	case "enter":
		m.filtering = false
	case "esc":
		m.filtering, m.filter = false, ""
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
		}
	default:
		for _, r := range k.Text {
			if len([]rune(m.filter)) >= maxFilterLen {
				break
			}
			if unicode.IsPrint(r) {
				m.filter += string(r)
			}
		}
	}
}

// shownEvents returns the events the timeline lists, oldest first: those
// whose kind or message contains the filter, ignoring case.
func (m Model) shownEvents() []domain.Event {
	if m.filter == "" {
		return m.events
	}
	f := strings.ToLower(m.filter)
	var out []domain.Event
	for _, e := range m.events {
		if strings.Contains(strings.ToLower(string(e.Kind)), f) || strings.Contains(strings.ToLower(e.Message), f) {
			out = append(out, e)
		}
	}
	return out
}

// active is the pane j/k, s, p and r act on.
func (m Model) active() pane {
	if m.drill {
		return paneDrill
	}
	return m.focus
}

// sortPane is the table whose sort column pane p uses.
func (m Model) sortPane(p pane) pane {
	if p == paneDrill {
		return paneHeavy
	}
	return p
}

// workloads returns the heavy workloads pane p lists in display order, or
// nil for the agents pane.
func (m Model) workloads(p pane) []process.WorkloadView {
	switch p {
	case paneHeavy:
		return m.heavy
	case paneDrill:
		var out []process.WorkloadView
		for _, h := range m.heavy {
			if h.Owner == m.drillLabel {
				out = append(out, h)
			}
		}
		return out
	}
	return nil
}

// drillAgent returns the drilled-into agent as last reported.
func (m Model) drillAgent() (process.AgentView, bool) {
	i := slices.IndexFunc(m.agents, func(a process.AgentView) bool { return a.Key == m.drillKey })
	if i < 0 {
		return process.AgentView{}, false
	}
	return m.agents[i], true
}

// keys returns the row identities of pane p.
func (m Model) keys(p pane) []domain.ProcessKey {
	var keys []domain.ProcessKey
	if p == paneAgents {
		for _, a := range m.agents {
			keys = append(keys, a.Key)
		}
	} else {
		for _, h := range m.workloads(p) {
			keys = append(keys, h.Key)
		}
	}
	return keys
}

// setSel moves pane p's selection to i, clamped to its rows.
func (m *Model) setSel(p pane, i int) {
	keys := m.keys(p)
	m.sel[p] = max(0, min(i, len(keys)-1))
	if len(keys) > 0 {
		m.selKey[p] = keys[m.sel[p]]
	} else {
		m.selKey[p] = domain.ProcessKey{}
	}
}

// agentsCollapsed reports whether the agents table, given rows rows, shows
// its first rows-1 agents and a summary of the rest: when they do not all
// fit and the selection is among the first ones. Moving the selection past
// them scrolls through the rest as usual.
func (m Model) agentsCollapsed(rows int) bool {
	return rows >= 2 && len(m.agents) > rows && m.sel[paneAgents] < rows-1
}

// clampScroll keeps each selection inside its pane's visible window.
func (m *Model) clampScroll() {
	l := m.layout()
	rows := [paneCount]int{l.agentRows, l.heavyRows, l.drillRows}
	for p := range paneCount {
		n := len(m.keys(p))
		h := max(rows[p], 1)
		if p == paneAgents && m.agentsCollapsed(rows[p]) {
			m.off[p] = 0
			continue
		}
		if m.sel[p] < m.off[p] {
			m.off[p] = m.sel[p]
		}
		if m.sel[p] >= m.off[p]+h {
			m.off[p] = m.sel[p] - h + 1
		}
		m.off[p] = max(0, min(m.off[p], n-h))
	}
}
