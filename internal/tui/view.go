package tui

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/transport"
)

const (
	minWidth  = 40
	minHeight = 10
	// marker prefixes every table row; the selected row in the focused pane
	// gets "> " so selection never depends on color alone.
	markerWidth = 2
	// In the one-column layout's SYSTEM panel, sparklines get up to
	// maxSparkWidth cells and are dropped, before anything else, when fewer
	// than minSparkWidth fit beside a bar of minBarWidth.
	maxSparkWidth = 10
	minSparkWidth = 6
	minBarWidth   = 10
	maxBarWidth   = 20
	// barOverhead is the label, spaces and percentage around a bar;
	// barDetailWidth is what a bar leaves for its detail text.
	barOverhead    = 14
	barDetailWidth = 21
)

// View renders the whole screen.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "throt"
	return v
}

func (m Model) render() string {
	if m.width == 0 || m.height == 0 {
		return "starting…"
	}
	if m.width < minWidth || m.height < minHeight {
		lines := splitLines(lipgloss.Wrap(fmt.Sprintf("terminal too small (%dx%d); throt needs at least %dx%d. Press q to quit.",
			m.width, m.height, minWidth, minHeight), m.width, ""))
		for i, l := range lines {
			lines[i] = truncate(l, m.width)
		}
		return strings.Join(lines[:min(len(lines), m.height)], "\n")
	}
	if m.splash {
		return strings.Join(m.splashLines(), "\n")
	}
	l := m.layout()
	lines := append([]string{}, l.top...)
	if l.grid {
		lines = append(lines, m.gridLines(l)...)
	} else {
		lines = append(lines, m.columnLines(l)...)
	}
	// Fill so the footer sits on the last row, and never exceed the height.
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:m.height-1], m.footer())
	for i, s := range lines {
		lines[i] = strings.TrimRight(s, " ")
	}
	return strings.Join(lines, "\n")
}

const (
	// maxAgentRows and maxHeavyRows are the most rows each table gets.
	maxAgentRows = 10
	maxHeavyRows = 8
	// gridMinWidth is the narrowest dashboard grid; below it panels stack
	// in one column. twoColumnWidth is where the memory gauge moves beside
	// the tables.
	gridMinWidth   = 80
	twoColumnWidth = 100
	// minEventsHeight is the EVENTS panel with three events.
	minEventsHeight = 5
	// minChartHeight is a time-series panel three rows high.
	minChartHeight = 5
)

// layout is where every panel goes and how many rows each table shows.
// clampScroll uses the row counts, so render and scrolling agree.
type layout struct {
	// top holds banners and alerts, across the whole width.
	top []string
	// grid is the dashboard grid; otherwise panels stack in one column,
	// bare (without borders) when rows are scarce.
	grid, bare bool
	// avail is the height below top and above the footer.
	avail int
	// Grid: the stat row, the time-series row, the MEMORY BY OWNER panel
	// (beside the tables, or below them on a narrower grid) and the EVENTS
	// panel heights; tableW is the table column's width.
	statH, chartH, gaugeH, eventsH, detailsH, tableW int
	// sideBySide puts the agents and heavy tables next to each other, both
	// agentRows high.
	sideBySide bool
	// sys is the one-column SYSTEM panel's body.
	sys []string
	// drillInfo is the drilled-into agent's summary.
	drillInfo                                     []string
	agentRows, heavyRows, timelineRows, drillRows int
}

// panel overheads: a table has a title (or top border), a column header
// and, bordered, a bottom border.
func (l layout) tableOverhead() int {
	if l.bare {
		return 2
	}
	return 3
}

// boxOverhead is the rows a panel's frame takes: borders, or a bare title.
func (l layout) boxOverhead(titled bool) int {
	switch {
	case !l.bare:
		return 2
	case titled:
		return 1
	}
	return 0
}

// layout splits the window between panels. Panels drop in priority order on
// short terminals: the stat panels (or SYSTEM) first, then the focused
// table, the other table, the time series, events and the memory gauge.
func (m Model) layout() layout {
	l := layout{top: m.topLines()}
	if m.width == 0 || m.height == 0 {
		return l
	}
	l.avail = m.height - len(l.top) - 1
	l.statH = 5
	if l.avail >= 26 {
		l.statH = 6
	}
	if m.width >= wideWidth && l.avail >= 40 {
		l.statH = 8
	}
	l.grid = m.width >= gridMinWidth && l.avail >= 5+8
	if l.grid {
		m.gridLayout(&l)
	} else {
		m.columnLayout(&l)
	}
	return l
}

// agentsWant and heavyWant are the rows the tables would like.
func (m Model) agentsWant() int { return max(1, min(len(m.status.Agents), maxAgentRows)) }
func (m Model) heavyWant() int  { return max(1, min(len(m.status.Heavy), maxHeavyRows)) }

