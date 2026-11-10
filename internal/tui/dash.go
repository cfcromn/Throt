package tui

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/domain"
)

// The dashboard is a grid of panels: boxes with a title in the top border.
// Every panel is drawn exactly as wide and as high as the grid gives it, so
// panels join side by side without measuring.

// borderSet is the characters of one border style.
type borderSet struct{ tl, tr, bl, br, h, v string }

var roundBorder = borderSet{"╭", "╮", "╰", "╯", "─", "│"}

// panel is one dashboard box before it is drawn.
type panel struct {
	title string
	// note follows the title in the secondary color: a count, a time span.
	note string
	body []string
	// focused draws the accent border and the ▸ marker.
	focused bool
	// border, when set, replaces the accent border style.
	border *lipgloss.Style
}

// panelPad is a bordered panel's border and inner padding, per side.
const panelPad = 2

// innerWidth is the content width of a bordered panel w wide.
func innerWidth(w int) int { return max(0, w-2*panelPad) }

// draw renders p in exactly h lines. Bordered, every line is exactly w
// cells wide; bare (no border, used when rows are scarce), the title is the
// first line and lines are at most w wide.
func (m Model) draw(p panel, w, h int, bare bool) []string {
	if h <= 0 || w <= 0 {
		return nil
	}
	title := tr(p.title)
	if title != "" {
		mark := "  "
		if p.focused {
			mark = "▸ "
		}
		if !bare {
			mark = strings.TrimLeft(mark, " ")
		}
		title = mark + title
	}
	// heading styles the title and what fits of the note in at most n cells.
	heading := func(n int) string {
		st := m.st.panelTitle
		if p.focused {
			st = m.st.focusTitle
		}
		t := truncate(title+p.note, n)
		if len(t) < len(title) || !strings.HasPrefix(t, title) {
			return st.Render(t)
		}
		return st.Render(title) + m.st.header.Render(t[len(title):])
	}
	if bare || w < 8 || h < 2 {
		var lines []string
		if p.title != "" {
			lines = append(lines, heading(w))
		}
		for _, l := range p.body {
			lines = append(lines, truncate(l, w))
		}
		for len(lines) < h {
			lines = append(lines, "")
		}
		return lines[:h]
	}
	b, bs := m.st.border, roundBorder
	if p.border != nil {
		b = *p.border
	}
	if p.focused {
		b = m.st.focusBorder
	}
	lines := make([]string, 0, h)
	top := b.Render(bs.tl + bs.h)
	if title != "" && w >= 6 {
		t := heading(w - 5)
		top += " " + t + " " + b.Render(strings.Repeat(bs.h, w-5-lipgloss.Width(t))+bs.tr)
	} else {
		top += b.Render(strings.Repeat(bs.h, w-3) + bs.tr)
	}
	lines = append(lines, top)
	cw := innerWidth(w)
	side := b.Render(bs.v)
	for i := range h - 2 {
		l := ""
		if i < len(p.body) {
			l = truncate(p.body[i], cw)
		}
		lines = append(lines, side+" "+pad(l, cw, false)+" "+side)
	}
	return append(lines, b.Render(bs.bl+strings.Repeat(bs.h, w-2)+bs.br))
}

// hjoin places equally high columns side by side, gap cells apart.
func hjoin(gap int, cols ...[]string) []string {
	if len(cols) == 0 {
		return nil
	}
	sp := strings.Repeat(" ", gap)
	out := make([]string, len(cols[0]))
	for i := range out {
		parts := make([]string, len(cols))
		for j, c := range cols {
			if i < len(c) {
				parts[j] = c[i]
			}
		}
		out[i] = strings.Join(parts, sp)
	}
	return out
}

// splitWidths divides total cells, less a gap between neighbours, in
// proportion to weights; the remainder goes to the first ones.
func splitWidths(total, gap int, weights []float64) []int {
	avail := total - gap*(len(weights)-1)
	var sum float64
	for _, w := range weights {
		sum += w
	}
	out := make([]int, len(weights))
	used := 0
	for i, w := range weights {
		out[i] = int(float64(avail) * w / sum)
		used += out[i]
	}
	for i := 0; used < avail; i = (i + 1) % len(out) {
		out[i]++
		used++
	}
	return out
}

