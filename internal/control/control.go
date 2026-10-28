// Package control pauses and resumes heavy workloads with verified process
// identity. It is the only place Throt decides to signal a process, so every
// rule here errs on the side of not signaling: a process is stopped only
// when it is attributed, heavy, owned by the daemon's user, and still the
// same process (PID and start time) at the moment of signaling.
//
// The package is deterministic: it makes no OS calls (a Signaler does),
// starts no goroutines, and reads time only from the clock it is given.
package control

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

const (
	// MaxPaused bounds how many workloads may be paused at once.
	MaxPaused = 64
	// MaxMembers bounds how many processes one workload may have.
	MaxMembers = 4096
)

// Refusal reasons. They are stable codes for logs and the local protocol.
const (
	ReasonUnattributed   = "unattributed"
	ReasonNotHeavy       = "not_heavy"
	ReasonNoMembers      = "no_members"
	ReasonTooManyMembers = "too_many_members"
	ReasonInvalidTarget  = "invalid_target"
	ReasonForeignUID     = "foreign_uid"
	ReasonAlreadyPaused  = "already_paused"
	ReasonLimitReached   = "limit_reached"
	ReasonNotPaused      = "not_paused"
)

// RefusedError reports that an operation was refused before any signal was
// sent.
type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string { return "control: refused: " + e.Reason }

func refuse(reason string) error { return &RefusedError{Reason: reason} }

// Signaler is the platform's process-signaling capability.
type Signaler interface {
	// Identity returns the current start time and uid of pid. The error
	// wraps domain.ErrProcessGone when no such process exists.
	Identity(ctx context.Context, pid int) (start time.Time, uid uint32, err error)
	// Stop suspends pid (SIGSTOP). It may wrap domain.ErrProcessGone.
	Stop(pid int) error
	// Continue resumes pid (SIGCONT). It may wrap domain.ErrProcessGone.
	Continue(pid int) error
}

// PausedWorkload records one paused workload. Members lists only the
// processes that were actually stopped, parents before children.
type PausedWorkload struct {
	Key     domain.ProcessKey   `json:"process"`
	Name    string              `json:"name"`
	Owner   string              `json:"owner"`
	Members []domain.ProcessKey `json:"members"`
	Since   time.Time           `json:"since"`
}

func (p PausedWorkload) clone() PausedWorkload {
	p.Members = slices.Clone(p.Members)
	return p
}

// Controller pauses and resumes workloads owned by one user. It is safe for
// concurrent use; operations are serialized so pause and resume never
// interleave.
type Controller struct {
	sig Signaler
	uid uint32
	now func() time.Time

	mu     sync.Mutex
	paused map[domain.ProcessKey]PausedWorkload
}

// New returns a controller that only signals processes owned by uid.
func New(sig Signaler, uid uint32, now func() time.Time) *Controller {
	return &Controller{sig: sig, uid: uid, now: now, paused: map[domain.ProcessKey]PausedWorkload{}}
}

// Pause stops every member of t, parents before children, after checking
// eligibility. Each member's identity is verified immediately before it is
// signaled; members that are gone or no longer match are skipped, and if
// the top process is gone nothing is paused. If a Stop fails or ctx is
// cancelled, members already stopped are continued in reverse order.
func (c *Controller) Pause(ctx context.Context, t process.WorkloadTarget) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.eligible(t); err != nil {
		return err
	}

	var stopped []domain.ProcessKey
	fail := func(cause error) error {
		if errs := c.continueAll(ctx, stopped); len(errs) > 0 {
			return fmt.Errorf("control: pause of %s failed and rollback was incomplete: %w",
				keyString(t.Key), errors.Join(append([]error{cause}, errs...)...))
		}
		return fmt.Errorf("control: pause of %s failed, rolled back %d stopped processes: %w",
			keyString(t.Key), len(stopped), cause)
	}
	for _, m := range t.Members {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		top := m.Key == t.Key
		gone, err := c.verify(ctx, m.Key)
		if err == nil && !gone {
			err = c.sig.Stop(m.Key.PID)
			if errors.Is(err, domain.ErrProcessGone) {
				gone, err = true, nil
			}
		}
		switch {
		case err != nil:
			return fail(err)
		case gone && top:
			return fail(fmt.Errorf("top process %s: %w", keyString(m.Key), domain.ErrProcessGone))
		case gone:
			continue
		}
		stopped = append(stopped, m.Key)
	}
	if len(stopped) == 0 {
		return fmt.Errorf("control: pause of %s stopped no processes: %w", keyString(t.Key), domain.ErrProcessGone)
	}
	c.paused[t.Key] = PausedWorkload{Key: t.Key, Name: t.Name, Owner: t.Owner, Members: stopped, Since: c.now()}
	return nil
}