func (m Model) gridLayout(l *layout) {
	w := m.width
	l.tableW = w
	if w >= twoColumnWidth {
		l.tableW = w - gaugeWidth(w) - gridGap
	}
	rest := l.avail - l.statH
	if m.help {
		return
	}
	if m.drill {
		l.drillInfo = m.drillInfoLines(innerWidth(w))
		if room := max(1, rest-2-4); len(l.drillInfo) > room {
			l.drillInfo = l.drillInfo[:room]
		}
		l.drillRows = max(1, rest-(2+len(l.drillInfo))-3)
		// The main tables keep the sizes computed below, so their
		// scroll positions survive the drilldown.
	}
	if m.details && !m.drill {
		_, body := m.detailsLines(innerWidth(l.tableW))
		l.detailsH = min(2+len(body), max(0, rest-4))
	}
	rest -= l.detailsH
	l.sideBySide = l.tableW >= sideBySideWidth && rest >= 4
	if l.sideBySide {
		m.sideBySideLayout(l, rest)
		return
	}
	if rest < 8 {
		// Details leave room for one table: the focused one.
		if m.focus == paneAgents {
			l.agentRows = max(1, rest-3)
		} else {
			l.heavyRows = max(1, rest-3)
		}
		return
	}
	l.agentRows, l.heavyRows = 1, 1
	rest -= 8
	take := func(want int) int {
		n := max(0, min(want, rest))
		rest -= n
		return n
	}
	// A few rows of each table, then events, then the time series; then
	// the tables grow, the gauge, the charts, and events take the rest.
	l.agentRows += take(min(m.agentsWant(), 3) - 1)
	l.heavyRows += take(min(m.heavyWant(), 2) - 1)
	if rest >= minEventsHeight {
		l.eventsH = take(minEventsHeight)
	}
	if rest >= minChartHeight {
		l.chartH = take(minChartHeight)
	}
	l.agentRows += take(m.agentsWant() - l.agentRows)
	l.heavyRows += take(m.heavyWant() - l.heavyRows)
	if w < twoColumnWidth && rest >= 4 {
		// The gauge goes below the tables: its totals and the largest owners.
		l.gaugeH = take(2 + 1 + min(len(m.owners()), 5))
	}
	if l.chartH > 0 {
		want := 8
		if w >= wideWidth {
			want = 10
		}
		l.chartH += take(want - l.chartH)
	}
	if l.eventsH > 0 {
		l.eventsH += rest
	} else {
		l.agentRows += take(len(m.status.Agents) - l.agentRows)
	}
	if l.eventsH > 0 {
		l.timelineRows = l.eventsH - 2
	}
}

// sideBySideWidth is the table column width from which the agents and
// heavy tables sit side by side.
const sideBySideWidth = 120

// sideBySideLayout is gridLayout's split of rest rows when the tables sit
// side by side: they share one height.
func (m Model) sideBySideLayout(l *layout, rest int) {
	rows := 1
	rest -= 4
	take := func(want int) int {
		n := max(0, min(want, rest))
		rest -= n
		return n
	}
	want := max(m.agentsWant(), m.heavyWant())
	rows += take(min(want, 3) - 1)
	if rest >= minEventsHeight {
		l.eventsH = take(minEventsHeight)
	}
	if rest >= minChartHeight {
		l.chartH = take(minChartHeight)
	}
	rows += take(want - rows)
	if l.chartH > 0 {
		l.chartH += take(10 - l.chartH)
	}
	if l.eventsH > 0 {
		l.eventsH += rest
		l.timelineRows = l.eventsH - 2
	} else {
		rows += take(len(m.status.Agents) - rows)
	}
	l.agentRows, l.heavyRows = rows, rows
}

// gaugeWidth is the MEMORY BY OWNER panel's width beside the tables.
func gaugeWidth(w int) int { return max(36, min(w/3, 64)) }

// columnLayout stacks the panels: SYSTEM, AGENTS, HEAVY WORKLOADS, details,
// MEMORY BY OWNER and EVENTS. It drops the borders when the bordered
// minimum (SYSTEM with the pressure and scheduler, and a row of each table)
// does not fit.
func (m Model) columnLayout(l *layout) {
	sched := len(m.schedulerLines(innerWidth(m.width)))
	need := 2 + 1 + sched + 8
	if m.drill && !m.help {
		need = 2 + 1 + sched + 4 + 4
	}
	l.bare = l.avail < need
	cw := m.width
	if !l.bare {
		cw = innerWidth(m.width)
	}
	if m.help {
		l.sys = m.sysLines(cw, l.avail-l.boxOverhead(false)-6)
		return
	}
	if m.drill {
		// The drilldown replaces the main panels; its table takes the rows
		// the agent summary leaves. On a short terminal the summary gives up
		// lines so at least one workload row stays visible.
		l.sys = m.sysLines(cw, l.avail-l.boxOverhead(false)-l.boxOverhead(true)-1-l.tableOverhead()-1)
		l.drillInfo = m.drillInfoLines(cw)
		left := l.avail - l.boxOverhead(false) - len(l.sys) - l.boxOverhead(true)
		if room := max(1, left-l.tableOverhead()-1); len(l.drillInfo) > room {
			l.drillInfo = l.drillInfo[:room]
		}
		l.drillRows = max(1, left-len(l.drillInfo)-l.tableOverhead())
	}
	var details int
	if m.details && !m.drill {
		_, body := m.detailsLines(cw)
		details = l.boxOverhead(true) + len(body)
	}
	l.detailsH = details
	tables := 2 * l.tableOverhead()
	// The SYSTEM details give way before the tables shrink below a few rows.
	reserve := min(m.agentsWant(), 3) + min(m.heavyWant(), 2)
	l.sys = m.sysLines(cw, l.avail-l.boxOverhead(false)-details-tables-reserve)
	rest := l.avail - l.boxOverhead(false) - len(l.sys) - details - tables
	eventsOv := l.boxOverhead(true)
	if rest >= 5 {
		l.timelineRows = max(1, (rest-1)/4)
		rest -= l.timelineRows + eventsOv
	}
	if details > 0 && rest < 2 {
		rows := max(1, rest+l.tableOverhead())
		if m.focus == paneAgents {
			l.agentRows = rows
		} else {
			l.heavyRows = rows
		}
		return
	}
	agentsWant, heavyWant := m.agentsWant(), max(1, len(m.status.Heavy))
	l.agentRows = max(1, min(agentsWant, rest/2))
	l.heavyRows = max(1, min(heavyWant, rest-l.agentRows))
	if spare := rest - l.agentRows - l.heavyRows; spare > 0 {
		grow := min(spare, agentsWant-l.agentRows)
		l.agentRows += grow
		spare -= grow
		gaugeOv := l.boxOverhead(true)
		if spare >= gaugeOv+3 {
			l.gaugeH = min(spare, gaugeOv+1+min(len(m.owners()), 5)+1)
			spare -= l.gaugeH
		}
		if l.timelineRows > 0 {
			l.timelineRows += spare
		}
	}
}

