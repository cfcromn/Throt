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
	// Sparklines get up to maxSparkWidth cells and are dropped, before
	// anything else, when fewer than minSparkWidth fit beside a bar of
	// minBarWidth.
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
	switch {
	case m.help:
		lines = append(lines, m.helpLines()...)
	case m.drill:
		lines = append(lines, l.drillInfo...)
		lines = append(lines, m.drillPane(l.drillRows)...)
	default:
		if l.agentRows > 0 {
			lines = append(lines, m.agentsPane(l.agentRows)...)
		}
		if l.heavyRows > 0 {
			lines = append(lines, m.heavyPane(l.heavyRows)...)
		}
		lines = append(lines, l.details...)
		if l.timelineRows > 0 {
			lines = append(lines, m.timeline(l.timelineRows)...)
		}
	}
	// Fill so the footer sits on the last row, and never exceed the height.
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:m.height-1], m.footer())
	return strings.Join(lines, "\n")
}

// maxAgentRows is the most rows the agents table gets.
const maxAgentRows = 10

type layout struct {
	top, details, drillInfo                       []string
	agentRows, heavyRows, timelineRows, drillRows int
}

// layout splits the window height between panes. Fixed parts (banners,
// system, details, footer) come first; the timeline gets a quarter of what
// remains and the tables share the rest, each at least one row. A table
// with 0 rows is hidden: when the details pane leaves no room for both
// tables, the unfocused one gives way.
func (m Model) layout() layout {
	l := layout{top: m.topLines()}
	if m.width == 0 || m.height == 0 {
		return l
	}
	if m.drill && !m.help {
		// The drilldown replaces the main panes; its table takes all the
		// rows the agent summary leaves. The main panes keep the sizes
		// computed below so their scroll positions survive.
		// On a short terminal the summary gives up lines so at least one
		// workload row stays visible.
		l.drillInfo = m.drillInfoLines()
		if room := max(1, m.height-len(l.top)-2-1-1); len(l.drillInfo) > room {
			l.drillInfo = l.drillInfo[:room]
		}
		l.drillRows = max(1, m.height-len(l.top)-len(l.drillInfo)-2-1)
	}
	if m.details && !m.help && !m.drill {
		l.details = m.detailsLines()
	}
	const paneOverhead = 2 // title and column header of a table
	rest := m.height - len(l.top) - len(l.details) - 2*paneOverhead - 1
	if rest >= 5 {
		l.timelineRows = max(1, (rest-1)/4)
		rest -= l.timelineRows + 1
	}
	// Many agents fold into the top maxAgentRows-1 and a summary row, so
	// the heavy workloads and the timeline keep their room.
	agentsWant, heavyWant := max(1, min(len(m.status.Agents), maxAgentRows)), max(1, len(m.status.Heavy))
	if len(l.details) > 0 && rest < 2 {
		rows := max(1, rest+paneOverhead+1)
		if m.focus == paneAgents {
			l.agentRows = rows
		} else {
			l.heavyRows = rows
		}
		return l
	}
	l.agentRows = max(1, min(agentsWant, rest/2))
	l.heavyRows = max(1, min(heavyWant, rest-l.agentRows))
	if spare := rest - l.agentRows - l.heavyRows; spare > 0 {
		grow := min(spare, agentsWant-l.agentRows)
		l.agentRows += grow
		spare -= grow
		if l.timelineRows > 0 {
			l.timelineRows += spare
		}
	}
	return l
}

// topLines renders the connection banner, the system section, and the
// degraded notice. On a short terminal the system details give way, so a
// table stays visible: the memory outside agents first, then SWAP, the why
// line, MEM and CPU. The
// pressure, the scheduler summary and the warnings always stay.
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
	header := m.st.title.Render("SYSTEM") + "  pressure " + pressure
	// The latency sparkline ends the header and only takes the room left.
	if h := m.status.Host; m.connected && h != nil && h.Has(domain.MetricLatency) {
		if sw := min(m.sparkWidth(), w-lipgloss.Width(header)-1); sw >= minSparkWidth/2 {
			vals, ok := m.hist.series(sw, latencyOf)
			header += " " + strings.TrimLeft(sparkline(vals, ok, 0, latencyScaleFloor, sw), " ")
		}
	}
	lines = append(lines, truncate(header, w))

	tail := m.schedulerLines()
	if n := m.status.OrphanCount; m.connected && n > 0 {
		noun := "processes"
		if n == 1 {
			noun = "process"
		}
		tail = append(tail, truncate(m.st.degraded.Render(
			fmt.Sprintf("STOPPED  %d %s not paused by throtd (see throt doctor)", n, noun)), w))
	}
	if m.connected && m.status.Degraded != "" {
		tail = append(tail, splitLines(m.st.degraded.Render(lipgloss.Wrap("DEGRADED  "+m.status.Degraded, w, "")))...)
	}

	// details are the why line, the CPU, MEM and SWAP bars and the memory
	// outside agents, "" when absent.
	details := [5]string{}
	if m.connected && m.status.Reason != "" {
		details[0] = m.reasonLine()
	}
	copy(details[1:], m.metricLines())
	details[4] = m.outsideLine()
	room := m.height - 1 - minPaneLines - len(lines) - len(tail)
	shown := func() int {
		n := 0
		for _, d := range details {
			if d != "" {
				n++
			}
		}
		return n
	}
	for _, i := range [...]int{4, 3, 0, 2, 1} {
		if shown() <= room {
			break
		}
		details[i] = ""
	}
	for _, d := range details {
		if d != "" {
			lines = append(lines, d)
		}
	}
	return append(lines, tail...)
}