// gridGap is the space between dashboard panels.
const gridGap = 1

// level returns the threshold style of a fraction: plain below 70%, yellow
// below 85%, red above.
func (m Model) level(frac float64) lipgloss.Style {
	switch {
	case frac >= 0.85:
		return m.st.bad
	case frac >= 0.70:
		return m.st.warn
	}
	return m.st.ok
}

// fill is the color of a bar or a chart row at a fraction: the accent, and
// the warning colors from the same thresholds as level.
func (m Model) fill(frac float64) lipgloss.Style {
	if frac >= 0.70 {
		return m.level(frac).UnsetBold()
	}
	return m.st.agentBar
}

// Latency thresholds for the LATENCY panel's color.
const (
	latencyWarn = 25 * time.Millisecond
	latencyBad  = 100 * time.Millisecond
)

// stat is one stat panel's content: a value, details under it, and a
// sparkline on the last row.
type stat struct {
	title string
	// value is plain text; valueStyle colors it. A figure is a number and
	// its unit, the unit in the secondary color; badge draws the value as a
	// filled label instead. meter draws frac as a bar under the value.
	value      string
	valueStyle lipgloss.Style
	figure     bool
	badge      bool
	meter      bool
	frac       float64
	// calm keeps the meter in the accent color whatever frac is.
	calm    bool
	details []string
	spark   string
	border  *lipgloss.Style
	// suffix follows a badge on its line when it fits.
	suffix string
	// why is set when the first detail is the pressure reason.
	why bool
	// short holds shorter forms of details, by text, for narrow panels.
	short map[string]string
}

// statBody lays a stat out in rows lines cw wide.
func (m Model) statBody(s stat, cw, rows int) []string {
	var lines []string
	value := s.valueStyle.Render(s.value)
	if s.badge {
		word, rest, _ := strings.Cut(s.value, "  ")
		value = s.valueStyle.Render(" " + word + " ")
		if rest != "" {
			value += "  " + rest
		}
	}
	if n := len(s.value) - len(strings.TrimLeft(s.value, "0123456789.")); s.figure && n > 0 {
		value = s.valueStyle.Render(s.value[:n]) + m.st.header.Render(s.value[n:])
	}
	// The meter goes under the value when there is a row for it, else
	// beside it.
	beside := max(6, lipgloss.Width(s.value)+2) // "100%" and two spaces
	fill := m.fill(s.frac)
	if s.calm {
		fill = m.st.agentBar
	}
	switch {
	case !s.meter:
	case rows >= 5:
		lines = append(lines, value, m.meter(s.frac, cw, fill))
	case cw >= beside+6:
		value += strings.Repeat(" ", beside-lipgloss.Width(value)) + m.meter(s.frac, cw-beside, fill)
	}
	if len(lines) == 0 {
		lines = append(lines, value)
	}
	room := rows - len(lines)
	if s.spark != "" {
		room--
	}
	for _, d := range s.details[:min(len(s.details), max(0, room))] {
		lines = append(lines, m.st.header.Render(d))
	}
	for s.spark != "" && len(lines) < rows-1 {
		lines = append(lines, "")
	}
	if s.spark != "" && rows >= 2 {
		lines = append(lines, s.spark)
	}
	return lines
}

