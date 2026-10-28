package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/app"
)

// wordmark is the splash's block-letter "THROT": full blocks are the face,
// box-drawing characters the shadow layer below and to the right of it.
const wordmark = `████████╗██╗  ██╗██████╗  ██████╗ ████████╗
╚══██╔══╝██║  ██║██╔══██╗██╔═══██╗╚══██╔══╝
   ██║   ███████║██████╔╝██║   ██║   ██║
   ██║   ██╔══██║██╔══██╗██║   ██║   ██║
   ██║   ██║  ██║██║  ██║╚██████╔╝   ██║
   ╚═╝   ╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝    ╚═╝`

const (
	// wordmarkWidth and wordmarkHeight are the art's size in cells.
	wordmarkWidth  = 43
	wordmarkHeight = 6
	// splashMin is how long the splash stays at least, unless a key
	// dismisses it; it then stays until the first poll has answered.
	splashMin = 1500 * time.Millisecond
	tagline   = "resource-aware scheduler for coding agents"
)

// splashDoneMsg says splashMin has passed.
type splashDoneMsg struct{}

// splashLines renders the startup splash: the wordmark (or, when it does
// not fit, a one-line "THROT"), the tagline, the version and the daemon
// connection state, centered in the window.
func (m Model) splashLines() []string {
	state := "connecting to throtd at " + m.socket + "…"
	switch {
	case m.polled && m.connected:
		state = "● connected to throtd"
	case m.polled:
		state = "○ waiting for throtd at " + m.socket
	}
	info := []string{"Throt: " + tagline, "version " + app.Version, state, "", "any key to continue  q quit"}

	var art []string
	if m.width >= wordmarkWidth+2 && m.height >= wordmarkHeight+len(info)+2 {
		for i, l := range splitLines(wordmark) {
			art = append(art, m.paintWordmark(i, l))
		}
		art = append(art, "")
	} else {
		art = []string{m.st.title.Render("THROT")}
	}
	lines := append(art, info...)
	top := max(0, (m.height-len(lines))/2)
	out := make([]string, top, m.height)
	for i, l := range lines {
		w := wordmarkWidth
		if i >= len(art) || len(art) == 1 {
			w = lipgloss.Width(l)
		}
		pad := strings.Repeat(" ", max(0, (m.width-w)/2))
		out = append(out, strings.TrimRight(truncate(pad+l, m.width), " "))
	}
	return out[:min(len(out), m.height)]
}

// paintWordmark colors row i of the wordmark: its blocks with row i of the
// gradient, the shadow characters with the shadow style.
func (m Model) paintWordmark(i int, line string) string {
	const (
		space = iota
		face
		shadow
	)
	var b, run strings.Builder
	kind := space
	flush := func() {
		switch s := run.String(); kind {
		case face:
			b.WriteString(m.st.splash[i].Render(s))
		case shadow:
			b.WriteString(m.st.splashShadow.Render(s))
		default:
			b.WriteString(s)
		}
		run.Reset()
	}
	for _, r := range line {
		k := shadow
		switch r {
		case ' ':
			k = space
		case '█':
			k = face
		}
		if k != kind {
			flush()
			kind = k
		}
		run.WriteRune(r)
	}
	flush()
	return b.String()
}