// gridLines renders the dashboard grid below the top lines.
func (m Model) gridLines(l layout) []string {
	w := m.width
	out := m.statRow(w, l.statH)
	rest := l.avail - l.statH
	switch {
	case m.help:
		return append(out, m.draw(panel{title: "HELP", body: m.helpLines(innerWidth(w))}, w, rest, false)...)
	case m.drill:
		out = append(out, m.draw(panel{title: m.drillTitle(), body: l.drillInfo}, w, 2+len(l.drillInfo), false)...)
		return append(out, m.draw(m.drillPane(l.drillRows, innerWidth(w)), w, l.drillRows+3, false)...)
	}
	if l.chartH > 0 {
		out = append(out, m.chartRow(w, l.chartH)...)
	}
	var col []string
	if l.sideBySide {
		aw := splitWidths(l.tableW, gridGap, []float64{0.42, 0.58})
		col = hjoin(gridGap,
			m.draw(m.agentsPane(l.agentRows, innerWidth(aw[0])), aw[0], l.agentRows+3, false),
			m.draw(m.heavyPane(l.heavyRows, innerWidth(aw[1])), aw[1], l.heavyRows+3, false))
	}
	if l.agentRows > 0 && !l.sideBySide {
		col = append(col, m.draw(m.agentsPane(l.agentRows, innerWidth(l.tableW)), l.tableW, l.agentRows+3, false)...)
	}
	if l.heavyRows > 0 && !l.sideBySide {
		col = append(col, m.draw(m.heavyPane(l.heavyRows, innerWidth(l.tableW)), l.tableW, l.heavyRows+3, false)...)
	}
	if l.detailsH > 0 {
		title, body := m.detailsLines(innerWidth(l.tableW))
		col = append(col, m.draw(panel{title: title, body: body}, l.tableW, l.detailsH, false)...)
	}
	if l.tableW < w {
		gw := w - l.tableW - gridGap
		gauge := m.draw(panel{title: "MEMORY BY OWNER", body: m.gaugeBody(innerWidth(gw), len(col)-2)}, gw, len(col), false)
		col = hjoin(gridGap, gauge, col)
	}
	out = append(out, col...)
	if l.gaugeH > 0 {
		out = append(out, m.draw(panel{title: "MEMORY BY OWNER", body: m.gaugeBody(innerWidth(w), l.gaugeH-2)}, w, l.gaugeH, false)...)
	}
	if l.eventsH > 0 {
		out = append(out, m.draw(m.timeline(l.eventsH-2, innerWidth(w)), w, l.eventsH, false)...)
	}
	return out
}

// columnLines renders the one-column layout below the top lines.
func (m Model) columnLines(l layout) []string {
	w := m.width
	cw := w
	if !l.bare {
		cw = innerWidth(w)
	}
	sysH := len(l.sys) + l.boxOverhead(false)
	out := m.draw(panel{title: "SYSTEM", body: l.sys}, w, sysH, l.bare)
	if l.bare {
		// Bare, the SYSTEM panel needs no title: its first line says it.
		out = m.draw(panel{body: l.sys}, w, len(l.sys), true)
	}
	tableH := func(rows int) int { return rows + l.tableOverhead() }
	switch {
	case m.help:
		return append(out, m.draw(panel{title: "HELP", body: m.helpLines(cw)}, w, l.avail-sysH, l.bare)...)
	case m.drill:
		out = append(out, m.draw(panel{title: m.drillTitle(), body: l.drillInfo}, w, len(l.drillInfo)+l.boxOverhead(true), l.bare)...)
		return append(out, m.draw(m.drillPane(l.drillRows, cw), w, tableH(l.drillRows), l.bare)...)
	}
	if l.agentRows > 0 {
		out = append(out, m.draw(m.agentsPane(l.agentRows, cw), w, tableH(l.agentRows), l.bare)...)
	}
	if l.heavyRows > 0 {
		out = append(out, m.draw(m.heavyPane(l.heavyRows, cw), w, tableH(l.heavyRows), l.bare)...)
	}
	if l.detailsH > 0 {
		title, body := m.detailsLines(cw)
		out = append(out, m.draw(panel{title: title, body: body}, w, l.detailsH, l.bare)...)
	}
	if l.gaugeH > 0 {
		ov := l.boxOverhead(true)
		out = append(out, m.draw(panel{title: "MEMORY BY OWNER", body: m.gaugeBody(cw, l.gaugeH-ov)}, w, l.gaugeH, l.bare)...)
	}
	if l.timelineRows > 0 {
		out = append(out, m.draw(m.timeline(l.timelineRows, cw), w, l.timelineRows+l.boxOverhead(true), l.bare)...)
	}
	return out
}

// topLines renders the connection banner and the alerts: stopped processes
// throtd did not pause and degraded samplers.
func (m Model) topLines() []string {
	w := m.width
	var lines []string
	switch {
	case !m.polled:
		lines = append(lines, truncate("connecting to throtd at "+m.socket+"…", w))
	case !m.connected:
		msg := fmt.Sprintf("cannot reach throtd at %s: start it with `throtd`", m.socket)
		detail := fmt.Sprintf("retrying every %s; last error: %v", pollInterval, m.err)
		if nr := (*transport.NotRunningError)(nil); errors.As(m.err, &nr) {
			// Its error already says to start throtd; say it once.
			msg = "throtd is not running — start it with `throtd`"
			detail = fmt.Sprintf("retrying every %s (socket %s)", pollInterval, nr.Socket)
		}
		lines = append(lines, splitLines(m.st.banner.Render(lipgloss.Wrap(msg, w, "/")))...)
		lines = append(lines, truncate(detail, w))
	}
	if n := m.status.OrphanCount; m.connected && n > 0 {
		noun := "processes"
		if n == 1 {
			noun = "process"
		}
		lines = append(lines, truncate(m.st.degraded.Render(
			fmt.Sprintf("STOPPED  %d %s not paused by throtd (see throt doctor)", n, noun)), w))
	}
	if m.connected && m.status.Degraded != "" {
		lines = append(lines, splitLines(m.st.degraded.Render(lipgloss.Wrap("DEGRADED  "+m.status.Degraded, w, "")))...)
	}
	return lines
}