// minPaneLines is what the system section leaves below it on a short
// terminal: one table's title, header and a row.
const minPaneLines = 3

// schedulerLines summarizes the scheduler in the words of `throt status`,
// or returns nothing when it is off: mode, the heavy limit (for an adaptive
// scheduler with its start and ceiling), the per-agent cap, queued work (in
// observe mode, the work it would queue) and reserved leases. A narrow
// window wraps it between items rather than cutting any.
func (m Model) schedulerLines() []string {
	sc := m.status.Scheduler
	if !m.connected || sc.Mode == "" || sc.Mode == "off" {
		return nil
	}
	limit := fmt.Sprintf("limit %d", sc.HeavyLimit)
	if sc.Adaptive {
		limit += fmt.Sprintf(" (adaptive, start %d, max %d)", sc.StartLimit, sc.MaxLimit)
	}
	items := []string{string(sc.Mode), limit}
	if sc.PerAgentLimit > 0 {
		items = append(items, fmt.Sprintf("per-agent %d", sc.PerAgentLimit))
	}
	if sc.Mode == "observe" {
		items = append(items, fmt.Sprintf("%d would queue", len(sc.WouldQueue)))
	} else {
		items = append(items, fmt.Sprintf("%d queued", len(sc.Queued)))
	}
	items = append(items, fmt.Sprintf("%d reserved", len(sc.Leases)))

	const label = "SCHEDULER  "
	// Continuations line up under the first item when the longest fits.
	indent := len(label)
	for _, it := range items {
		if indent+lipgloss.Width(it)+1 > m.width {
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
		case lipgloss.Width(line)+1+lipgloss.Width(it) <= m.width:
			line += " " + it
		default:
			lines = append(lines, truncate(line, m.width))
			line = strings.Repeat(" ", indent) + it
		}
	}
	return append(lines, truncate(line, m.width))
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
func (m Model) reasonLine() string {
	text, ok := explainReason(m.status.Reason)
	if p := m.status.Pressure; p >= domain.PressureElevated && int(p) < len(m.st.pressure) {
		// The cause is as prominent as the level it explains.
		text = m.st.pressure[p].Render(text)
	}
	line := "  why: " + text
	if raw := "  (" + m.status.Reason + ")"; ok && lipgloss.Width(line+raw) <= m.width {
		line += m.st.header.Render(raw)
	}
	return truncate(line, m.width)
}

// sparkWidth is the width of every sparkline at this terminal width, or 0
// when they are dropped.
func (m Model) sparkWidth() int {
	w := min(maxSparkWidth, m.width-barOverhead-barDetailWidth-minBarWidth-2)
	if w < minSparkWidth {
		return 0
	}
	return w
}

// spark renders one signal's history at sparkWidth, or "" when sparklines
// are dropped. scale and floor are as for sparkline.
func (m Model) spark(get func(histSample) (float64, bool), scale, floor float64) string {
	sw := m.sparkWidth()
	if sw == 0 {
		return ""
	}
	vals, ok := m.hist.series(sw, get)
	return sparkline(vals, ok, scale, floor, sw)
}

// metricLines renders CPU, MEM, and SWAP bars. Metrics the host did not
// report, or all of them before the first sample, show as n/a. The kernel
// memory-pressure verdict, when present, follows the MEM detail.
func (m Model) metricLines() []string {
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
	if sw := m.sparkWidth(); sw > 0 {
		sparkCols = sw + 2
	}
	room := func(detail int) int { return min(maxBarWidth, m.width-barOverhead-detail-sparkCols) }
	width := max(min(minBarWidth, max(5, room(barDetailWidth))),
		room(max(barDetailWidth, lipgloss.Width(cpu), lipgloss.Width(mem), lipgloss.Width(swap))))

	cpuLine := m.bar("CPU", width, has(domain.MetricCPU), cpuFrac, m.spark(cpuOf, 1, 0), cpu)
	if !has(domain.MetricCPU) && has(domain.MetricLoad) {
		cpuLine = truncate("CPU   n/a  "+load, m.width)
	}
	memLine := m.bar("MEM", width, has(domain.MetricMemory), memFrac, m.spark(memOf, 1, 0), mem)
	if !has(domain.MetricMemory) && kernel != "" {
		memLine = truncate("MEM   n/a  "+kernel, m.width)
	}
	return []string{
		cpuLine,
		memLine,
		// The SWAP bar is swap used; its sparkline is swap growth.
		m.bar("SWAP", width, has(domain.MetricSwap), swapFrac, m.spark(swapOf, 0, swapScaleFloor), swap),
	}
}

// bar renders "LABEL ████░░░░  42%  ▁▂▃▅  detail" with a bar width cells
// wide, or "LABEL n/a" when !ok. spark is "" when sparklines are dropped.
func (m Model) bar(label string, width int, ok bool, frac float64, spark, detail string) string {
	if !ok {
		return fmt.Sprintf("%-5s n/a", label)
	}
	frac = max(0, min(frac, 1))
	if spark != "" {
		spark += "  "
	}
	fill := int(frac*float64(width) + 0.5)
	return truncate(fmt.Sprintf("%-5s %s%s %4.0f%%  %s%s", label,
		strings.Repeat("█", fill), strings.Repeat("░", width-fill), frac*100, spark, detail), m.width)
}

func (m Model) agentsPane(rows int) []string {
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
	return m.table(paneAgents, "AGENTS", note, "(no agents detected)", cols, cells, rows, more)
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

// outsideLine names where the memory outside every agent goes: the total
// and the process names with the most of it, largest first, as many as fit.
// It is "" when throtd does not report it.
func (m Model) outsideLine() string {
	st := m.status
	if !m.connected || len(st.UnattributedMemory) == 0 {
		return ""
	}
	line := m.st.title.Render("OUTSIDE AGENTS") + "  " + formatBytes(st.UnattributedRSS)
	sep := ": "
	for _, u := range st.UnattributedMemory {
		item := truncate(u.Name, maxOutsideName)
		if u.Processes > 1 {
			item += fmt.Sprintf(" ×%d", u.Processes)
		}
		item += " " + formatBytes(u.RSS)
		if lipgloss.Width(line)+len(sep)+lipgloss.Width(item) > m.width {
			break
		}
		line += sep + item
		sep = ", "
	}
	return truncate(line, m.width)
}

// maxOutsideName bounds a process name on the OUTSIDE AGENTS line.
const maxOutsideName = 24

func (m Model) heavyPane(rows int) []string {
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
		cells[i] = []string{owner(h.Owner), fmt.Sprint(h.Key.PID), h.Name, string(h.Class), cores(h.CPU), formatBytes(h.RSS), state(h.Key)}
		cols[6].width = max(cols[6].width, lipgloss.Width(cells[i][6]))
	}
	return m.table(paneHeavy, "HEAVY WORKLOADS", "", "(no heavy workloads)", cols, cells, rows, "")
}

// stateOf returns a lookup of each workload's scheduling state: "QUEUED",
// "PAUSED", "running (would queue)" for work observe mode would have
// queued, or "" while it runs.
func (m Model) stateOf() func(domain.ProcessKey) string {
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
	return func(k domain.ProcessKey) string {
		switch {
		case queued[k]:
			return "QUEUED"
		case paused[k]:
			return "PAUSED"
		case would[k]:
			return "running (would queue)"
		}
		return ""
	}
}

// drillInfoLines summarizes the drilled-into agent.
func (m Model) drillInfoLines() []string {
	title := "AGENT " + m.drillLabel + "  (esc back)"
	a, ok := m.drillAgent()
	if !ok {
		return []string{truncate(m.st.title.Render(title), m.width),
			truncate(fmt.Sprintf("  pid %d is no longer reported by throtd", m.drillKey.PID), m.width)}
	}
	return m.fieldLines(title, agentFields(a))
}

// drillPane lists the heavy workloads the drilled-into agent owns.
func (m Model) drillPane(rows int) []string {
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
		st := state(h.Key)
		if st == "" {
			st = "running"
		}
		cells[i] = []string{fmt.Sprint(h.Key.PID), h.Name, string(h.Class), cores(h.CPU), formatBytes(h.RSS), st}
		cols[5].width = max(cols[5].width, lipgloss.Width(st))
	}
	return m.table(paneDrill, "HEAVY WORKLOADS", "", "(no heavy workloads)", cols, cells, rows, "")
}

