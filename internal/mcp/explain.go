package mcp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// explainEvents is how many events about the workload throt_explain shows,
// as `throt explain` does.
const explainEvents = 10

// explainView is the structured content of throt_explain. It carries what
// `throt explain PID` prints, computed from the same daemon status and
// events. The local resume_cooldown is not reported by the daemon, so the
// summary names it without a value.
type explainView struct {
	PID       int                `json:"pid"`
	Name      string             `json:"name"`
	Class     string             `json:"class"`
	State     string             `json:"state"`
	Owner     string             `json:"owner,omitempty"`
	OwnerPID  int                `json:"owner_pid,omitempty"`
	Scheduler explainSchedView   `json:"scheduler"`
	Pressure  string             `json:"pressure"`
	Events    []explainEventView `json:"events"`
	Summary   string             `json:"summary"`
}

type explainSchedView struct {
	Mode          string `json:"mode"`
	HeavyLimit    int    `json:"heavy_limit"`
	PerAgentLimit int    `json:"per_agent_limit,omitempty"`
	Running       int    `json:"running"`
}

type explainEventView struct {
	Time     string `json:"time"`
	Kind     string `json:"kind"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	Pressure string `json:"pressure"`
	Slots    string `json:"slots"`
}

// Workload states of throt_explain.
const (
	explainRunning  = "running"
	explainQueued   = "QUEUED by scheduler"
	explainResuming = "RESUMING (paused, resume in progress)"
	explainPaused   = "PAUSED by the user"
)

func (s *Server) explain(ctx context.Context, pid int) toolResult {
	st, err := s.client.Status(ctx)
	if err != nil {
		return daemonDown(err)
	}
	events, err := s.client.Events(ctx, app.MaxEventsPerCall)
	if err != nil {
		return daemonDown(err)
	}
	v, err := newExplainView(pid, st, events)
	if err != nil {
		return errorResult(err.Error())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (pid %d, %s)\nstate: %s\n", v.Name, v.PID, v.Class, v.State)
	if v.Owner == "" {
		b.WriteString("owner: unattributed (observe-only)\n")
	} else {
		fmt.Fprintf(&b, "owner: %s (pid %d)\n", v.Owner, v.OwnerPID)
	}
	limits := fmt.Sprintf("heavy limit %d", v.Scheduler.HeavyLimit)
	if v.Scheduler.PerAgentLimit > 0 {
		limits += fmt.Sprintf(", per-agent limit %d", v.Scheduler.PerAgentLimit)
	}
	fmt.Fprintf(&b, "scheduler: %s, %s, %d/%d heavy running\npressure: %s\n", v.Scheduler.Mode, limits,
		v.Scheduler.Running, v.Scheduler.HeavyLimit, v.Pressure)
	b.WriteString("recent events (time, kind, action, reason, pressure, slots):\n")
	if len(v.Events) == 0 {
		b.WriteString("  (none in the daemon's event history)\n")
	}
	for _, e := range v.Events {
		fmt.Fprintf(&b, "  %s  %s  %s  %s  %s  %s\n", e.Time, e.Kind, e.Action, e.Reason, e.Pressure, e.Slots)
	}
	b.WriteString(v.Summary)
	return textResult(b.String(), v)
}

// newExplainView explains the heavy workload pid. It mirrors `throt
// explain`, which lives in cmd/throt.
func newExplainView(pid int, st app.StatusResult, events []domain.Event) (explainView, error) {
	i := slices.IndexFunc(st.Heavy, func(h process.WorkloadView) bool { return h.Key.PID == pid })
	if i < 0 {
		return explainView{}, notHeavy(pid, st.Heavy)
	}
	h := st.Heavy[i]
	queued := slices.Contains(st.Scheduler.Queued, h.Key)
	var paused *control.PausedWorkload
	for i := range st.Paused {
		if st.Paused[i].Key == h.Key {
			paused = &st.Paused[i]
		}
	}
	state := explainRunning
	switch {
	case queued:
		state = explainQueued
	case paused != nil && paused.State == control.StateResuming:
		state = explainResuming
	case paused != nil:
		state = explainPaused
	}
	mode := st.Scheduler.Mode
	if mode == "" {
		mode = scheduler.ModeOff
	}
	mine := workloadEvents(events, h.Key)
	v := explainView{
		PID: pid, Name: h.Name, Class: string(h.Class), State: state, Owner: h.Owner, OwnerPID: h.OwnerPID,
		Scheduler: explainSchedView{
			Mode: string(mode), HeavyLimit: st.Scheduler.HeavyLimit, PerAgentLimit: st.Scheduler.PerAgentLimit,
			Running: unstopped(st),
		},
		Pressure: st.Pressure.String(),
		Events:   make([]explainEventView, 0, len(mine)),
		Summary:  explainSummary(h, st, queued, paused, mine),
	}
	for _, e := range mine {
		v.Events = append(v.Events, explainEventView{
			Time: e.Time.Format(time.RFC3339), Kind: string(e.Kind), Action: attr(e, "decision", "action"),
			Reason: attr(e, "reason"), Pressure: attr(e, "pressure"), Slots: slots(e),
		})
	}
	return v, nil
}

func notHeavy(pid int, heavy []process.WorkloadView) error {
	if len(heavy) == 0 {
		return fmt.Errorf("pid %d is not a heavy workload in the latest sample; there are no heavy workloads right now", pid)
	}
	pids := make([]string, 0, min(len(heavy), maxTextRows))
	for i, h := range heavy {
		if i == maxTextRows {
			pids = append(pids, fmt.Sprintf("and %d more", len(heavy)-maxTextRows))
			break
		}
		pids = append(pids, fmt.Sprintf("%d (%s)", h.Key.PID, h.Name))
	}
	return fmt.Errorf("pid %d is not a heavy workload in the latest sample; heavy workloads: %s", pid, strings.Join(pids, ", "))
}

// workloadEvents returns the last explainEvents events that name key's PID.
// Events from before the process started belong to an earlier process that
// had the same PID, so they are skipped.
func workloadEvents(events []domain.Event, key domain.ProcessKey) []domain.Event {
	pid := strconv.Itoa(key.PID)
	var out []domain.Event
	for _, e := range events {
		// Start times are second-granular; an event in that second counts.
		if e.Attrs["pid"] == pid && !e.Time.Before(key.Start.Truncate(time.Second)) {
			out = append(out, e)
		}
	}
	return out[max(0, len(out)-explainEvents):]
}

// explainSummary is the one-line answer to "why is this in its state?".
func explainSummary(h process.WorkloadView, st app.StatusResult, queued bool, paused *control.PausedWorkload, mine []domain.Event) string {
	pid := h.Key.PID
	last := lastDecision(mine)
	switch {
	case queued:
		if last == nil || (last.Attrs["decision"] != string(scheduler.ActionQueue) && last.Attrs["decision"] != string(scheduler.ActionPreempt)) {
			return "Queued by the scheduler; the decision is no longer in the event history. It resumes when a slot frees (after the resume cooldown)."
		}
		return queuedSummary(*last, h, st)
	case paused != nil && paused.State == control.StateResuming:
		return "Being resumed: a resume did not reach every process yet, and throtd retries it every sample."
	case paused != nil:
		return fmt.Sprintf("Paused by the user at %s; the scheduler never resumes manual pauses, so it stays paused until the user runs `throt resume %d`.",
			paused.Since.Format(time.RFC3339), pid)
	case h.Owner == "":
		return "Running; it is not attributed to an agent, so throt never pauses it (it still counts toward the heavy limit)."
	}
	switch st.Scheduler.Mode {
	case scheduler.ModeEnforce:
	case scheduler.ModeObserve:
		if last != nil && last.Attrs["observed"] == "true" && last.Attrs["decision"] != string(scheduler.ActionResume) {
			return fmt.Sprintf("Running; at %s the scheduler would have %s it (%s), but observe mode never acts.",
				last.Time.Format(time.RFC3339), past(last.Attrs["decision"]), reasonText(last.Attrs["reason"]))
		}
		return "Running; the scheduler is in observe mode, so it logs decisions but never pauses work."
	default:
		return fmt.Sprintf("Running; the scheduler is off, so only `throt pause %d` would stop it.", pid)
	}
	if last != nil && last.Attrs["decision"] == string(scheduler.ActionResume) {
		return fmt.Sprintf("Running since the scheduler resumed it at %s (%s).", last.Time.Format(time.RFC3339), reasonText(last.Attrs["reason"]))
	}
	return fmt.Sprintf("Running in one of %d/%d heavy slots under %s pressure; the scheduler never pauses running work, except to preempt under critical pressure when pause_on_critical is set.",
		unstopped(st), st.Scheduler.HeavyLimit, st.Pressure)
}

func queuedSummary(e domain.Event, h process.WorkloadView, st app.StatusResult) string {
	verb := "Queued"
	if e.Attrs["decision"] == string(scheduler.ActionPreempt) {
		verb = "Preempted"
	}
	pressure := e.Attrs["pressure"]
	var why, until string
	switch e.Attrs["reason"] {
	case scheduler.ReasonSlotsExhausted:
		why = fmt.Sprintf("%s heavy slots were in use under %s pressure", slots(e), pressure)
		until = "when a slot frees"
	case scheduler.ReasonQueueWaiting:
		why = fmt.Sprintf("other work was already waiting for a heavy slot (%s in use, %s pressure)", slots(e), pressure)
		until = "in queue order when a slot frees"
	case scheduler.ReasonPressureHigh:
		why = "high pressure blocks new heavy work"
		until = "when pressure drops, or when a slot is free once it has waited max_queue_wait"
	case scheduler.ReasonPressureCritical:
		why = "pressure was critical"
		until = "when pressure drops and a slot frees"
	case scheduler.ReasonOwnerBudget:
		why = fmt.Sprintf("%s already ran its per-agent limit of %d heavy workloads", h.Owner, st.Scheduler.PerAgentLimit)
		until = "when one of them finishes"
	default:
		why = reasonText(e.Attrs["reason"])
		until = "when the scheduler frees a slot"
	}
	return fmt.Sprintf("%s at %s because %s; it resumes %s (after the resume cooldown).", verb, e.Time.Format(time.RFC3339), why, until)
}

// lastDecision returns the newest scheduler decision in events, or nil.
func lastDecision(events []domain.Event) *domain.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == domain.EventSchedulerDecision {
			return &events[i]
		}
	}
	return nil
}

// unstopped counts heavy workloads that are neither queued nor paused.
func unstopped(st app.StatusResult) int {
	n := 0
	for _, h := range st.Heavy {
		stopped := slices.Contains(st.Scheduler.Queued, h.Key) ||
			slices.ContainsFunc(st.Paused, func(p control.PausedWorkload) bool { return p.Key == h.Key })
		if !stopped {
			n++
		}
	}
	return n
}

// attr returns the first non-empty attribute among names, or "-".
func attr(e domain.Event, names ...string) string {
	for _, n := range names {
		if v := e.Attrs[n]; v != "" {
			return v
		}
	}
	return "-"
}

func slots(e domain.Event) string {
	r, l := e.Attrs["running"], e.Attrs["limit"]
	if r == "" || l == "" {
		return "-"
	}
	return r + "/" + l
}

func reasonText(code string) string {
	if code == "" {
		return "no reason recorded"
	}
	return strings.ReplaceAll(code, "_", " ")
}

func past(decision string) string {
	switch decision {
	case string(scheduler.ActionQueue):
		return "queued"
	case string(scheduler.ActionPreempt):
		return "preempted"
	}
	return decision
}