// sysLines is the one-column SYSTEM panel's body, cw wide, in at most room
// lines: the pressure line, the why line, the CPU, MEM and SWAP bars and the
// scheduler. When room is short the details give way, SWAP first, then the
// why line, MEM and CPU; the pressure and scheduler always stay.
func (m Model) sysLines(cw, room int) []string {
	pressure := "n/a"
	if m.connected {
		p := m.status.Pressure
		name := strings.ToUpper(p.String())
		if int(p) >= 0 && int(p) < len(m.st.pressure) {
			name = m.st.pressure[p].Render(name)
		}
		pressure = name
		if h := m.status.Host; h != nil && h.Has(domain.MetricLatency) {
			pressure += "  latency " + h.Latency.Round(time.Millisecond).String()
		}
		if h := m.status.Host; h != nil {
			pressure += psiSummary(*h)
		}
	}
	head := "pressure " + pressure
	// The latency sparkline ends the line and only takes the room left.
	if h := m.status.Host; m.connected && h != nil && h.Has(domain.MetricLatency) {
		if sw := min(sparkWidth(cw), cw-lipgloss.Width(head)-1); sw >= minSparkWidth/2 {
			vals, ok := m.hist.series(sw, latencyOf)
			head += " " + strings.TrimLeft(sparkline(vals, ok, 0, latencyScaleFloor, sw), " ")
		}
	}
	head = truncate(head, cw)
	details := [4]string{}
	if m.connected && m.status.Reason != "" {
		details[0] = m.reasonLine(cw)
	}
	copy(details[1:], m.metricLines(cw))
	tail := m.schedulerLines(cw)
	shown := func() int {
		n := 0
		for _, d := range details {
			if d != "" {
				n++
			}
		}
		return n
	}
	for _, i := range [...]int{3, 0, 2, 1} {
		if 1+shown()+len(tail) <= room {
			break
		}
		details[i] = ""
	}
	lines := []string{head}
	for _, d := range details {
		if d != "" {
			lines = append(lines, d)
		}
	}
	return append(lines, tail...)
}

// schedulerLines summarizes the scheduler in the words of `throt status`,
// or returns nothing when it is off. A narrow panel wraps it between items
// rather than cutting any.
func (m Model) schedulerLines(w int) []string {
	sc := m.status.Scheduler
	if !m.connected || sc.Mode == "" || sc.Mode == "off" {
		return nil
	}
	items := append([]string{string(sc.Mode)}, m.schedulerItems()...)
	const label = "SCHEDULER  "
	// Continuations line up under the first item when the longest fits.
	indent := len(label)
	for _, it := range items {
		if indent+lipgloss.Width(it)+1 > w {
			indent = 2
		}
	}
	var lines []string
	line := m.st.title.Render(strings.TrimRight(label, " ")) + "  "
	for i, it := range items {
		if i < len(items)-1 {
			it += ","
		}
		switch {
		case i == 0:
			line += it
		case lipgloss.Width(line)+1+lipgloss.Width(it) <= w:
			line += " " + it
		default:
			lines = append(lines, truncate(line, w))
			line = strings.Repeat(" ", indent) + it
		}
	}
	return append(lines, truncate(line, w))
}

// psiSummary renders the Linux PSI "some" shares the sample carries as
// "  PSI mem 3% io 0% cpu 12%", or "" when it has none.
func psiSummary(h domain.HostSample) string {
	s := ""
	for _, r := range [...]struct {
		m     domain.Metric
		label string
	}{{domain.MetricMemoryStall, "mem"}, {domain.MetricIOStall, "io"}, {domain.MetricCPUStall, "cpu"}} {
		if st, ok := h.StallFor(r.m); ok {
			s += fmt.Sprintf(" %s %.0f%%", r.label, st.Some*100)
		}
	}
	if s == "" {
		return ""
	}
	return "  PSI" + s
}

// reasonLine explains which signal drives pressure. A known code is
// followed by the raw code when it fits; an unknown code is shown raw.
func (m Model) reasonLine(w int) string {
	text, ok := explainReason(m.status.Reason)
	if p := m.status.Pressure; p >= domain.PressureElevated && int(p) < len(m.st.pressure) {
		// The cause is as prominent as the level it explains.
		text = m.st.pressure[p].Render(text)
	}
	line := "why: " + text
	if raw := "  (" + m.status.Reason + ")"; ok && lipgloss.Width(line+raw) <= w {
		line += m.st.header.Render(raw)
	}
	return truncate(line, w)
}

// sparkWidth is the width of every SYSTEM panel sparkline at width w, or 0
// when they are dropped.
func sparkWidth(w int) int {
	sw := min(maxSparkWidth, w-barOverhead-barDetailWidth-minBarWidth-2)
	if sw < minSparkWidth {
		return 0
	}
	return sw
}

// spark renders one signal's history at sparkWidth(w), or "" when
// sparklines are dropped. scale and floor are as for sparkline.
func (m Model) spark(w int, get func(histSample) (float64, bool), scale, floor float64) string {
	sw := sparkWidth(w)
	if sw == 0 {
		return ""
	}
	vals, ok := m.hist.series(sw, get)
	return sparkline(vals, ok, scale, floor, sw)
}

