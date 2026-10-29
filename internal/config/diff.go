package config

import (
	"strconv"
	"strings"
	"time"
)

// Change is one config key whose value differs between two configs.
type Change struct {
	// Key is the dotted JSON key, e.g. "scheduler.mode".
	Key      string
	From, To string
}

func (c Change) String() string { return c.Key + " " + c.From + " -> " + c.To }

// Diff lists the keys whose values differ from a to b, in a fixed order.
func Diff(a, b Config) []Change {
	var out []Change
	fa, fb := flatten(a), flatten(b)
	for i := range fa {
		if fa[i].Value != fb[i].Value {
			out = append(out, Change{Key: fa[i].Key, From: fa[i].Value, To: fb[i].Value})
		}
	}
	return out
}

// keyValue is one config key and its value rendered as text.
type keyValue struct{ Key, Value string }

// flatten renders every key of c, in the fixed order Diff uses.
func flatten(c Config) []keyValue {
	dur := func(d Duration) string { return time.Duration(d).String() }
	s := c.Scheduler
	return []keyValue{
		{"socket_path", c.SocketPath},
		{"sample_interval", dur(c.SampleInterval)},
		{"event_history", strconv.Itoa(c.EventHistory)},
		{"profile", c.Profile},
		{"scheduler.mode", s.Mode},
		{"scheduler.heavy_limit", strconv.Itoa(s.HeavyLimit)},
		{"scheduler.start_limit", strconv.Itoa(s.StartLimit)},
		{"scheduler.resume_cooldown", dur(s.ResumeCooldown)},
		{"scheduler.pause_on_critical", strconv.FormatBool(s.PauseOnCritical)},
		{"scheduler.max_queue_wait", dur(s.MaxQueueWait)},
		{"scheduler.adaptive", strconv.FormatBool(s.Adaptive)},
		{"scheduler.per_agent_limit", strconv.Itoa(s.PerAgentLimit)},
		{"scheduler.background", strconv.FormatBool(s.Background)},
		{"agents", agentsString(c.Agents)},
	}
}

// agentsString renders agents compactly, e.g. "[my(a,b) other(c)]".
func agentsString(agents []Agent) string {
	parts := make([]string, len(agents))
	for i, a := range agents {
		parts[i] = a.Name + "(" + strings.Join(a.Executables, ",") + ")"
	}
	return "[" + strings.Join(parts, " ") + "]"
}