// statRow renders the stat panels in one row h lines high: PRESSURE, CPU,
// MEMORY, SWAP, LATENCY (on a wide screen; else in the PRESSURE panel) and
// SCHEDULER.
func (m Model) statRow(w, h int) []string {
	wide, latency := w >= wideWidth, w >= latencyWidth
	stats := []stat{m.pressureStat(latency), m.cpuStat(), m.memStat(), m.swapStat()}
	weights := []float64{1.2, 1, 1, 1}
	if wide {
		weights[0] = 1.4
	}
	if latency {
		stats = append(stats, m.latencyStat())
		weights = append(weights, 0.9)
	}
	stats = append(stats, m.schedulerStat())
	weights = append(weights, 1.4)
	if wide {
		weights[len(weights)-1] = 1.7
	}
	widths := splitWidths(w, gridGap, weights)
	cols := make([][]string, len(stats))
	for i, s := range stats {
		cw := innerWidth(widths[i])
		// The sparkline is as wide as the panel.
		switch s.title {
		case "PRESSURE":
			s.spark = m.levelSpark(cw)
		case "CPU":
			s.spark = m.histSpark(cpuOf, 1, 0, cw)
		case "MEMORY":
			s.spark = m.histSpark(memOf, 1, 0, cw)
		case "SWAP":
			s.spark = m.histSpark(swapOf, 0, swapScaleFloor, cw)
		case "LATENCY":
			s.spark = m.histSpark(latencyOf, 0, latencyScaleFloor, cw)
		}
		if !m.connected || m.status.Host == nil {
			s.spark = ""
		}
		for j, d := range s.details {
			if alt, ok := s.short[d]; ok && lipgloss.Width(d) > cw {
				s.details[j] = alt
			}
		}
		switch s.title {
		case "SCHEDULER":
			if len(s.details) > 0 && lipgloss.Width(s.details[0]) > cw {
				s.details[0] = m.shortLimit()
			}
			s.details = wrapItems(s.details, cw, " · ")
		case "PRESSURE":
			// The latency follows the level when it fits beside it (the
			// badge pads the level by a cell each side), else it is a detail.
			if s.suffix != "" {
				if lipgloss.Width(s.value)+2+2+lipgloss.Width(s.suffix) <= cw {
					s.value += "  " + s.suffix
				} else {
					s.details = append(s.details, s.suffix)
				}
			}
			// The why line wraps only while the other details still fit.
			if len(s.details) > 0 && s.why {
				room := h - 2 - 2 - len(s.details) // less the value and sparkline rows
				wrapped := splitLines(lipgloss.Wrap(s.details[0], cw, ""))
				if keep := min(len(wrapped), room+1); keep > 1 {
					// What does not fit is cut at the end of the last row.
					last := strings.Join(wrapped[keep-1:], " ")
					wrapped = append(wrapped[:keep-1], truncate(last, cw))
					s.details = append(wrapped, s.details[1:]...)
				}
			}
		}
		cols[i] = m.draw(panel{title: s.title, body: m.statBody(s, cw, h-2), border: s.border}, widths[i], h, false)
	}
	return hjoin(gridGap, cols...)
}

// wideWidth is the width from which the stat panels get more room;
// latencyWidth the width from which latency gets a panel of its own.
const (
	wideWidth    = 140
	latencyWidth = 120
)

// histSpark renders one signal's history in width cells, or "" when the
// history is empty.
func (m Model) histSpark(get func(histSample) (float64, bool), scale, floor float64, width int) string {
	if m.hist.n == 0 || width <= 0 {
		return ""
	}
	vals, ok := m.hist.series(width, get)
	return m.st.agentBar.Render(sparkline(vals, ok, scale, floor, width))
}

// levelBlocks are the pressure levels as sparkline cells, so the state
// history reads without color too.
var levelBlocks = [...]rune{' ', '▁', '▃', '▅', '█'}

// levelSpark is the pressure level history, each cell in its level's color.
func (m Model) levelSpark(width int) string {
	if m.hist.n == 0 || width <= 0 {
		return ""
	}
	vals, _ := m.hist.series(width, levelOf)
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(vals)))
	for _, v := range vals {
		l := max(0, min(int(v), len(levelBlocks)-1))
		b.WriteString(m.st.levelBorder[l].Render(string(levelBlocks[l])))
	}
	return b.String()
}

func (m Model) host() (*domain.HostSample, bool) {
	h := m.status.Host
	return h, m.connected && h != nil
}