// metricLines renders CPU, MEM, and SWAP bars w wide. Metrics the host did
// not report, or all of them before the first sample, show as n/a. The
// kernel memory-pressure verdict, when present, follows the MEM detail.
func (m Model) metricLines(w int) []string {
	h := m.status.Host
	has := func(x domain.Metric) bool { return m.connected && h != nil && h.Has(x) }

	var cpu, mem, swap string
	var cpuFrac, memFrac, swapFrac float64
	load := "load n/a"
	if has(domain.MetricLoad) {
		load = fmt.Sprintf("load %.2f/cpu", h.Load1)
	}
	if has(domain.MetricCPU) {
		cpuFrac, cpu = h.CPUUsage, load
	}
	if has(domain.MetricMemory) {
		used := h.MemTotal - min(h.MemAvailable, h.MemTotal)
		memFrac, mem = ratio(used, h.MemTotal), fmt.Sprintf("%s / %s", formatBytes(used), formatBytes(h.MemTotal))
	}
	kernel := ""
	if m.connected && h != nil && h.HasMemoryPressure() {
		kernel = "kernel: " + h.MemoryPressure.String()
		mem += "  " + kernel
	}
	if has(domain.MetricSwap) {
		swapFrac, swap = ratio(h.SwapUsed, h.SwapTotal), fmt.Sprintf("%s / %s", formatBytes(h.SwapUsed), formatBytes(h.SwapTotal))
	}
	// All bars share one width so the percentages line up. A long detail
	// (the kernel verdict) narrows them, but not below minBarWidth.
	sparkCols := 0
	if sw := sparkWidth(w); sw > 0 {
		sparkCols = sw + 2
	}
	room := func(detail int) int { return min(maxBarWidth, w-barOverhead-detail-sparkCols) }
	width := max(min(minBarWidth, max(5, room(barDetailWidth))),
		room(max(barDetailWidth, lipgloss.Width(cpu), lipgloss.Width(mem), lipgloss.Width(swap))))

	cpuLine := m.bar(w, "CPU", width, has(domain.MetricCPU), cpuFrac, m.spark(w, cpuOf, 1, 0), cpu)
	if !has(domain.MetricCPU) && has(domain.MetricLoad) {
		cpuLine = truncate("CPU   n/a  "+load, w)
	}
	memLine := m.bar(w, "MEM", width, has(domain.MetricMemory), memFrac, m.spark(w, memOf, 1, 0), mem)
	if !has(domain.MetricMemory) && kernel != "" {
		memLine = truncate("MEM   n/a  "+kernel, w)
	}
	return []string{
		cpuLine,
		memLine,
		// The SWAP bar is swap used; its sparkline is swap growth.
		m.bar(w, "SWAP", width, has(domain.MetricSwap), swapFrac, m.spark(w, swapOf, 0, swapScaleFloor), swap),
	}
}

// bar renders "LABEL ████░░░░  42%  ▁▂▃▅  detail" with a bar width cells
// wide, or "LABEL n/a" when !ok. spark is "" when sparklines are dropped.
func (m Model) bar(w int, label string, width int, ok bool, frac float64, spark, detail string) string {
	if !ok {
		return fmt.Sprintf("%-5s n/a", label)
	}
	frac = max(0, min(frac, 1))
	if spark != "" {
		spark += "  "
	}
	fill := int(frac*float64(width) + 0.5)
	return truncate(fmt.Sprintf("%-5s %s%s %4.0f%%  %s%s", label,
		m.level(frac).UnsetBold().Render(strings.Repeat("█", fill)), m.st.track.Render(strings.Repeat("░", width-fill)), frac*100, spark, detail), w)
}

func (m Model) agentsPane(rows, w int) panel {
	cols := []column{
		{title: "AGENT", min: 8, sort: sortName},
		{title: "PID", width: 7, right: true, drop: 2},
		{title: "CPU", width: 6, right: true, sort: sortCPU},
		{title: "MEM", width: 9, right: true, sort: sortMem},
		{title: "CHILDREN", width: 8, right: true, drop: 1},
	}
	cells := make([][]string, len(m.agents))
	var cpu float64
	var rss uint64
	for i, a := range m.agents {
		cells[i] = []string{a.Agent, fmt.Sprint(a.Key.PID), cores(a.CPU), formatBytes(a.RSS), fmt.Sprint(a.Children)}
		cpu, rss = cpu+a.CPU, rss+a.RSS
	}
	note, more := "", ""
	if len(m.agents) > 1 {
		note = fmt.Sprintf("  total %s CPU, %s", cores(cpu), formatBytes(rss))
	}
	if m.agentsCollapsed(rows) {
		more = moreAgents(m.agents[rows-1:])
	}
	return m.table(paneAgents, "AGENTS", note, "(no agents detected)", cols, cells, rows, more, w)
}

// moreAgents summarizes the agents a collapsed table leaves out.
func moreAgents(rest []process.AgentView) string {
	var maxCPU, sumCPU float64
	var maxRSS, sumRSS uint64
	for _, a := range rest {
		maxCPU, maxRSS = max(maxCPU, a.CPU), max(maxRSS, a.RSS)
		sumCPU, sumRSS = sumCPU+a.CPU, sumRSS+a.RSS
	}
	return fmt.Sprintf("+%d more (each ≤%.0f%% CPU, ≤%s; %s CPU, %s together)",
		len(rest), math.Ceil(maxCPU*100), formatBytes(maxRSS), cores(sumCPU), formatBytes(sumRSS))
}

