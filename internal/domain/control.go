package domain

import "errors"

// ErrProcessGone reports that a process exited, or its PID now belongs to a
// different process, between observation and action.
var ErrProcessGone = errors.New("process is gone")

const (
	EventControlPaused  EventKind = "control_paused"
	EventControlResumed EventKind = "control_resumed"
	EventControlRefused EventKind = "control_refused"
	EventControlFailed  EventKind = "control_failed"
	// EventSchedulerDecision records every scheduler decision, including
	// observe-only ones, with its reason code.
	EventSchedulerDecision EventKind = "scheduler_decision"
	// EventSchedulerLimit records adaptive changes to the heavy limit.
	EventSchedulerLimit EventKind = "scheduler_limit"
	// EventLeaseGranted records an admission lease granted to `throt run`;
	// EventLeaseReleased records its end, with a reason: exited,
	// became_workload, or expired.
	EventLeaseGranted  EventKind = "lease_granted"
	EventLeaseReleased EventKind = "lease_released"
)