// table renders a titled, scrollable table of exactly rows+2 lines. note
// follows the title. A non-empty more replaces the last row: the table then
// shows its first rows-1 rows and more summarizes the rest.
func (m Model) table(p pane, title, note, empty string, cols []column, cells [][]string, rows int, more string) []string {
	focused := m.active() == p && !m.help
	mark := "  "
	if focused {
		mark = "▸ "
	}
	head := mark + title
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
	widths := fitColumns(cols, want, m.width-markerWidth)
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
	lines := []string{truncate(m.st.title.Render(head), m.width),
		m.st.header.Render(strings.Repeat(" ", markerWidth) + formatRow(cols, widths, header))}
	if len(cells) == 0 {
		msg := empty
		if !m.connected {
			msg = "(no data)"
		}
		lines = append(lines, truncate(strings.Repeat(" ", markerWidth)+msg, m.width))
	}
	for i := m.off[p]; i < len(cells) && i < m.off[p]+visible; i++ {
		row := formatRow(cols, widths, cells[i])
		if focused && i == m.sel[p] {
			lines = append(lines, m.st.selected.Render("> "+row))
		} else {
			lines = append(lines, "  "+row)
		}
	}
	if more != "" {
		lines = append(lines, m.st.header.Render(truncate(strings.Repeat(" ", markerWidth)+more, m.width)))
	}
	for len(lines) < rows+2 {
		lines = append(lines, "")
	}
	return lines
}