func (m Model) heavyPane(rows, w int) panel {
	cols := []column{
		{title: "OWNER", min: 6},
		{title: "PID", width: 7, right: true, drop: 3},
		{title: "PROCESS", min: 8, sort: sortName},
		{title: "CLASS", width: 15, drop: 2},
		{title: "CPU", width: 6, right: true, sort: sortCPU},
		{title: "MEM", width: 9, right: true, sort: sortMem},
		{title: "STATE", width: 6, drop: 1},
	}
	state := m.stateOf()
	cells := make([][]string, len(m.heavy))
	for i, h := range m.heavy {
		cells[i] = []string{owner(h.Owner), fmt.Sprint(h.Key.PID), h.Name, string(h.Class), cores(h.CPU), formatBytes(h.RSS), state(h)}
		cols[6].width = max(cols[6].width, lipgloss.Width(cells[i][6]))
	}
	return m.table(paneHeavy, "HEAVY WORKLOADS", "", "(no heavy workloads)", cols, cells, rows, "", w)
}

// stateOf returns a lookup of each workload's scheduling state: "QUEUED",
// "PAUSED", "running (would queue)" for work observe mode would have
// queued, "bg" while it runs at background priority, or "" while it runs.
func (m Model) stateOf() func(process.WorkloadView) string {
	paused := make(map[domain.ProcessKey]bool, len(m.status.Paused))
	for _, p := range m.status.Paused {
		paused[p.Key] = true
	}
	queued := make(map[domain.ProcessKey]bool, len(m.status.Scheduler.Queued))
	for _, k := range m.status.Scheduler.Queued {
		queued[k] = true
	}
	would := make(map[domain.ProcessKey]bool, len(m.status.Scheduler.WouldQueue))
	for _, k := range m.status.Scheduler.WouldQueue {
		would[k] = true
	}
	return func(h process.WorkloadView) string {
		switch k := h.Key; {
		case queued[k]:
			return "QUEUED"
		case paused[k]:
			return "PAUSED"
		case would[k]:
			return "running (would queue)"
		case h.Idle:
			return "idle"
		case h.Background:
			return "bg"
		}
		return ""
	}
}

func (m Model) drillTitle() string { return "AGENT " + m.drillLabel + "  (esc back)" }

// drillInfoLines summarizes the drilled-into agent, w wide.
func (m Model) drillInfoLines(w int) []string {
	a, ok := m.drillAgent()
	if !ok {
		return []string{truncate(fmt.Sprintf("pid %d is no longer reported by throtd", m.drillKey.PID), w)}
	}
	return fieldLines(m.st, agentFields(a), w)
}

// drillPane lists the heavy workloads the drilled-into agent owns.
func (m Model) drillPane(rows, w int) panel {
	cols := []column{
		{title: "PID", width: 7, right: true, drop: 1},
		{title: "PROCESS", min: 8, sort: sortName},
		{title: "CLASS", width: 15, drop: 2},
		{title: "CPU", width: 6, right: true, sort: sortCPU},
		{title: "MEM", width: 9, right: true, sort: sortMem},
		{title: "STATE", width: 7},
	}
	state := m.stateOf()
	hs := m.workloads(paneDrill)
	cells := make([][]string, len(hs))
	for i, h := range hs {
		st := state(h)
		if st == "" {
			st = "running"
		}
		cells[i] = []string{fmt.Sprint(h.Key.PID), h.Name, string(h.Class), cores(h.CPU), formatBytes(h.RSS), st}
		cols[5].width = max(cols[5].width, lipgloss.Width(st))
	}
	return m.table(paneDrill, "HEAVY WORKLOADS", "", "(no heavy workloads)", cols, cells, rows, "", w)
}

// table renders a titled, scrollable table panel: a column header and
// exactly rows rows, w wide. note follows the title. A non-empty more
// replaces the last row: the table then shows its first rows-1 rows and
// more summarizes the rest.
func (m Model) table(p pane, title, note, empty string, cols []column, cells [][]string, rows int, more string, w int) panel {
	focused := m.active() == p && !m.help
	head := title
	visible := rows
	if more != "" {
		visible = rows - 1
	}
	if n := len(cells); n > rows && more == "" {
		head += fmt.Sprintf(" %d-%d of %d", m.off[p]+1, min(m.off[p]+rows, n), n)
	} else if n > 0 {
		head += fmt.Sprintf(" (%d)", n)
	}
	head += note
	// Columns are as wide as their widest cell or title (with room for a
	// sort indicator), within their limits.
	want := make([]int, len(cols))
	for i, c := range cols {
		want[i] = lipgloss.Width(c.title)
		if c.sort != sortAuto {
			want[i]++
		}
		for _, r := range cells {
			want[i] = max(want[i], lipgloss.Width(r[i]))
		}
	}
	widths := fitColumns(cols, want, w-markerWidth)
	sortBy := m.effectiveSort(m.sortBy[m.sortPane(p)])
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.title
		if sortBy != sortDaemon && c.sort == sortBy {
			// Name sorts ascending, CPU and memory descending.
			if sortBy == sortName {
				header[i] += "▲"
			} else {
				header[i] += "▼"
			}
		}
	}
	lines := []string{m.st.header.Render(truncate(strings.Repeat(" ", markerWidth)+formatRow(cols, widths, header), w))}
	if len(cells) == 0 {
		msg := empty
		if !m.connected {
			msg = "(no data)"
		}
		lines = append(lines, truncate(strings.Repeat(" ", markerWidth)+msg, w))
	}
	for i := m.off[p]; i < len(cells) && i < m.off[p]+visible; i++ {
		row := truncate(formatRow(cols, widths, cells[i]), w-markerWidth)
		if focused && i == m.sel[p] {
			lines = append(lines, m.st.selected.Render("> "+row))
		} else {
			lines = append(lines, "  "+row)
		}
	}
	if more != "" {
		lines = append(lines, m.st.header.Render(truncate(strings.Repeat(" ", markerWidth)+more, w)))
	}
	return panel{title: head, body: lines, focused: focused}
}

