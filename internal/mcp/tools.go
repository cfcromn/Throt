package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/capacity"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/stats"
)

const (
	defaultEventLimit = 20
	maxEventLimit     = 200
	// maxTextRows bounds the agent and heavy-workload rows in a text summary.
	maxTextRows = 20
)

// tool is one entry of tools/list.
type tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations"`
}

// readOnly marks a tool as side-effect free and local.
func readOnly(title string) map[string]any {
	return map[string]any{"title": title, "readOnlyHint": true, "idempotentHint": true, "openWorldHint": false}
}

// toolList returns the tools this server offers. None of them pauses,
// resumes, or changes configuration: control stays a human decision.
func toolList() []tool {
	return []tool{
		{
			Name:  "throt_status",
			Title: "Throt status",
			Description: "Read-only snapshot of local machine pressure from throtd: pressure level and reason, " +
				"CPU, memory, swap and foreground latency, coding agents with their CPU, memory and child counts, " +
				"heavy workloads (builds, tests, container builds) with owner and state (running, PAUSED, QUEUED), " +
				"and the heavy-slot scheduler mode and limit.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Annotations: readOnly("Throt status"),
		},
		{
			Name:  "throt_capacity",
			Title: "Throt capacity check",
			Description: "Advisory check before starting a heavy job (build, test suite, container build): " +
				"returns ok_to_start, a reason, current pressure, running heavy workloads versus the scheduler's " +
				"heavy limit, and a suggestion. Not OK under high or critical pressure, or when the scheduler is on " +
				"and every heavy slot is taken. Unknown pressure is OK with a caveat. Pass your agent label " +
				"(as shown by throt_status, e.g. \"claude-1\") to also get your own running heavy count. " +
				"This only advises; it reserves nothing and controls nothing.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","minLength":1,"maxLength":128,` +
				`"description":"Optional agent instance label, e.g. \"claude-1\", to report that agent's running heavy workloads."}},` +
				`"additionalProperties":false}`),
			Annotations: readOnly("Throt capacity check"),
		},
		{
			Name:  "throt_events",
			Title: "Throt events",
			Description: "Read-only list of the newest throtd events (pressure changes, agents discovered or lost, " +
				"scheduler decisions, pauses and resumes), oldest first, one per line.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":200,"default":20,` +
				`"description":"How many of the newest events to return."}},"additionalProperties":false}`),
			Annotations: readOnly("Throt events"),
		},
		{
			Name:  "throt_stats",
			Title: "Throt usage stats",
			Description: "Read-only per-agent resource accounting since throtd started: for each coding agent instance " +
				"(label and PID), CPU time used by it and its child processes, peak memory, heavy jobs started, and time its " +
				"heavy jobs spent queued by the scheduler, most CPU time first, plus all-agent and unattributed totals. " +
				"Answers which agent cost the machine the most.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Annotations: readOnly("Throt usage stats"),
		},
	}
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

func textResult(text string, structured any) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: text}}, StructuredContent: structured}
}

func errorResult(text string) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: text}}, IsError: true}
}

// daemonDown explains how to recover when throtd cannot be reached. Throt
// fails open, so the agent may proceed without it.
func daemonDown(err error) toolResult {
	return errorResult("throtd is not reachable: " + err.Error() + "\n" +
		"Start the daemon with `throtd &` (check with `throt ping`), then retry. " +
		"Throt is advisory: without it, proceed as you would normally.")
}

// callTool runs a tool. Unknown tools and malformed params are protocol
// errors; bad arguments and daemon failures are tool errors the model can
// read and act on.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: expected {\"name\": string, \"arguments\": object}"}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	switch p.Name {
	case "throt_status":
		var a struct{}
		if err := decodeArgs(p.Arguments, &a); err != nil {
			return errorResult(err.Error()), nil
		}
		return s.status(ctx), nil
	case "throt_capacity":
		var a struct {
			Agent string `json:"agent"`
		}
		if err := decodeArgs(p.Arguments, &a); err != nil {
			return errorResult(err.Error()), nil
		}
		return s.capacity(ctx, a.Agent), nil
	case "throt_stats":
		var a struct{}
		if err := decodeArgs(p.Arguments, &a); err != nil {
			return errorResult(err.Error()), nil
		}
		return s.stats(ctx), nil
	case "throt_events":
		var a struct {
			Limit *int `json:"limit"`
		}
		if err := decodeArgs(p.Arguments, &a); err != nil {
			return errorResult(err.Error()), nil
		}
		limit := defaultEventLimit
		if a.Limit != nil {
			limit = *a.Limit
		}
		if limit < 1 || limit > maxEventLimit {
			return errorResult(fmt.Sprintf("invalid arguments: limit must be between 1 and %d", maxEventLimit)), nil
		}
		return s.events(ctx, limit), nil
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
}

// decodeArgs decodes tool arguments strictly; absent or null means none.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// statusView is the structured content of throt_status.
type statusView struct {
	Pressure       string         `json:"pressure"`
	PressureReason string         `json:"pressure_reason,omitempty"`
	Degraded       string         `json:"degraded,omitempty"`
	Host           *hostView      `json:"host"`
	Agents         []agentView    `json:"agents"`
	Heavy          []workloadView `json:"heavy"`
	Scheduler      schedulerView  `json:"scheduler"`
}