// eligible checks every pause rule; it sends no signals.
func (c *Controller) eligible(t process.WorkloadTarget) error {
	switch {
	case t.Owner == "":
		return refuse(ReasonUnattributed)
	case !t.Class.Heavy():
		return refuse(ReasonNotHeavy)
	case len(t.Members) == 0:
		return refuse(ReasonNoMembers)
	case len(t.Members) > MaxMembers:
		return refuse(ReasonTooManyMembers)
	case t.Members[0].Key != t.Key:
		// The top process must be signaled first so it cannot spawn
		// children mid-pause.
		return refuse(ReasonInvalidTarget)
	}
	seen := make(map[domain.ProcessKey]struct{}, len(t.Members))
	for _, m := range t.Members {
		if !validPID(m.Key.PID) {
			return refuse(ReasonInvalidTarget)
		}
		if _, dup := seen[m.Key]; dup {
			return refuse(ReasonInvalidTarget)
		}
		seen[m.Key] = struct{}{}
		if m.UID != c.uid {
			return refuse(ReasonForeignUID)
		}
	}
	for _, p := range c.paused {
		if p.Key == t.Key {
			return refuse(ReasonAlreadyPaused)
		}
		for _, k := range p.Members {
			if _, ok := seen[k]; ok {
				return refuse(ReasonAlreadyPaused)
			}
		}
	}
	if len(c.paused) >= MaxPaused {
		return refuse(ReasonLimitReached)
	}
	return nil
}

// validPID rejects PIDs that kill(2) treats specially (0 and negatives
// address process groups, -1 every process) and init/launchd.
func validPID(pid int) bool { return pid > 1 }

// verify reports whether k is gone (exited, or its PID now names another
// process or another user's process). A non-nil error means identity could
// not be determined; the caller must not signal.
func (c *Controller) verify(ctx context.Context, k domain.ProcessKey) (gone bool, err error) {
	start, uid, err := c.sig.Identity(ctx, k.PID)
	if errors.Is(err, domain.ErrProcessGone) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("identity of %s: %w", keyString(k), err)
	}
	return !start.Equal(k.Start) || uid != c.uid, nil
}