// detailsLines describes the selected row, w wide, using only fields the
// daemon already reports.
func (m Model) detailsLines(w int) (string, []string) {
	var title string
	var fields [][2]string
	switch {
	case m.focus == paneAgents && len(m.agents) > 0:
		a := m.agents[m.sel[paneAgents]]
		title = "DETAILS  agent " + a.Agent
		fields = agentFields(a)
	case m.focus == paneHeavy && len(m.heavy) > 0:
		h := m.heavy[m.sel[paneHeavy]]
		title = "DETAILS  workload " + h.Name
		ownerPID := "-"
		if h.OwnerPID != 0 {
			ownerPID = fmt.Sprint(h.OwnerPID)
		}
		fields = [][2]string{
			{"pid", fmt.Sprint(h.Key.PID)}, {"started", startTime(h.Key)}, {"class", string(h.Class)},
			{"owner", owner(h.Owner)}, {"owner pid", ownerPID}, {"cpu", cores(h.CPU)}, {"mem", formatBytes(h.RSS)},
		}
	default:
		return "DETAILS", []string{"nothing selected"}
	}
	return title, fieldLines(m.st, fields, w)
}

func agentFields(a process.AgentView) [][2]string {
	return [][2]string{
		{"executable", a.Name}, {"pid", fmt.Sprint(a.Key.PID)}, {"started", startTime(a.Key)},
		{"cpu", cores(a.CPU)}, {"mem", formatBytes(a.RSS)}, {"children", fmt.Sprint(a.Children)},
	}
}

// fieldLines renders "name value" fields wrapped to w.
func fieldLines(st styles, fields [][2]string, w int) []string {
	var lines []string
	line := ""
	for _, f := range fields {
		item := st.header.Render(f[0]) + " " + f[1]
		if line != "" && lipgloss.Width(line)+2+lipgloss.Width(item) > w {
			lines = append(lines, truncate(line, w))
			line = ""
		}
		if line != "" {
			line += "  "
		}
		line += item
	}
	return append(lines, truncate(line, w))
}

// timeline renders the EVENTS panel: the newest events first, one per line,
// rows lines w wide. Messages are whole sentences, so the kind is shown
// only for an event without one. Control, scheduler and failure events
// carry a tag (CTL, SCHED, FAIL) as well as a color. With a filter only
// matching events are listed.
func (m Model) timeline(rows, w int) panel {
	events := m.shownEvents()
	title := "EVENTS"
	if m.filter != "" || m.filtering {
		cursor := ""
		if m.filtering {
			cursor = "_"
		}
		title += fmt.Sprintf("  filter: %s%s (%d of %d)", m.filter, cursor, len(events), len(m.events))
	}
	if m.eventsErr != nil {
		title += "  (events unavailable: " + m.eventsErr.Error() + ")"
	}
	var lines []string
	// time and tag columns with their gaps
	msgWidth := w - (8 + 2 + eventTagWidth + 2)
	for _, it := range coalesce(events) {
		if len(lines) >= rows {
			break
		}
		e := it.e
		st := m.st.event[classify(e.Kind)]
		line := fmt.Sprintf("%s  %s  %s", m.st.header.Render(e.Time.Local().Format("15:04:05")),
			st.Render(eventTags[classify(e.Kind)]), truncate(it.text(), max(0, msgWidth)))
		lines = append(lines, truncate(strings.TrimRight(line, " "), w))
	}
	switch {
	case len(m.events) == 0:
		lines = append(lines, "(no events)")
	case len(events) == 0:
		lines = append(lines, truncate("(no event matches the filter; esc clears it)", w))
	}
	return panel{title: title, body: lines}
}

// burstText words a burst of same-kind lifecycle events, given their count.
// Only these kinds are coalesced: control and scheduler events always keep
// a line each.
var burstText = map[domain.EventKind]string{
	domain.EventAgentDiscovered:   "%d agents started",
	domain.EventAgentLost:         "%d agents exited",
	domain.EventProcessDiscovered: "discovered %d heavy workloads",
	domain.EventProcessLost:       "%d heavy workloads exited",
}

// timelineItem is one timeline line: an event, or a burst of n events of
// one kind in the same second, e the newest; names lists them newest first.
type timelineItem struct {
	e     domain.Event
	n     int
	names []string
}

func (it timelineItem) text() string {
	if it.n > 1 {
		names := slices.Clone(it.names)
		slices.Reverse(names)
		return fmt.Sprintf(burstText[it.e.Kind], it.n) + ": " + strings.Join(names, ", ")
	}
	if it.e.Message == "" {
		return string(it.e.Kind)
	}
	return it.e.Message
}

// coalesce returns the timeline lines for events (oldest first), newest
// first, merging adjacent lifecycle events of one kind in the same second.
// It changes only the presentation: the filter still counts events.
func coalesce(events []domain.Event) []timelineItem {
	var out []timelineItem
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		name := e.Attrs["name"]
		if a := e.Attrs["agent"]; a != "" && (e.Kind == domain.EventAgentDiscovered || e.Kind == domain.EventAgentLost) {
			name = a
		}
		if _, ok := burstText[e.Kind]; ok && len(out) > 0 {
			last := &out[len(out)-1]
			if last.e.Kind == e.Kind && last.e.Time.Unix() == e.Time.Unix() {
				last.n++
				last.names = append(last.names, name)
				continue
			}
		}
		out = append(out, timelineItem{e: e, n: 1, names: []string{name}})
	}
	return out
}

