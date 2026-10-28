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
)
