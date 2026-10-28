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
	add := func(key, from, to string) {
		if from != to {
			out = append(out, Change{Key: key, From: from, To: to})
		}
	}
	dur := func(d Duration) string { return time.Duration(d).String() }
	add("socket_path", a.SocketPath, b.SocketPath)
	add("sample_interval", dur(a.SampleInterval), dur(b.SampleInterval))
	add("event_history", strconv.Itoa(a.EventHistory), strconv.Itoa(b.EventHistory))
	add("profile", a.Profile, b.Profile)
	sa, sb := a.Scheduler, b.Scheduler
	add("scheduler.mode", sa.Mode, sb.Mode)
	add("scheduler.heavy_limit", strconv.Itoa(sa.HeavyLimit), strconv.Itoa(sb.HeavyLimit))
	add("scheduler.resume_cooldown", dur(sa.ResumeCooldown), dur(sb.ResumeCooldown))
	add("scheduler.pause_on_critical", strconv.FormatBool(sa.PauseOnCritical), strconv.FormatBool(sb.PauseOnCritical))
	add("scheduler.max_queue_wait", dur(sa.MaxQueueWait), dur(sb.MaxQueueWait))
	add("scheduler.adaptive", strconv.FormatBool(sa.Adaptive), strconv.FormatBool(sb.Adaptive))
	add("scheduler.per_agent_limit", strconv.Itoa(sa.PerAgentLimit), strconv.Itoa(sb.PerAgentLimit))
	add("agents", agentsString(a.Agents), agentsString(b.Agents))
	return out
}

// agentsString renders agents compactly, e.g. "[my(a,b) other(c)]".
func agentsString(agents []Agent) string {
	parts := make([]string, len(agents))
	for i, a := range agents {
		parts[i] = a.Name + "(" + strings.Join(a.Executables, ",") + ")"
	}
	return "[" + strings.Join(parts, " ") + "]"
}