func (m Model) helpLines(w int) []string {
	lines := []string{
		"q, ctrl+c   quit (throtd keeps running)",
		"tab         switch focus: agents / heavy workloads (shift+tab too)",
		"j/k, ↑/↓    move the selection",
		"enter       agents: open the agent drilldown; heavy: details",
		"esc         leave the drilldown; close details and help;",
		"            else clear the events filter",
		"/           filter the events by kind or text (enter keeps, esc clears)",
		"p / r       pause / resume the selected heavy workload (asks y/n)",
		"s           sort the table: auto, cpu, mem, name, daemon order",
		"?           toggle this help",
		"",
		"Auto sorts by memory while pressure is from memory, else by CPU.",
		"Agents beyond the first rows fold into a +N more row; j scrolls on.",
		"Sparklines and charts: the latest polls (one a second) of CPU,",
		"memory used, swap (growth under SWAP), latency and pressure.",
		"MEMORY BY OWNER: ● agents, ○ processes outside every agent.",
		"The TUI makes no decisions: p and r send only what you confirm,",
		"and throtd checks ownership before acting.",
	}
	for i, l := range lines {
		lines[i] = truncate(l, w)
	}
	return lines
}

// footer shows a pending confirmation, else a recent control result, else
// the key hints.
func (m Model) footer() string {
	if a := m.confirm; a != nil {
		return m.st.title.Render(truncate(fmt.Sprintf("%s %s (pid %d)? y/n", a.verb, a.name, a.key.PID), m.width))
	}
	if m.notice != "" {
		return truncate(m.notice, m.width)
	}
	if m.filtering {
		return m.st.footer.Render(truncate("filter: type text  backspace delete  enter keep  esc clear", m.width))
	}
	// The footer always ends with ? help and q quit, which fit even at the
	// minimum width; the other hints give way first, whole.
	var hints string
	switch {
	case m.help:
		hints = "esc close"
	case m.drill:
		hints = "esc back  j/k move  p/r pause/resume  s sort"
	case m.focus == paneHeavy:
		hints = "tab focus  j/k move  p/r pause/resume  enter details  s sort  / filter"
	default:
		hints = "tab focus  j/k move  enter open  s sort  / filter"
	}
	const always = "? help  q quit"
	if w := m.width - len(always) - 2; w > 0 {
		if hints = truncateWords(hints, w); hints != "" {
			return m.st.footer.Render(hints + "  " + always)
		}
	}
	return m.st.footer.Render(truncate(always, m.width))
}

// truncateWords cuts s at the last double-space gap that leaves it at most
// width cells wide, so a hint is dropped whole rather than cut.
func truncateWords(s string, width int) string {
	for lipgloss.Width(s) > width {
		i := strings.LastIndex(s, "  ")
		if i < 0 {
			return ""
		}
		s = s[:i]
	}
	return s
}

// column describes one table column. A column with width 0 is flexible:
// at least min wide, and growing towards its content while space is left.
// A fixed column is at most width wide. Columns with a higher drop value
// are removed first when the table does not fit.
type column struct {
	title string
	width int
	min   int
	right bool
	drop  int
	// sort is the sort key this column shows, if any.
	sort sortKey
}

const (
	columnGap    = 2
	maxFlexWidth = 32
)

// fitColumns returns each column's width for a table avail cells wide,
// given the width each column's content wants; a dropped column gets
// width 0. No column is wider than its content.
func fitColumns(cols []column, want []int, avail int) []int {
	keep := make([]bool, len(cols))
	for i := range keep {
		keep[i] = true
	}
	for {
		need, worst := 0, -1
		for i, c := range cols {
			if !keep[i] {
				continue
			}
			if need > 0 {
				need += columnGap
			}
			if c.width == 0 {
				need += min(c.min, want[i])
			} else {
				need += min(c.width, want[i])
			}
			if c.drop > 0 && (worst < 0 || c.drop > cols[worst].drop) {
				worst = i
			}
		}
		if need <= avail || worst < 0 {
			widths := make([]int, len(cols))
			spare := max(0, avail-need)
			for i, c := range cols {
				switch {
				case !keep[i]:
				case c.width == 0:
					// Flexible columns grow in order, each to its content.
					lo := min(c.min, want[i])
					widths[i] = lo + min(spare, max(0, min(maxFlexWidth, want[i])-lo))
					spare -= widths[i] - lo
				default:
					widths[i] = min(c.width, want[i])
				}
			}
			return widths
		}
		keep[worst] = false
	}
}

func formatRow(cols []column, widths []int, cells []string) string {
	var b strings.Builder
	for i, c := range cols {
		if widths[i] == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(strings.Repeat(" ", columnGap))
		}
		b.WriteString(pad(truncate(cells[i], widths[i]), widths[i], c.right))
	}
	return strings.TrimRight(b.String(), " ")
}

// truncate shortens s to at most width display cells, ending with "…" when
// it cuts. It counts cells per grapheme cluster, so wide characters and
// emoji are measured as the terminal draws them and never split. It is
// ANSI-aware only in measuring: a styled string that does not fit loses its
// styling.
func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return strings.Repeat("…", max(0, width))
	}
	return truncStyle.MaxWidth(width-1).Render(stripStyles(s)) + "…"
}

// truncStyle only cuts; it adds no styling.
var truncStyle = lipgloss.NewStyle()

// stripStyles removes ANSI escape sequences (CSI ... final byte).
func stripStyles(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func pad(s string, width int, right bool) string {
	gap := strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
	if right {
		return gap + s
	}
	return s + gap
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

func ratio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// cores renders CPU cores as a percentage of one core, like `throt status`.
func cores(c float64) string { return fmt.Sprintf("%.0f%%", c*100) }

func formatBytes(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

func owner(o string) string {
	if o == "" {
		return "-"
	}
	return o
}

func startTime(k domain.ProcessKey) string {
	if k.Start.IsZero() {
		return "-"
	}
	return k.Start.Local().Format("2006-01-02 15:04:05")
}