// pressureStat is the level, why, PSI and, when no LATENCY panel shows it
// (latencyPanel is false), the latency.
func (m Model) pressureStat(latencyPanel bool) stat {
	s := stat{title: "PRESSURE", value: "n/a", badge: true}
	if !m.connected {
		return s
	}
	p := m.status.Pressure
	s.value = tr(strings.ToUpper(p.String()))
	if int(p) >= 0 && int(p) < len(m.st.badge) {
		s.valueStyle = m.st.badge[p]
		// The panel takes the level's color only once there is pressure.
		if p >= domain.PressureElevated {
			b := m.st.levelBorder[p]
			s.border = &b
		}
	}
	if m.status.Reason != "" {
		text, _ := explainReason(m.status.Reason)
		s.details, s.why = append(s.details, tr("why: ")+text), true
	}
	if h, ok := m.host(); ok {
		if psi := strings.TrimSpace(psiSummary(*h)); psi != "" {
			s.details = append(s.details, psi)
		}
		if h.Has(domain.MetricLatency) && !latencyPanel {
			s.suffix = tr("latency ") + h.Latency.Round(time.Millisecond).String()
		}
	}
	return s
}

func (m Model) cpuStat() stat {
	s := stat{title: "CPU", value: "n/a", valueStyle: m.st.header}
	h, ok := m.host()
	if ok && h.Has(domain.MetricCPU) {
		s.value, s.valueStyle, s.figure = percent(h.CPUUsage), m.level(h.CPUUsage), true
		s.meter, s.frac = true, h.CPUUsage
	}
	if ok && h.Has(domain.MetricLoad) {
		d := fmt.Sprintf(tr("load %.2f/cpu"), h.Load1)
		s.details, s.short = append(s.details, d), map[string]string{d: fmt.Sprintf(tr("load %.2f"), h.Load1)}
	}
	return s
}

func (m Model) memStat() stat {
	s := stat{title: "MEMORY", value: "n/a", valueStyle: m.st.header}
	h, ok := m.host()
	if ok && h.Has(domain.MetricMemory) {
		used := h.MemTotal - min(h.MemAvailable, h.MemTotal)
		f := ratio(used, h.MemTotal)
		s.value, s.valueStyle, s.figure = percent(f), m.level(f), true
		s.meter, s.frac = true, f
		d := bytesPair(used, h.MemTotal)
		s.details, s.short = append(s.details, d), map[string]string{d: formatBytes(used)}
	}
	if ok && h.HasMemoryPressure() {
		s.details = append(s.details, tr("kernel: ")+tr(h.MemoryPressure.String()))
	}
	return s
}

func (m Model) swapStat() stat {
	s := stat{title: "SWAP", value: "n/a", valueStyle: m.st.header}
	h, ok := m.host()
	if ok && h.Has(domain.MetricSwap) {
		// The amount, not a share of the swap space: macOS grows that space
		// as it fills, so it always reads nearly full.
		s.value, s.valueStyle, s.figure = formatBytes(h.SwapUsed), m.st.ok, true
		s.meter, s.frac, s.calm = true, swapShare(*h), true
		if last, ok := m.hist.last(); ok && last.hasSwapRate {
			s.details = append(s.details, tr("growth ")+formatRate(last.swapRate))
		}
		s.details = append(s.details, tr("of ")+formatBytes(h.SwapTotal))
	}
	return s
}

// swapShare is swap in use as a share of physical memory, at most 1, or of
// the swap space when the sample has no memory size.
func swapShare(h domain.HostSample) float64 {
	if h.Has(domain.MetricMemory) && h.MemTotal > 0 {
		return min(1, ratio(h.SwapUsed, h.MemTotal))
	}
	return ratio(h.SwapUsed, h.SwapTotal)
}

func (m Model) latencyStat() stat {
	s := stat{title: "LATENCY", value: "n/a", valueStyle: m.st.header}
	h, ok := m.host()
	if ok && h.Has(domain.MetricLatency) {
		l := h.Latency.Round(time.Millisecond)
		s.value, s.figure = fmt.Sprintf("%dms", l.Milliseconds()), true
		switch {
		case l >= latencyBad:
			s.valueStyle = m.st.bad
		case l >= latencyWarn:
			s.valueStyle = m.st.warn
		default:
			s.valueStyle = m.st.ok
		}
		s.details, s.short = append(s.details, tr("foreground lag")), map[string]string{tr("foreground lag"): tr("lag")}
	}
	return s
}

