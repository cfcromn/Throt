package process

import (
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// StoppedView is one stopped process as of the latest Update.
type StoppedView struct {
	Key   domain.ProcessKey    `json:"process"`
	UID   uint32               `json:"uid"`
	Name  string               `json:"name"`
	Class domain.WorkloadClass `json:"class"`
	// Owner is the owning agent instance label, or "" when unattributed.
	Owner string `json:"owner,omitempty"`
	// Parent is the parent's executable name, "" when the parent is unknown.
	Parent string `json:"parent,omitempty"`
	// ParentOwned reports whether the parent belongs to an agent, so a shell
	// parent is an agent's command shell rather than the user's terminal.
	ParentOwned bool `json:"parent_owned,omitempty"`
	// ParentKey identifies the parent; zero when the parent is unknown.
	// ClassifyOrphan follows it through stopped ancestors.
	ParentKey domain.ProcessKey `json:"parent_process,omitzero"`
	// Since is the first Update that saw the process stopped. When the
	// process was already stopped at the graph's first Update, it is only a
	// lower bound.
	Since time.Time `json:"since"`
}

// MaxOrphans bounds the list FindOrphans returns.
const MaxOrphans = 50

// OrphanKind is FindOrphans' best guess at why a process is stopped.
type OrphanKind string

const (
	// OrphanJobControl: the parent is a shell outside any agent, so the
	// user most likely suspended it with Ctrl-Z.
	OrphanJobControl OrphanKind = "job_control"
	// OrphanLeftPaused: an agent's process or a heavy workload, the kind
	// throtd pauses, so an earlier throtd most likely left it stopped.
	OrphanLeftPaused OrphanKind = "left_paused"
	// OrphanUnknown: something else stopped it.
	OrphanUnknown OrphanKind = "unknown"
)

// Orphan is a stopped process of the user that no paused record explains.
// Throt only reports it; it never resumes one on its own.
type Orphan struct {
	Key   domain.ProcessKey    `json:"process"`
	Name  string               `json:"name"`
	Class domain.WorkloadClass `json:"class"`
	Owner string               `json:"owner,omitempty"`
	Kind  OrphanKind           `json:"kind"`
	Hint  string               `json:"hint"`
	// Since is when it was first observed stopped (see StoppedView.Since).
	Since time.Time `json:"since,omitzero"`
}

// AttributedHeavy reports whether the orphan is a heavy workload process of
// an agent: the kind throtd pauses, which `throt resume` may resume.
func (o Orphan) AttributedHeavy() bool { return o.Owner != "" && o.Class.Heavy() }

// isShell reports whether name is an interactive shell, whose stopped
// children are most likely jobs suspended with Ctrl-Z. A login shell's
// argv[0] has a leading '-' ("-zsh"), which macOS ps reports as its name.
func isShell(name string) bool {
	switch strings.TrimPrefix(name, "-") {
	case "zsh", "bash", "fish", "sh":
		return true
	}
	return false
}

// ClassifyOrphan explains one stopped process; stopped is every stopped
// process of the same snapshot. Ctrl-Z stops a shell job's whole process
// group, so a process is job control when it, or any ancestor reached
// through stopped processes only, has a shell parent outside every agent:
// the children of a suspended agent or terminal make are part of the job,
// not throtd pauses, even though their own parent is the agent or make. A
// shell job wins over attribution. An agent's own command shell (bash -c
// make) is not job control, and a stopped child of a running agent is not
// part of a job.
func ClassifyOrphan(v StoppedView, stopped []StoppedView) Orphan {
	return classify(v, stoppedByKey(stopped))
}

func stoppedByKey(stopped []StoppedView) map[domain.ProcessKey]StoppedView {
	m := make(map[domain.ProcessKey]StoppedView, len(stopped))
	for _, s := range stopped {
		m[s.Key] = s
	}
	return m
}

// inShellJob walks from v up through stopped ancestors and reports whether
// one of them has a shell parent outside every agent. The walk is bounded
// by the number of stopped processes, so a malformed cycle terminates.
func inShellJob(v StoppedView, stopped map[domain.ProcessKey]StoppedView) bool {
	for range len(stopped) + 1 {
		if isShell(v.Parent) && !v.ParentOwned {
			return true
		}
		p, ok := stopped[v.ParentKey]
		if v.ParentKey == (domain.ProcessKey{}) || !ok {
			return false
		}
		v = p
	}
	return false
}

func classify(v StoppedView, stopped map[domain.ProcessKey]StoppedView) Orphan {
	o := Orphan{Key: v.Key, Name: v.Name, Class: v.Class, Owner: v.Owner, Since: v.Since}
	pid := strconv.Itoa(v.Key.PID)
	switch {
	case inShellJob(v, stopped):
		o.Kind, o.Hint = OrphanJobControl, "likely job control (Ctrl-Z); resume the job with fg in its shell"
	case o.AttributedHeavy():
		o.Kind, o.Hint = OrphanLeftPaused, "likely left paused by an earlier throtd (kill -CONT "+pid+" or throt resume "+pid+")"
	case v.Owner != "" || v.Class.Heavy():
		o.Kind, o.Hint = OrphanLeftPaused, "likely left paused by an earlier throtd (kill -CONT "+pid+" or throt resume --force "+pid+")"
	default:
		o.Kind, o.Hint = OrphanUnknown, "stopped by another program (kill -CONT "+pid+" resumes it)"
	}
	return o
}

// FindOrphans returns the stopped processes owned by uid that recorded does
// not claim (recorded reports keys in a paused record, members included),
// in stopped's order, at most MaxOrphans of them, and how many there are in
// total. It is pure: it reads only its arguments.
func FindOrphans(stopped []StoppedView, uid uint32, recorded func(domain.ProcessKey) bool) (orphans []Orphan, total int) {
	byKey := stoppedByKey(stopped)
	for _, v := range stopped {
		if v.UID != uid || recorded(v.Key) {
			continue
		}
		total++
		if len(orphans) < MaxOrphans {
			orphans = append(orphans, classify(v, byKey))
		}
	}
	return orphans, total
}