// continueAll continues keys in reverse order (children before parents),
// verifying each identity first. Gone members are skipped silently. It
// ignores cancellation: resuming is the fail-open direction and must finish.
func (c *Controller) continueAll(ctx context.Context, keys []domain.ProcessKey) []error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	for _, k := range slices.Backward(keys) {
		gone, err := c.verify(ctx, k)
		if err == nil && !gone {
			if err = c.sig.Continue(k.PID); errors.Is(err, domain.ErrProcessGone) {
				err = nil
			} else if err != nil {
				err = fmt.Errorf("continue %s: %w", keyString(k), err)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// Resume continues the paused workload key, children before parents, and
// forgets it, even if some Continue calls fail. Members that are gone are
// skipped. Resume runs to completion even if ctx is cancelled, so work is
// never left stopped by an abandoned request.
func (c *Controller) Resume(ctx context.Context, key domain.ProcessKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.paused[key]
	if !ok {
		return refuse(ReasonNotPaused)
	}
	return c.resume(ctx, p)
}

func (c *Controller) resume(ctx context.Context, p PausedWorkload) error {
	delete(c.paused, p.Key)
	if errs := c.continueAll(ctx, p.Members); len(errs) > 0 {
		return fmt.Errorf("control: resume of %s incomplete: %w", keyString(p.Key), errors.Join(errs...))
	}
	return nil
}

// ResumeAll resumes every paused workload, in key order, and returns one
// error per workload that did not resume cleanly. It is meant for daemon
// shutdown, so Throt fails open.
func (c *Controller) ResumeAll(ctx context.Context) []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, p := range c.sorted() {
		if err := c.resume(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// Paused returns a copy of the paused workloads, sorted by key.
func (c *Controller) Paused() []PausedWorkload {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sorted()
	for i := range out {
		out[i] = out[i].clone()
	}
	return out
}

func (c *Controller) sorted() []PausedWorkload {
	out := make([]PausedWorkload, 0, len(c.paused))
	for _, p := range c.paused {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b PausedWorkload) int { return compareKeys(a.Key, b.Key) })
	return out
}

// Forget drops members that alive reports gone, and drops a record once
// none of its members are alive. It sends no signals. A record whose top
// process died but which still has a live, stopped member is kept, so a
// later Resume or ResumeAll still continues that member.
func (c *Controller) Forget(alive func(domain.ProcessKey) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.paused {
		p.Members = slices.DeleteFunc(p.Members, func(m domain.ProcessKey) bool { return !alive(m) })
		if len(p.Members) == 0 {
			delete(c.paused, k)
		} else {
			c.paused[k] = p
		}
	}
}

// stateVersion is the MarshalState format version.
const stateVersion = 1

type state struct {
	Version int              `json:"version"`
	Paused  []PausedWorkload `json:"paused"`
}

// MarshalState encodes the paused records for crash recovery.
func (c *Controller) MarshalState() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Marshal(state{Version: stateVersion, Paused: c.sorted()})
}

// RestoreAndResume continues every member recorded in state (as written by
// MarshalState) whose identity still matches, children before parents. It
// never pauses and records nothing. Malformed state returns a single error
// and sends no signals.
func (c *Controller) RestoreAndResume(ctx context.Context, data []byte) []error {
	s, err := parseState(data)
	if err != nil {
		return []error{err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, p := range s.Paused {
		if e := c.continueAll(ctx, p.Members); len(e) > 0 {
			errs = append(errs, fmt.Errorf("control: restore of %s incomplete: %w", keyString(p.Key), errors.Join(e...)))
		}
	}
	return errs
}

// parseState decodes and fully validates data before anything is signaled.
func parseState(data []byte) (state, error) {
	var s state
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return state{}, fmt.Errorf("control: malformed state: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return state{}, errors.New("control: malformed state: trailing data")
	}
	if s.Version != stateVersion {
		return state{}, fmt.Errorf("control: malformed state: unsupported version %d", s.Version)
	}
	if len(s.Paused) > MaxPaused {
		return state{}, fmt.Errorf("control: malformed state: %d records exceeds %d", len(s.Paused), MaxPaused)
	}
	for _, p := range s.Paused {
		if len(p.Members) > MaxMembers {
			return state{}, fmt.Errorf("control: malformed state: %s has %d members", keyString(p.Key), len(p.Members))
		}
		for _, m := range p.Members {
			if !validPID(m.PID) || m.Start.IsZero() {
				return state{}, fmt.Errorf("control: malformed state: invalid member %s", keyString(m))
			}
		}
	}
	return s, nil
}

func compareKeys(a, b domain.ProcessKey) int {
	return cmp.Or(cmp.Compare(a.PID, b.PID), a.Start.Compare(b.Start))
}

func keyString(k domain.ProcessKey) string { return "pid:" + strconv.Itoa(k.PID) }