// schedulerStat shows the mode and, as items wrapped to the panel, the
// heavy limit, queued work, the per-agent cap and reserved leases.
func (m Model) schedulerStat() stat {
	s := stat{title: "SCHEDULER", value: "n/a", valueStyle: m.st.header}
	if !m.connected {
		return s
	}
	sc := m.status.Scheduler
	if sc.Mode == "" || sc.Mode == "off" {
		s.value = tr("OFF")
		s.details, s.short = []string{tr("not scheduling")}, map[string]string{tr("not scheduling"): tr("idle")}
		return s
	}
	s.value, s.valueStyle = strings.ToUpper(string(sc.Mode)), m.st.focusTitle
	// Queued work comes right after the limit, so a small panel keeps it.
	items := m.schedulerItems()
	n := len(items)
	s.details = append([]string{items[0], items[n-2]}, items[1:n-2]...)
	s.details = append(s.details, items[n-1])
	return s
}

// bytesPair renders "used / total", sharing the unit when both are GiB.
func bytesPair(used, total uint64) string {
	if used >= 1<<30 && total >= 1<<30 {
		return fmt.Sprintf("%.1f/%s", float64(used)/(1<<30), formatBytes(total))
	}
	return formatBytes(used) + " / " + formatBytes(total)
}

// shortLimit is the heavy limit item of a narrow SCHEDULER panel.
func (m Model) shortLimit() string {
	sc := m.status.Scheduler
	if sc.Adaptive {
		return fmt.Sprintf(tr("limit %d (%d–%d)"), sc.HeavyLimit, sc.StartLimit, sc.MaxLimit)
	}
	return fmt.Sprintf(tr("limit %d"), sc.HeavyLimit)
}

// schedulerItems are the scheduler summary in the words of `throt status`.
func (m Model) schedulerItems() []string {
	sc := m.status.Scheduler
	limit := fmt.Sprintf(tr("limit %d"), sc.HeavyLimit)
	if sc.Adaptive {
		limit += fmt.Sprintf(tr(" (adaptive, start %d, max %d)"), sc.StartLimit, sc.MaxLimit)
	}
	items := []string{limit}
	if sc.PerAgentLimit > 0 {
		items = append(items, fmt.Sprintf(tr("per-agent %d"), sc.PerAgentLimit))
	}
	if sc.Mode == "observe" {
		items = append(items, fmt.Sprintf(tr("%d would queue"), len(sc.WouldQueue)))
	} else {
		items = append(items, fmt.Sprintf(tr("%d queued"), len(sc.Queued)))
	}
	return append(items, fmt.Sprintf(tr("%d reserved"), len(sc.Leases)))
}

// wrapItems joins items with sep into lines at most width wide, breaking
// only between items; an item wider than a line is cut.
func wrapItems(items []string, width int, sep string) []string {
	var lines []string
	line := ""
	for _, it := range items {
		switch {
		case line == "":
			line = it
		case lipgloss.Width(line)+lipgloss.Width(sep)+lipgloss.Width(it) <= width:
			line += sep + it
		default:
			lines = append(lines, truncate(line, width))
			line = it
		}
	}
	if line != "" {
		lines = append(lines, truncate(line, width))
	}
	return lines
}

func percent(f float64) string { return fmt.Sprintf("%.0f%%", max(0, f)*100) }

// chartRow renders the CPU, MEMORY and SWAP time series side by side.
func (m Model) chartRow(w, h int) []string {
	series := []struct {
		title, unit string
		get         func(histSample) (float64, bool)
	}{{"CPU", " %", cpuOf}, {"MEMORY", " %", memOf}, {"SWAP", " % of RAM", swapUsedOf}}
	widths := splitWidths(w, gridGap, []float64{1, 1, 1})
	cols := make([][]string, len(series))
	for i, s := range series {
		note := tr(s.unit) + tr(" · last ") + shortDuration(historyLen*pollInterval)
		cols[i] = m.draw(panel{title: s.title, note: note, body: m.chart(s.get, innerWidth(widths[i]), h-2)}, widths[i], h, false)
	}
	return hjoin(gridGap, cols...)
}

func shortDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.Round(time.Second).String()
}

// chartAxisWidth is the y axis: a label and a space.
const chartAxisWidth = 5

// chart draws a 0–100% line chart of one signal, cw cells wide and rows
// high, in braille: each cell is two dot columns wide and four steps high.
// The whole history fits: each dot column is the largest of the samples it
// covers, newest on the right. Rows take the color of their height, and a
// faint baseline runs where there is nothing to draw.
func (m Model) chart(get func(histSample) (float64, bool), cw, rows int) []string {
	if rows <= 0 {
		return nil
	}
	plotW := cw - chartAxisWidth
	if !m.connected || m.hist.n == 0 || plotW < 4 {
		return []string{m.st.header.Render(tr("(no data yet)"))}
	}
	// The chart always spans the whole history: with fewer dot columns
	// than samples each column covers several, newest on the right.
	dots := 2 * plotW
	n := max(historyLen, dots)
	vals, ok := m.hist.series(n, get)
	off := n - len(vals)       // samples not yet taken, on the left
	level := make([]int, dots) // filled dots per dot column, -1 for none
	for b := range dots {
		hi := -1.0
		for i := b * n / dots; i < max((b+1)*n/dots, b*n/dots+1); i++ {
			if j := i - off; j >= 0 && ok[j] {
				hi = max(hi, vals[j])
			}
		}
		level[b] = -1
		if hi >= 0 {
			level[b] = int(math.Round(max(0, min(hi, 1)) * float64(rows*4)))
			if hi > 0 {
				level[b] = max(level[b], 1)
			}
		}
	}
	// on reports whether dot row y of dot column b is on the line: the
	// column's own height, and the steps up or down from the column before.
	on := func(b, y int) bool {
		if level[b] < 0 {
			return false
		}
		top := max(level[b], 1) - 1
		if y == top || b == 0 || level[b-1] < 0 {
			return y == top
		}
		prev := max(level[b-1], 1) - 1
		return (y > prev && y < top) || (y < prev && y > top)
	}
	// Braille dot bits, bottom to top, of a cell's left and right column.
	leftBits := [4]rune{0x40, 0x04, 0x02, 0x01}
	rightBits := [4]rune{0x80, 0x20, 0x10, 0x08}
	lines := make([]string, rows)
	for r := range rows {
		label := ""
		switch {
		case r == 0:
			label = "100%"
		case r == rows-1:
			label = "0%"
		case rows%2 == 1 && r == rows/2:
			label = "50%"
		}
		st := m.fill((float64(rows-r) - 0.5) / float64(rows))
		var cells strings.Builder
		base := (rows - 1 - r) * 4
		for c := range plotW {
			var bits rune
			for k := range 4 {
				if on(2*c, base+k) {
					bits |= leftBits[k]
				}
				if on(2*c+1, base+k) {
					bits |= rightBits[k]
				}
			}
			switch {
			case bits != 0:
				cells.WriteString(st.Render(string(0x2800 + bits)))
			case r == rows-1:
				cells.WriteString(m.st.track.Render("⣀"))
			default:
				cells.WriteByte(' ')
			}
		}
		lines[r] = m.st.axis.Render(fmt.Sprintf("%4s ", label)) + cells.String()
	}
	return lines
}

// usage is one MEMORY BY OWNER entry: an agent or a group of same-named
// processes outside every agent.
type usage struct {
	label string
	agent bool
	rss   uint64
}

