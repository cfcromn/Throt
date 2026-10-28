package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/domain"
)

const (
	minWidth  = 40
	minHeight = 12
	// marker prefixes every table row; the selected row in the focused pane
	// gets "> " so selection never depends on color alone.
	markerWidth = 2
)

// Read-only styles.
var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	headerStyle   = lipgloss.NewStyle().Faint(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	bannerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Red)
	degradedStyle = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	footerStyle   = lipgloss.NewStyle().Faint(true)
	pressureStyle = [...]lipgloss.Style{
		domain.PressureUnknown:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightBlack),
		domain.PressureNormal:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green),
		domain.PressureElevated: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Yellow),
		domain.PressureHigh:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")),
		domain.PressureCritical: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Red),
	}
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
		return lipgloss.Wrap(fmt.Sprintf("terminal too small (%dx%d); throt needs at least %dx%d. Press q to quit.",
			m.width, m.height, minWidth, minHeight), m.width, "")
	}
	l := m.layout()
	lines := append([]string{}, l.top...)
	if m.help {
		lines = append(lines, m.helpLines()...)
	} else {
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

type layout struct {
	top, details                       []string
	agentRows, heavyRows, timelineRows int
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
	if m.details && !m.help {
		l.details = m.detailsLines()
	}
	const paneOverhead = 2 // title and column header of a table
	rest := m.height - len(l.top) - len(l.details) - 2*paneOverhead - 1
	if rest >= 5 {
		l.timelineRows = max(1, (rest-1)/4)
		rest -= l.timelineRows + 1
	}
	agentsWant, heavyWant := max(1, len(m.status.Agents)), max(1, len(m.status.Heavy))
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
// degraded notice.
func (m Model) topLines() []string {
	w := m.width
	var lines []string
	switch {
	case !m.polled:
		lines = append(lines, truncate("connecting to throtd at "+m.socket+"…", w))
	case !m.connected:
		msg := fmt.Sprintf("cannot reach throtd at %s: start it with `throtd`", m.socket)
		lines = append(lines, splitLines(bannerStyle.Render(lipgloss.Wrap(msg, w, "/")))...)
		lines = append(lines, truncate(fmt.Sprintf("retrying every %s; last error: %v", pollInterval, m.err), w))
	}

	pressure := "n/a"
	if m.connected {
		p := m.status.Pressure
		name := strings.ToUpper(p.String())
		if int(p) >= 0 && int(p) < len(pressureStyle) {
			name = pressureStyle[p].Render(name)
		}
		pressure = name
		if m.status.Reason != "" {
			pressure += "  " + m.status.Reason
		}
		if h := m.status.Host; h != nil && h.Has(domain.MetricLatency) {
			pressure += "  latency " + h.Latency.Round(time.Millisecond).String()
		}
		if sc := m.status.Scheduler; sc.Mode != "" && sc.Mode != "off" {
			pressure += fmt.Sprintf("  scheduler %s %d", sc.Mode, sc.HeavyLimit)
		}
	}
	lines = append(lines, truncate(titleStyle.Render("SYSTEM")+"  pressure "+pressure, w))
	lines = append(lines, m.metricLines()...)

	if m.connected && m.status.Degraded != "" {
		lines = append(lines, splitLines(degradedStyle.Render(lipgloss.Wrap("DEGRADED  "+m.status.Degraded, w, "")))...)
	}
	return lines
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
	cpuLine := m.bar("CPU", has(domain.MetricCPU), cpuFrac, cpu)
	if !has(domain.MetricCPU) && has(domain.MetricLoad) {
		cpuLine = truncate("CPU   n/a  "+load, m.width)
	}
	memLine := m.bar("MEM", has(domain.MetricMemory), memFrac, mem)
	if !has(domain.MetricMemory) && kernel != "" {
		memLine = truncate("MEM   n/a  "+kernel, m.width)
	}
	return []string{
		cpuLine,
		memLine,
		m.bar("SWAP", has(domain.MetricSwap), swapFrac, swap),
	}
}

// bar renders "LABEL ████░░░░  42%  detail", or "LABEL n/a" when !ok.
func (m Model) bar(label string, ok bool, frac float64, detail string) string {
	if !ok {
		return fmt.Sprintf("%-5s n/a", label)
	}
	frac = max(0, min(frac, 1))
	const detailWidth = 21
	width := max(5, min(40, m.width-14-detailWidth))
	fill := int(frac*float64(width) + 0.5)
	return truncate(fmt.Sprintf("%-5s %s%s %4.0f%%  %s", label,
		strings.Repeat("█", fill), strings.Repeat("░", width-fill), frac*100, detail), m.width)
}

func (m Model) agentsPane(rows int) []string {
	cols := []column{
		{title: "AGENT", min: 8},
		{title: "PID", width: 7, right: true, drop: 2},
		{title: "CPU", width: 6, right: true},
		{title: "MEM", width: 9, right: true},
		{title: "CHILDREN", width: 8, right: true, drop: 1},
	}
	cells := make([][]string, len(m.status.Agents))
	for i, a := range m.status.Agents {
		cells[i] = []string{a.Agent, fmt.Sprint(a.Key.PID), cores(a.CPU), formatBytes(a.RSS), fmt.Sprint(a.Children)}
	}
	return m.table(paneAgents, "AGENTS", "(no agents detected)", cols, cells, rows)
}

func (m Model) heavyPane(rows int) []string {
	cols := []column{
		{title: "OWNER", min: 6},
		{title: "PID", width: 7, right: true, drop: 3},
		{title: "PROCESS", min: 8},
		{title: "CLASS", width: 15, drop: 2},
		{title: "CPU", width: 6, right: true},
		{title: "MEM", width: 9, right: true},
		{title: "STATE", width: 6, drop: 1},
	}
	paused := make(map[domain.ProcessKey]bool, len(m.status.Paused))
	for _, p := range m.status.Paused {
		paused[p.Key] = true
	}
	queued := make(map[domain.ProcessKey]bool, len(m.status.Scheduler.Queued))
	for _, k := range m.status.Scheduler.Queued {
		queued[k] = true
	}
	cells := make([][]string, len(m.status.Heavy))
	for i, h := range m.status.Heavy {
		state := ""
		switch {
		case queued[h.Key]:
			state = "QUEUED"
		case paused[h.Key]:
			state = "PAUSED"
		}
		cells[i] = []string{owner(h.Owner), fmt.Sprint(h.Key.PID), h.Name, string(h.Class), cores(h.CPU), formatBytes(h.RSS), state}
	}
	return m.table(paneHeavy, "HEAVY WORKLOADS", "(no heavy workloads)", cols, cells, rows)
}

// table renders a titled, scrollable table of exactly rows+2 lines.
func (m Model) table(p pane, title, empty string, cols []column, cells [][]string, rows int) []string {
	focused := m.focus == p && !m.help
	mark := "  "
	if focused {
		mark = "▸ "
	}
	head := mark + title
	if n := len(cells); n > rows {
		head += fmt.Sprintf(" %d-%d of %d", m.off[p]+1, min(m.off[p]+rows, n), n)
	} else if n > 0 {
		head += fmt.Sprintf(" (%d)", n)
	}
	widths := fitColumns(cols, m.width-markerWidth)
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.title
	}
	lines := []string{truncate(titleStyle.Render(head), m.width),
		headerStyle.Render(strings.Repeat(" ", markerWidth) + formatRow(cols, widths, header))}
	if len(cells) == 0 {
		msg := empty
		if !m.connected {
			msg = "(no data)"
		}
		lines = append(lines, truncate(strings.Repeat(" ", markerWidth)+msg, m.width))
	}
	for i := m.off[p]; i < len(cells) && i < m.off[p]+rows; i++ {
		row := formatRow(cols, widths, cells[i])
		if focused && i == m.sel[p] {
			lines = append(lines, selectedStyle.Render("> "+row))
		} else {
			lines = append(lines, "  "+row)
		}
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
	case m.focus == paneAgents && len(m.status.Agents) > 0:
		a := m.status.Agents[m.sel[paneAgents]]
		title = "DETAILS  agent " + a.Agent
		fields = [][2]string{
			{"executable", a.Name}, {"pid", fmt.Sprint(a.Key.PID)}, {"started", startTime(a.Key)},
			{"cpu", cores(a.CPU)}, {"mem", formatBytes(a.RSS)}, {"children", fmt.Sprint(a.Children)},
		}
	case m.focus == paneHeavy && len(m.status.Heavy) > 0:
		h := m.status.Heavy[m.sel[paneHeavy]]
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
		return []string{titleStyle.Render("DETAILS"), "  nothing selected"}
	}
	lines := []string{truncate(titleStyle.Render(title), m.width)}
	line := " "
	for _, f := range fields {
		item := " " + headerStyle.Render(f[0]) + " " + f[1]
		if lipgloss.Width(line)+lipgloss.Width(item) > m.width && line != " " {
			lines = append(lines, line)
			line = " "
		}
		line += item
	}
	return append(lines, truncate(line, m.width))
}

// timeline renders the newest events first, one per line.
func (m Model) timeline(rows int) []string {
	title := "TIMELINE"
	if m.eventsErr != nil {
		title += "  (events unavailable: " + m.eventsErr.Error() + ")"
	}
	lines := []string{truncate(titleStyle.Render(title), m.width)}
	kindWidth := min(18, m.width/3)
	for i := len(m.events) - 1; i >= 0 && len(lines) <= rows; i-- {
		e := m.events[i]
		line := fmt.Sprintf("  %s  %s  %s", e.Time.Local().Format("15:04:05"), pad(truncate(string(e.Kind), kindWidth), kindWidth, false), e.Message)
		lines = append(lines, truncate(line, m.width))
	}
	if len(m.events) == 0 {
		lines = append(lines, "  (no events)")
	}
	return lines
}

func (m Model) helpLines() []string {
	lines := []string{
		titleStyle.Render("HELP"),
		"  q, ctrl+c   quit (throtd keeps running)",
		"  tab         switch focus: agents / heavy workloads",
		"  j/k, ↑/↓    move the selection",
		"  enter       toggle details for the selected row",
		"  p / r       pause / resume the selected heavy workload (asks y/n)",
		"  ?           toggle this help",
		"  esc         close details and help",
		"",
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
		return titleStyle.Render(truncate(fmt.Sprintf("%s %s (pid %d)? y/n", a.verb, a.name, a.key.PID), m.width))
	}
	if m.notice != "" {
		return truncate(m.notice, m.width)
	}
	return footerStyle.Render(truncate("q quit  tab focus  j/k move  enter details  ? help", m.width))
}

// column describes one table column. A column with width 0 is flexible and
// shares the leftover space; columns with a higher drop value are removed
// first when the table does not fit.
type column struct {
	title string
	width int
	min   int
	right bool
	drop  int
}

const (
	columnGap    = 2
	maxFlexWidth = 32
)

// fitColumns returns each column's width for a table avail cells wide;
// a dropped column gets width 0.
func fitColumns(cols []column, avail int) []int {
	keep := make([]bool, len(cols))
	for i := range keep {
		keep[i] = true
	}
	for {
		need, flex, worst := 0, 0, -1
		for i, c := range cols {
			if !keep[i] {
				continue
			}
			if need > 0 {
				need += columnGap
			}
			if c.width == 0 {
				need += c.min
				flex++
			} else {
				need += c.width
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
					widths[i] = min(maxFlexWidth, c.min+spare/max(flex, 1))
				default:
					widths[i] = c.width
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
// it cuts. It is ANSI-aware only in measuring: a styled string that does not
// fit loses its styling.
func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	s = stripStyles(s)
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

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
