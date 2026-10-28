// Package tui implements throt's terminal UI. It is a read-only client of
// throtd: it polls status and events and renders them, and it never makes
// scheduling decisions or controls processes. Closing it leaves the daemon
// untouched.
package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// Client is the daemon API the TUI needs.
type Client interface {
	Status(ctx context.Context) (app.StatusResult, error)
	Events(ctx context.Context, limit int) ([]domain.Event, error)
}

const (
	pollInterval = time.Second
	pollTimeout  = 2 * time.Second
	// eventLimit bounds how many events one poll fetches and keeps; the
	// timeline never shows more than a screenful.
	eventLimit = 100
)

// pane identifies a selectable table.
type pane int

const (
	paneAgents pane = iota
	paneHeavy
	paneCount
)

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
	events    []domain.Event
	eventsErr error

	focus pane
	// sel and off are the selected row and first visible row per pane.
	sel [paneCount]int
	off [paneCount]int
	// selKey keeps the selection on the same process when rows reorder.
	selKey [paneCount]domain.ProcessKey

	details bool
	help    bool
}

// New returns a model that polls client. socket is shown in the
// unreachable-daemon banner.
func New(client Client, socket string) Model {
	return Model{client: client, socket: socket}
}

// Init starts polling immediately.
func (m Model) Init() tea.Cmd { return m.poll() }

// poll fetches status and events under a timeout so a stuck daemon never
// blocks the UI. Only one poll is in flight at a time: the next tick is
// scheduled when its result arrives.
func (m Model) poll() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		var r pollMsg
		r.status, r.statusErr = c.Status(ctx)
		if r.statusErr == nil {
			r.events, r.eventsErr = c.Events(ctx, eventLimit)
		}
		return r
	}
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// Update handles input, window resizes, and poll results.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		cmd = m.poll()
	case pollMsg:
		m.applyPoll(msg)
		cmd = tick()
	case tea.KeyPressMsg:
		cmd = m.handleKey(msg)
	}
	m.clampScroll()
	return m, cmd
}

func (m *Model) applyPoll(r pollMsg) {
	m.polled = true
	if r.statusErr != nil {
		// Drop the old data rather than show stale numbers as current.
		m.connected, m.err = false, r.statusErr
		m.status, m.events, m.eventsErr = app.StatusResult{}, nil, nil
	} else {
		m.connected, m.err = true, nil
		m.status = r.status
		m.eventsErr = r.eventsErr
		if r.eventsErr == nil {
			m.events = r.events
			if len(m.events) > eventLimit {
				m.events = m.events[len(m.events)-eventLimit:]
			}
		}
	}
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

func (m *Model) handleKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "?":
		m.help = !m.help
	case "esc":
		m.help, m.details = false, false
	case "tab", "shift+tab":
		m.focus = (m.focus + 1) % paneCount
	case "j", "down":
		m.setSel(m.focus, m.sel[m.focus]+1)
	case "k", "up":
		m.setSel(m.focus, m.sel[m.focus]-1)
	case "enter":
		m.details = !m.details
	}
	return nil
}

// keys returns the row identities of pane p.
func (m Model) keys(p pane) []domain.ProcessKey {
	var keys []domain.ProcessKey
	if p == paneAgents {
		for _, a := range m.status.Agents {
			keys = append(keys, a.Key)
		}
	} else {
		for _, h := range m.status.Heavy {
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

// clampScroll keeps each selection inside its pane's visible window.
func (m *Model) clampScroll() {
	l := m.layout()
	rows := [paneCount]int{l.agentRows, l.heavyRows}
	for p := range paneCount {
		n := len(m.keys(p))
		h := max(rows[p], 1)
		if m.sel[p] < m.off[p] {
			m.off[p] = m.sel[p]
		}
		if m.sel[p] >= m.off[p]+h {
			m.off[p] = m.sel[p] - h + 1
		}
		m.off[p] = max(0, min(m.off[p], n-h))
	}
}