// owners returns every agent and outside group, largest first.
func (m Model) owners() []usage {
	var out []usage
	for _, a := range m.status.Agents {
		out = append(out, usage{label: a.Agent, agent: true, rss: a.RSS})
	}
	for _, u := range m.status.UnattributedMemory {
		label := u.Name
		if u.Processes > 1 {
			label += fmt.Sprintf(" ×%d", u.Processes)
		}
		out = append(out, usage{label: label, rss: u.RSS})
	}
	slices.SortStableFunc(out, func(a, b usage) int { return cmp.Compare(b.rss, a.rss) })
	return out
}

// maxGaugeLabel bounds a MEMORY BY OWNER label.
const maxGaugeLabel = 26

// gaugeBody renders the MEMORY BY OWNER bar gauge in rows lines cw wide:
// the agent and outside totals, then one bar per owner, largest first and
// scaled to the largest, then what did not fit. Agents are marked ●,
// outside groups ○.
func (m Model) gaugeBody(cw, rows int) []string {
	if !m.connected {
		return []string{tr("(no data)")}
	}
	var agentRSS uint64
	for _, a := range m.status.Agents {
		agentRSS += a.RSS
	}
	head := fmt.Sprintf(tr("● agents %s"), formatBytes(agentRSS))
	var listed uint64
	for _, u := range m.status.UnattributedMemory {
		listed += u.RSS
	}
	if len(m.status.UnattributedMemory) > 0 || m.status.UnattributedRSS > 0 {
		head += fmt.Sprintf(tr("  ○ outside %s"), formatBytes(m.status.UnattributedRSS))
	}
	// throtd names only the largest outside groups; the rest is "other".
	other := m.status.UnattributedRSS - min(listed, m.status.UnattributedRSS)
	lines := []string{m.st.header.Render(truncate(head, cw))}
	all := m.owners()
	if len(all) == 0 {
		return append(lines, tr("(no agents detected)"))
	}
	n := min(len(all), rows-1)
	if n < len(all) || other > 0 {
		n = max(0, rows-2)
	}
	labelW := 0
	for _, u := range all[:n] {
		labelW = max(labelW, lipgloss.Width(u.label))
	}
	const valueW = 8
	labelW = min(labelW, maxGaugeLabel, max(6, cw-2-valueW-2-6))
	barW := cw - 2 - labelW - 1 - valueW - 1
	top := all[0].rss
	for _, u := range all[:n] {
		mark, bar := "○", m.st.outsideBar
		if u.agent {
			mark, bar = "●", m.st.agentBar
		}
		line := mark + " " + pad(truncate(u.label, labelW), labelW, false) + " "
		if barW >= 4 {
			line += m.meter(float64(u.rss)/float64(max(top, 1)), barW, bar) + " "
		}
		lines = append(lines, line+pad(formatBytes(u.rss), valueW, true))
	}
	if rest := all[n:]; len(rest) > 0 || other > 0 {
		var agents, outside int
		var aRSS, oRSS uint64
		for _, u := range rest {
			if u.agent {
				agents, aRSS = agents+1, aRSS+u.rss
			} else {
				outside, oRSS = outside+1, oRSS+u.rss
			}
		}
		var parts []string
		if agents > 0 {
			parts = append(parts, fmt.Sprintf(tr("+%d agents %s"), agents, formatBytes(aRSS)))
		}
		if outside > 0 {
			parts = append(parts, fmt.Sprintf(tr("+%d outside %s"), outside, formatBytes(oRSS)))
		}
		if other > 0 {
			// Outside memory throtd does not break down by name.
			parts = append(parts, tr("○ other ")+formatBytes(other))
		}
		lines = append(lines, m.st.header.Render(truncate(strings.Join(parts, " · "), cw)))
	}
	return lines
}

// meter is a thin bar of frac of width cells, in half cells, on a faint
// track.
func (m Model) meter(frac float64, width int, st lipgloss.Style) string {
	if width <= 0 {
		return ""
	}
	halves := int(math.Round(max(0, min(frac, 1)) * float64(width*2)))
	if frac > 0 {
		halves = max(halves, 1)
	}
	bar := strings.Repeat("━", halves/2) + strings.Repeat("╸", halves%2)
	return st.Render(bar) + m.st.track.Render(strings.Repeat("─", width-(halves+1)/2))
}
