package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/control"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/scheduler"
	"github.com/cfcromn/Throt/internal/transport"
)

// explainEvents is how many events about the workload explain shows.
const explainEvents = 10

// cmdExplain is 'throt explain PID'.
func cmdExplain(ctx context.Context, e env, args []string) error {
	pid, err := pidArg(args, "usage: throt explain PID")
	if err != nil {
		return err
	}
	cfg := e.clientConfig()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var st app.StatusResult
	if err := transport.Call(ctx, cfg.SocketPath, "status", nil, &st); err != nil {
		return err
	}
	var events []domain.Event
	if err := transport.Call(ctx, cfg.SocketPath, "events", app.EventsParams{Limit: app.MaxEventsPerCall}, &events); err != nil {
		return err
	}
	return explain(e.stdout, pid, st, events, time.Duration(cfg.Scheduler.ResumeCooldown))
}

// explain writes why the heavy workload pid is running, paused or queued,
// using only what the daemon already reports. cooldown is the local
// config's resume_cooldown; the daemon does not report it.
func explain(w io.Writer, pid int, st app.StatusResult, events []domain.Event, cooldown time.Duration) error {
	i := slices.IndexFunc(st.Heavy, func(h process.WorkloadView) bool { return h.Key.PID == pid })
	if i < 0 {
		return notHeavy(pid, st.Heavy)
	}
	h := st.Heavy[i]
	queued := slices.Contains(st.Scheduler.Queued, h.Key)
	var paused *control.PausedWorkload
	for i := range st.Paused {
		if st.Paused[i].Key == h.Key {
			paused = &st.Paused[i]
		}
	}
	state := "running"
	switch {
	case queued:
		state = "QUEUED by scheduler"
	case paused != nil && paused.State == control.StateResuming:
		state = "RESUMING (paused, resume in progress)"
	case paused != nil:
		state = "PAUSED by you"
	}
	owner := "unattributed (observe-only)"
	if h.Owner != "" {
		owner = fmt.Sprintf("%s (pid %d)", h.Owner, h.OwnerPID)
	}
	sc := st.Scheduler
	limits := limitText(sc)
	if sc.PerAgentLimit > 0 {
		limits += fmt.Sprintf(", per-agent limit %d", sc.PerAgentLimit)
	}

	fmt.Fprintf(w, "%s (pid %d, %s)\n", h.Name, pid, h.Class)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "state\t%s\n", state)
	fmt.Fprintf(tw, "owner\t%s\n", owner)
	// With the scheduler off the limit is not enforced, so a running/limit
	// ratio would only mislead.
	if modeName(sc.Mode) == string(scheduler.ModeOff) {
		fmt.Fprintf(tw, "scheduler\t%s, %s\n", modeName(sc.Mode), limits)
	} else {
		fmt.Fprintf(tw, "scheduler\t%s, %s, %d/%d heavy running\n", modeName(sc.Mode), limits, running(st), sc.HeavyLimit)
	}
	fmt.Fprintf(tw, "pressure\t%s\n", st.Pressure)
	tw.Flush()

	mine := workloadEvents(events, h.Key)
	fmt.Fprintln(w, "\nRECENT EVENTS")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tKIND\tACTION\tREASON\tPRESSURE\tSLOTS")
	if len(mine) == 0 {
		fmt.Fprintln(tw, "(none in the daemon's event history)")
	}
	for _, e := range mine {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clock(e.Time), e.Kind,
			attr(e, "decision", "action"), attr(e, "reason"), attr(e, "pressure"), slots(e))
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, summary(h, st, queued, paused, mine, cooldown))
	return nil
}

func notHeavy(pid int, heavy []process.WorkloadView) error {
	if len(heavy) == 0 {
		return fmt.Errorf("pid %d is not a heavy workload in the latest sample; there are no heavy workloads right now", pid)
	}
	pids := make([]string, len(heavy))
	for i, h := range heavy {
		pids[i] = fmt.Sprintf("%d (%s)", h.Key.PID, h.Name)
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

// summary is the one-line answer to "why is this in its state?".
func summary(h process.WorkloadView, st app.StatusResult, queued bool, paused *control.PausedWorkload, mine []domain.Event, cooldown time.Duration) string {
	pid := h.Key.PID
	last := lastDecision(mine)
	switch {
	case queued:
		if last == nil || (last.Attrs["decision"] != string(scheduler.ActionQueue) && last.Attrs["decision"] != string(scheduler.ActionPreempt)) {
			return fmt.Sprintf("Queued by the scheduler; the decision is no longer in the event history. It resumes when a slot frees (cooldown %s).", cooldown)
		}
		return queuedSummary(*last, h, st, cooldown)
	case paused != nil && paused.State == control.StateResuming:
		return "Being resumed: a resume did not reach every process yet, and throtd retries it every sample."
	case paused != nil:
		return fmt.Sprintf("Paused by you at %s; the scheduler never resumes manual pauses, so it stays paused until `throt resume %d`.", clock(paused.Since), pid)
	case h.Owner == "":
		return "Running; it is not attributed to an agent, so throt never pauses it (it still counts toward the heavy limit)."
	}
	switch st.Scheduler.Mode {
	case scheduler.ModeEnforce:
	case scheduler.ModeObserve:
		if last != nil && last.Attrs["observed"] == "true" && last.Attrs["decision"] != string(scheduler.ActionResume) {
			return fmt.Sprintf("Running; at %s the scheduler would have %s it (%s), but observe mode never acts.", clock(last.Time), past(last.Attrs["decision"]), reasonText(last.Attrs["reason"]))
		}
		return "Running; the scheduler is in observe mode, so it logs decisions but never pauses work."
	default:
		return fmt.Sprintf("Running; the scheduler is off, so only `throt pause %d` would stop it.", pid)
	}
	if last != nil && last.Attrs["decision"] == string(scheduler.ActionResume) {
		return fmt.Sprintf("Running since the scheduler resumed it at %s (%s).", clock(last.Time), reasonText(last.Attrs["reason"]))
	}
	return fmt.Sprintf("Running in one of %d/%d heavy slots under %s pressure; the scheduler never pauses running work, except to preempt under critical pressure when pause_on_critical is set.",
		running(st), st.Scheduler.HeavyLimit, st.Pressure)
}

func queuedSummary(e domain.Event, h process.WorkloadView, st app.StatusResult, cooldown time.Duration) string {
	verb := "Queued"
	if e.Attrs["decision"] == string(scheduler.ActionPreempt) {
		verb = "Preempted"
	}
	at, pressure := clock(e.Time), e.Attrs["pressure"]
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
	return fmt.Sprintf("%s at %s because %s; it resumes %s (cooldown %s).", verb, at, why, until, cooldown)
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

// running counts heavy workloads that are neither queued nor paused.
func running(st app.StatusResult) int {
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

func modeName(m scheduler.Mode) string {
	if m == "" {
		return string(scheduler.ModeOff)
	}
	return string(m)
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

func clock(t time.Time) string { return t.Local().Format("15:04:05") }
