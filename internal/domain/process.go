package domain

import "time"

// ProcessSample is one observation of a process. Only the executable's base
// name is kept: arguments, environment, and paths can contain secrets.
type ProcessSample struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	UID  uint32 `json:"uid"`
	// Start is when the process started. Together with PID it identifies a
	// process across samples and defends against PID reuse.
	Start time.Time `json:"start"`
	// Name is the executable's base name, e.g. "go" or "node".
	Name string `json:"name"`
	// CPUTime is cumulative user+system CPU time.
	CPUTime time.Duration `json:"cpu_time"`
	// RSS is resident memory in bytes.
	RSS uint64 `json:"rss"`
	// State is the scheduling state; "" when the platform does not report it.
	State ProcessState `json:"state,omitempty"`
}

// ProcessState is a process's scheduling state, reduced to what Throt acts
// on. Only StateStopped matters for control: a process stopped by a signal
// (SIGSTOP, or SIGTSTP from Ctrl-Z). A traced process (Linux "t") is
// StateOther, never StateStopped.
type ProcessState string

const (
	StateRunning  ProcessState = "running"
	StateSleeping ProcessState = "sleeping"
	StateStopped  ProcessState = "stopped"
	StateZombie   ProcessState = "zombie"
	StateOther    ProcessState = "other"
)

// ProcessKey identifies one process instance; a reused PID has a new Start.
type ProcessKey struct {
	PID   int       `json:"pid"`
	Start time.Time `json:"start"`
}

// Key returns the sample's identity.
func (p ProcessSample) Key() ProcessKey { return ProcessKey{PID: p.PID, Start: p.Start} }

// WorkloadClass labels what a process is doing.
type WorkloadClass string

const (
	ClassUnknown        WorkloadClass = "unknown"
	ClassAgent          WorkloadClass = "agent"
	ClassBuild          WorkloadClass = "build"
	ClassTest           WorkloadClass = "test"
	ClassContainerBuild WorkloadClass = "container-build"
	// ClassCommand is a busy command an agent ran through a shell (one tool
	// call), whatever its executable (see process.Graph).
	ClassCommand WorkloadClass = "command"
)

// Heavy reports whether the class is an expensive workload the scheduler
// may eventually coordinate.
func (c WorkloadClass) Heavy() bool {
	return c == ClassBuild || c == ClassTest || c == ClassContainerBuild || c == ClassCommand
}

const (
	EventProcessDiscovered EventKind = "process_discovered"
	EventProcessLost       EventKind = "process_lost"
	EventAgentDiscovered   EventKind = "agent_discovered"
	EventAgentLost         EventKind = "agent_lost"
	// EventOrphanStopped reports a stopped, attributed heavy workload that
	// no paused record explains, e.g. one left by a throtd killed hard.
	EventOrphanStopped EventKind = "orphan_stopped"
)