// hostView holds host metrics in human units; "n/a" when unavailable.
type hostView struct {
	CPU     string `json:"cpu"`
	Load    string `json:"load"`
	Memory  string `json:"memory"`
	Swap    string `json:"swap"`
	Latency string `json:"latency"`
}

type agentView struct {
	Agent      string `json:"agent"`
	PID        int    `json:"pid"`
	CPUPercent int    `json:"cpu_percent"`
	Memory     string `json:"memory"`
	Children   int    `json:"children"`
}

type workloadView struct {
	Owner      string `json:"owner"`
	PID        int    `json:"pid"`
	Name       string `json:"name"`
	Class      string `json:"class"`
	State      string `json:"state"`
	CPUPercent int    `json:"cpu_percent"`
	Memory     string `json:"memory"`
}

type schedulerView struct {
	Mode       string `json:"mode"`
	HeavyLimit int    `json:"heavy_limit"`
	Queued     int    `json:"queued"`
}

func newStatusView(st app.StatusResult) statusView {
	v := statusView{
		Pressure:       st.Pressure.String(),
		PressureReason: st.Reason,
		Degraded:       st.Degraded,
		Agents:         []agentView{},
		Heavy:          []workloadView{},
		Scheduler: schedulerView{
			Mode:       string(st.Scheduler.Mode),
			HeavyLimit: st.Scheduler.HeavyLimit,
			Queued:     len(st.Scheduler.Queued),
		},
	}
	if h := st.Host; h != nil {
		na := func(m domain.Metric, s string) string {
			if !h.Has(m) {
				return "n/a"
			}
			return s
		}
		v.Host = &hostView{
			CPU:     na(domain.MetricCPU, fmt.Sprintf("%.0f%%", h.CPUUsage*100)),
			Load:    na(domain.MetricLoad, fmt.Sprintf("%.2f per CPU", h.Load1)),
			Memory:  na(domain.MetricMemory, fmt.Sprintf("%s used of %s", bytesHuman(h.MemTotal-min(h.MemAvailable, h.MemTotal)), bytesHuman(h.MemTotal))),
			Swap:    na(domain.MetricSwap, fmt.Sprintf("%s used of %s", bytesHuman(h.SwapUsed), bytesHuman(h.SwapTotal))),
			Latency: na(domain.MetricLatency, h.Latency.Round(time.Millisecond).String()),
		}
	}
	for _, a := range st.Agents {
		v.Agents = append(v.Agents, agentView{
			Agent: a.Agent, PID: a.Key.PID, CPUPercent: percent(a.CPU), Memory: bytesHuman(a.RSS), Children: a.Children,
		})
	}
	states := capacity.WorkloadStates(st)
	for _, h := range st.Heavy {
		owner := h.Owner
		if owner == "" {
			owner = "-"
		}
		v.Heavy = append(v.Heavy, workloadView{
			Owner: owner, PID: h.Key.PID, Name: h.Name, Class: string(h.Class),
			State: states[h.Key], CPUPercent: percent(h.CPU), Memory: bytesHuman(h.RSS),
		})
	}
	return v
}

func (s *Server) status(ctx context.Context) toolResult {
	st, err := s.client.Status(ctx)
	if err != nil {
		return daemonDown(err)
	}
	v := newStatusView(st)
	var b strings.Builder
	fmt.Fprintf(&b, "pressure: %s", v.Pressure)
	if v.PressureReason != "" {
		fmt.Fprintf(&b, " (%s)", v.PressureReason)
	}
	b.WriteString("\n")
	if v.Degraded != "" {
		fmt.Fprintf(&b, "degraded: %s\n", v.Degraded)
	}
	if h := v.Host; h != nil {
		fmt.Fprintf(&b, "cpu: %s, load: %s\nmemory: %s\nswap: %s\nlatency: %s\n", h.CPU, h.Load, h.Memory, h.Swap, h.Latency)
	} else {
		b.WriteString("host: no sample yet\n")
	}
	fmt.Fprintf(&b, "scheduler: %s (heavy limit %d, %d queued)\n", v.Scheduler.Mode, v.Scheduler.HeavyLimit, v.Scheduler.Queued)
	fmt.Fprintf(&b, "agents (%d):\n", len(v.Agents))
	for i, a := range v.Agents {
		if i == maxTextRows {
			fmt.Fprintf(&b, "  ... and %d more\n", len(v.Agents)-maxTextRows)
			break
		}
		fmt.Fprintf(&b, "  %s pid %d: cpu %d%%, mem %s, %d children\n", a.Agent, a.PID, a.CPUPercent, a.Memory, a.Children)
	}
	fmt.Fprintf(&b, "heavy workloads (%d):\n", len(v.Heavy))
	for i, h := range v.Heavy {
		if i == maxTextRows {
			fmt.Fprintf(&b, "  ... and %d more\n", len(v.Heavy)-maxTextRows)
			break
		}
		fmt.Fprintf(&b, "  %s pid %d %s (%s): %s, cpu %d%%, mem %s\n", h.Owner, h.PID, h.Name, h.Class, h.State, h.CPUPercent, h.Memory)
	}
	return textResult(strings.TrimSuffix(b.String(), "\n"), v)
}