// detailsLines describes the selected row using only fields the daemon
// already reports.
func (m Model) detailsLines() []string {
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
		return []string{m.st.title.Render("DETAILS"), "  nothing selected"}
	}
	return m.fieldLines(title, fields)
}

func agentFields(a process.AgentView) [][2]string {
	return [][2]string{
		{"executable", a.Name}, {"pid", fmt.Sprint(a.Key.PID)}, {"started", startTime(a.Key)},
		{"cpu", cores(a.CPU)}, {"mem", formatBytes(a.RSS)}, {"children", fmt.Sprint(a.Children)},
	}
}

// fieldLines renders a title and "name value" fields wrapped to the width.
func (m Model) fieldLines(title string, fields [][2]string) []string {
	lines := []string{truncate(m.st.title.Render(title), m.width)}
	line := " "
	for _, f := range fields {
		item := " " + m.st.header.Render(f[0]) + " " + f[1]
		if lipgloss.Width(line)+lipgloss.Width(item) > m.width && line != " " {
			lines = append(lines, line)
			line = " "
		}
		line += item
	}
	return append(lines, truncate(line, m.width))
}

// timeline renders the newest events first, one per line. Messages are
// whole sentences, so the kind is shown only for an event without one.
// Control, scheduler and failure events carry a tag (CTL, SCHED, FAIL) as
// well as a color. With a filter only matching events are listed.
func (m Model) timeline(rows int) []string {
	events := m.shownEvents()
	title := m.st.title.Render("TIMELINE")
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
	lines := []string{truncate(title, m.width)}
	// time and tag columns with their gaps
	msgWidth := m.width - (2 + 8 + 2 + eventTagWidth + 2)
	for _, it := range coalesce(events) {
		if len(lines) > rows {
			break
		}
		e := it.e
		st := m.st.event[classify(e.Kind)]
		text := it.text()
		line := fmt.Sprintf("  %s  %s  %s", e.Time.Local().Format("15:04:05"),
			st.Render(eventTags[classify(e.Kind)]), truncate(text, max(0, msgWidth)))
		lines = append(lines, truncate(strings.TrimRight(line, " "), m.width))
	}
	switch {
	case len(m.events) == 0:
		lines = append(lines, "  (no events)")
	case len(events) == 0:
		lines = append(lines, truncate("  (no event matches the filter; esc clears it)", m.width))
	}
	return lines
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

func (m Model) helpLines() []string {
	lines := []string{
		m.st.title.Render("HELP"),
		"  q, ctrl+c   quit (throtd keeps running)",
		"  tab         switch focus: agents / heavy workloads (shift+tab too)",
		"  j/k, ↑/↓    move the selection",
		"  enter       agents: open the agent drilldown; heavy: details",
		"  esc         leave the drilldown; close details and help;",
		"              else clear the timeline filter",
		"  /           filter the timeline by kind or text (enter keeps, esc clears)",
		"  p / r       pause / resume the selected heavy workload (asks y/n)",
		"  s           sort the table: auto, cpu, mem, name, daemon order",
		"  ?           toggle this help",
		"",
		"  Auto sorts by memory while pressure is from memory, else by CPU.",
		"  Agents beyond the first rows fold into a +N more row; j scrolls on.",
		"  Sparklines: the latest polls of CPU, memory used, swap growth",
		"  (on the SWAP line) and latency (in the header).",
		"  The TUI makes no decisions: p and r send only what you confirm,",
		"  and throtd checks ownership before acting.",
	}
	for i, l := range lines {
		lines[i] = truncate(l, m.width)
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