func (s *Server) capacity(ctx context.Context, agent string) toolResult {
	st, err := s.client.Status(ctx)
	if err != nil {
		return daemonDown(err)
	}
	if agent == "" {
		// The model can't know its own label; the agent that launched
		// this server can be found by PID.
		for _, a := range st.Agents {
			if a.Key.PID == s.parent {
				agent = a.Agent
			}
		}
	}
	c := capacity.Assess(st, agent)
	verdict := "NO"
	if c.OKToStart {
		verdict = "YES"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ok to start a heavy job: %s\nreason: %s\n", verdict, c.Reason)
	fmt.Fprintf(&b, "running heavy: %d (limit %d, scheduler %s)\n", c.RunningHeavy, c.HeavyLimit, c.SchedulerMode)
	if c.AgentRunningHeavy != nil {
		fmt.Fprintf(&b, "running heavy for %s: %d\n", c.Agent, *c.AgentRunningHeavy)
	}
	fmt.Fprintf(&b, "suggestion: %s\n(advisory only: throt reserves nothing for you)", c.Suggestion)
	return textResult(b.String(), c)
}

func (s *Server) events(ctx context.Context, limit int) toolResult {
	events, err := s.client.Events(ctx, limit)
	if err != nil {
		return daemonDown(err)
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	if len(events) == 0 {
		return textResult("no events", map[string]any{"events": []domain.Event{}})
	}
	lines := make([]string, 0, len(events))
	for _, e := range events {
		line := e.Time.Format(time.RFC3339) + "  " + string(e.Kind)
		if e.Message != "" {
			line += "  " + e.Message
		}
		lines = append(lines, line)
	}
	return textResult(strings.Join(lines, "\n"), map[string]any{"events": events})
}

// statsView is the structured content of throt_stats.
type statsView struct {
	Since        string         `json:"since,omitempty"`
	Agents       []statsRowView `json:"agents"`
	Total        statsRowView   `json:"total"`
	Unattributed statsRowView   `json:"unattributed"`
}

type statsRowView struct {
	Agent         string  `json:"agent"`
	PID           int     `json:"pid,omitempty"`
	CPUSeconds    float64 `json:"cpu_seconds"`
	PeakMemory    string  `json:"peak_memory"`
	HeavyJobs     int     `json:"heavy_jobs"`
	QueuedSeconds float64 `json:"queued_seconds"`
	ActiveSeconds float64 `json:"active_seconds"`
}

func newStatsRow(label string, pid int, r stats.Row) statsRowView {
	round := func(s float64) float64 { return math.Round(s*10) / 10 }
	return statsRowView{
		Agent: label, PID: pid, CPUSeconds: round(r.CPUSeconds), PeakMemory: bytesHuman(r.PeakRSS),
		HeavyJobs: r.HeavyStarted, QueuedSeconds: round(r.QueuedSeconds), ActiveSeconds: round(r.ActiveFor().Seconds()),
	}
}

func newStatsView(r app.StatsResult) statsView {
	v := statsView{
		Agents:       make([]statsRowView, 0, len(r.Agents)),
		Total:        newStatsRow("all agents", 0, r.Total),
		Unattributed: newStatsRow("unattributed", 0, r.Unattributed),
	}
	if !r.Since.IsZero() {
		v.Since = r.Since.Format(time.RFC3339)
	}
	for _, a := range r.Agents {
		v.Agents = append(v.Agents, newStatsRow(a.Agent, a.Process.PID, a))
	}
	return v
}

func (s *Server) stats(ctx context.Context) toolResult {
	r, err := s.client.Stats(ctx)
	if err != nil {
		return daemonDown(err)
	}
	v := newStatsView(r)
	if v.Since == "" {
		return textResult("no samples yet", v)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "since %s (in memory; resets when throtd restarts)\n", v.Since)
	line := func(r statsRowView) {
		name := r.Agent
		if r.PID != 0 {
			name += fmt.Sprintf(" pid %d", r.PID)
		}
		fmt.Fprintf(&b, "  %s: cpu %s, peak mem %s, %d heavy jobs, queued %s, active %s\n", name,
			secs(r.CPUSeconds), r.PeakMemory, r.HeavyJobs, secs(r.QueuedSeconds), secs(r.ActiveSeconds))
	}
	fmt.Fprintf(&b, "agents (%d, most cpu first):\n", len(v.Agents))
	for i, a := range v.Agents {
		if i == maxTextRows {
			fmt.Fprintf(&b, "  ... and %d more\n", len(v.Agents)-maxTextRows)
			break
		}
		line(a)
	}
	b.WriteString("totals:\n")
	line(v.Total)
	line(v.Unattributed)
	return textResult(strings.TrimSuffix(b.String(), "\n"), v)
}

func secs(s float64) string {
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

func percent(cores float64) int { return int(cores*100 + 0.5) }

func bytesHuman(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}
