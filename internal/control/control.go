// Package control pauses and resumes heavy workloads with verified process
// identity. It is the only place Throt decides to signal a process, so every
// rule here errs on the side of not signaling: a process is stopped only
// when it is attributed, heavy, owned by the daemon's user, and still the
// same process (PID and start time) at the moment of signaling. The
// Signaler verifies identity and signals in one call.
//
// Work is never forgotten while it might still be stopped: a member leaves
// a record only once it was continued or the OS confirms it is gone. Records
// that still have members to continue are kept in the resuming state and
// retried by RetryResumes.
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
	// CallTimeout is the default deadline for each per-process OS call.
	CallTimeout = 2 * time.Second
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

// Record states.
const (
	// StatePausing: members are being stopped. Members lists every process
	// the pause intends to stop, so a crash mid-pause leaves a record of
	// everything that might be stopped.
	StatePausing = "pausing"
	// StatePaused: Members lists the processes that were stopped.
	StatePaused = "paused"
	// StateResuming: Members lists processes that still need to be
	// continued; RetryResumes keeps trying.
	StateResuming = "resuming"
)

// ErrMalformedState reports that persisted state could not be used. No
// signal is sent for malformed state.
var ErrMalformedState = errors.New("control: malformed state")

// RefusedError reports that an operation was refused before any signal was
// sent.
type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string { return "control: refused: " + e.Reason }

func refuse(reason string) error { return &RefusedError{Reason: reason} }

// Describe returns a short sentence for a refusal reason code, for people
// to read; logs and the protocol keep the code. An unknown code is
// returned as is.
func Describe(reason string) string {
	switch reason {
	case ReasonUnattributed:
		return "no agent owns it, so throtd does not control it"
	case ReasonNotHeavy:
		return "it is not a heavy workload"
	case ReasonNoMembers:
		return "it has no processes left"
	case ReasonTooManyMembers:
		return fmt.Sprintf("it has more than %d processes", MaxMembers)
	case ReasonInvalidTarget:
		return "its process list is invalid"
	case ReasonForeignUID:
		return "some of its processes belong to another user"
	case ReasonAlreadyPaused:
		return "it is already paused, alone or as part of another workload"
	case ReasonLimitReached:
		return fmt.Sprintf("%d workloads are already paused, the most throtd holds", MaxPaused)
	case ReasonNotPaused:
		return "it is not paused"
	}
	return reason
}

// Signaler is the platform's process-signaling capability. Stop and Continue
// verify that key still names the same process (PID and start time) owned
// by uid and signal it in one operation; they return an error wrapping
// domain.ErrProcessGone, and send nothing, when the process is gone or its
// identity does not match. Every call should honor ctx's deadline.
type Signaler interface {
	// Stop suspends key (SIGSTOP).
	Stop(ctx context.Context, key domain.ProcessKey, uid uint32) error
	// Continue resumes key (SIGCONT).
	Continue(ctx context.Context, key domain.ProcessKey, uid uint32) error
	// Gone reports whether the OS confirms key no longer exists (it exited,
	// or its PID now names a process with another start time). An error
	// means it could not be determined.
	Gone(ctx context.Context, key domain.ProcessKey) (bool, error)
	// Identify returns pid's live start time and uid, with exactly the
	// values Stop compares. It fails with domain.ErrProcessGone when pid
	// names no process.
	Identify(ctx context.Context, pid int) (domain.ProcessSample, error)
}

// PausedWorkload records one paused workload. Members are ordered parents
// before children; see the State constants for what they mean.
type PausedWorkload struct {
	Key     domain.ProcessKey   `json:"process"`
	Name    string              `json:"name"`
	Owner   string              `json:"owner"`
	Members []domain.ProcessKey `json:"members"`
	Since   time.Time           `json:"since"`
	State   string              `json:"state"`
	// By is who paused the workload: OriginManual or OriginScheduler.
	By string `json:"by,omitempty"`
}

// Pause origins. A record without one (older state files) is manual.
const (
	OriginManual    = "manual"
	OriginScheduler = "scheduler"
)

func (p PausedWorkload) clone() PausedWorkload {
	p.Members = slices.Clone(p.Members)
	return p
}

// Option configures a Controller.
type Option func(*Controller)

// WithCallTimeout sets the deadline for each per-process OS call.
func WithCallTimeout(d time.Duration) Option {
	return func(c *Controller) { c.timeout = d }
}

// Controller pauses and resumes workloads owned by one user. It is safe for
// concurrent use; operations that signal are serialized so pause and resume
// never interleave, while Paused and MarshalState read a copy and never wait
// for them.
type Controller struct {
	sig      Signaler
	uid      uint32
	now      func() time.Time
	onChange func(state []byte) error
	timeout  time.Duration

	mu     sync.Mutex // serializes signaling and guards paused and retryAfter
	paused map[domain.ProcessKey]PausedWorkload
	// retryAfter is the record whose retry failed last round; the next
	// round starts after it. Zero means start at the lowest key.
	retryAfter domain.ProcessKey

	viewMu sync.RWMutex // guards view
	view   []PausedWorkload
}

// New returns a controller that only signals processes owned by uid.
// onChange, if non-nil, is called synchronously with the encoded state (see
// MarshalState) every time the records change, before any signal that
// depends on the change is sent. state is nil when no records remain.
func New(sig Signaler, uid uint32, now func() time.Time, onChange func(state []byte) error, opts ...Option) *Controller {
	c := &Controller{
		sig: sig, uid: uid, now: now, onChange: onChange, timeout: CallTimeout,
		paused: map[domain.ProcessKey]PausedWorkload{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// changed publishes the records to Paused and onChange. The caller holds mu.
func (c *Controller) changed() error {
	view := c.sorted()
	c.viewMu.Lock()
	c.view = view
	c.viewMu.Unlock()
	if c.onChange == nil {
		return nil
	}
	if len(view) == 0 {
		return c.onChange(nil)
	}
	b, err := encodeState(view)
	if err != nil {
		return err
	}
	return c.onChange(b)
}

// call runs one per-process OS call with its own deadline. It ignores
// cancellation of ctx, so shutdown cannot abort a resume halfway, but the
// deadline keeps a hung call from blocking forever.
func (c *Controller) call(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	return f(ctx)
}

func (c *Controller) stop(ctx context.Context, k domain.ProcessKey) error {
	return c.call(ctx, func(ctx context.Context) error { return c.sig.Stop(ctx, k, c.uid) })
}

func (c *Controller) cont(ctx context.Context, k domain.ProcessKey) error {
	return c.call(ctx, func(ctx context.Context) error { return c.sig.Continue(ctx, k, c.uid) })
}

func (c *Controller) gone(ctx context.Context, k domain.ProcessKey) (gone bool, err error) {
	err = c.call(ctx, func(ctx context.Context) error {
		gone, err = c.sig.Gone(ctx, k)
		return err
	})
	return gone, err
}

// Pause stops every member of t, parents before children, after checking
// eligibility. The record, listing every intended member in the pausing
// state, is published through onChange before the first signal. Members
// that are gone or no longer match are skipped, and if the top process is
// gone nothing stays paused. If a Stop fails or ctx is cancelled, members
// already stopped are continued in reverse order; any that cannot be
// continued stay recorded in the resuming state.
func (c *Controller) Pause(ctx context.Context, t process.WorkloadTarget) error {
	return c.PauseAs(ctx, t, OriginManual)
}

// PauseAs is Pause with the origin recorded on the record, so the
// scheduler never mistakes a manual hold for its own queueing.
func (c *Controller) PauseAs(ctx context.Context, t process.WorkloadTarget, by string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.eligible(t); err != nil {
		return err
	}
	if err := c.liveOwners(ctx, t); err != nil {
		return err
	}

	rec := PausedWorkload{Key: t.Key, Name: t.Name, Owner: t.Owner, Since: c.now(), State: StatePausing, By: by}
	for _, m := range t.Members {
		rec.Members = append(rec.Members, m.Key)
	}
	c.paused[t.Key] = rec
	if err := c.changed(); err != nil {
		// Without a durable record, a crash could leave work stopped forever.
		delete(c.paused, t.Key)
		c.changed()
		return fmt.Errorf("control: cannot persist pause state, refusing to pause %s: %w", keyString(t.Key), err)
	}

	var stopped []domain.ProcessKey
	fail := func(cause error) error {
		left, errs := c.continueAll(ctx, stopped)
		if len(left) == 0 {
			delete(c.paused, t.Key)
			c.changed()
			return fmt.Errorf("control: pause of %s failed, rolled back %d stopped processes: %w",
				keyString(t.Key), len(stopped), cause)
		}
		rec.Members, rec.State = left, StateResuming
		c.paused[t.Key] = rec
		c.changed()
		return fmt.Errorf("control: pause of %s failed and rollback was incomplete (%d processes left to resume): %w",
			keyString(t.Key), len(left), errors.Join(append([]error{cause}, errs...)...))
	}
	for _, m := range t.Members {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		err := c.stop(ctx, m.Key)
		switch {
		case errors.Is(err, domain.ErrProcessGone) && m.Key == t.Key:
			return fail(fmt.Errorf("top process %s: %w", keyString(m.Key), err))
		case errors.Is(err, domain.ErrProcessGone):
			continue
		case err != nil:
			return fail(fmt.Errorf("stop %s: %w", keyString(m.Key), err))
		}
		stopped = append(stopped, m.Key)
	}
	rec.Members, rec.State = stopped, StatePaused
	c.paused[t.Key] = rec
	c.changed()
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

// liveOwners checks every member's live uid before anything is signaled:
// a sample's uid can be stale (a process that called setuid keeps its PID
// and start time), and a member refused by Stop mid-pause would stop and
// roll back the rest every round. A member that is gone or whose PID was
// reused is left for Stop to skip. A failed lookup refuses to pause.
func (c *Controller) liveOwners(ctx context.Context, t process.WorkloadTarget) error {
	for _, m := range t.Members {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("control: pause of %s: %w", keyString(t.Key), err)
		}
		var p domain.ProcessSample
		err := c.call(ctx, func(ctx context.Context) (err error) {
			p, err = c.sig.Identify(ctx, m.Key.PID)
			return err
		})
		switch {
		case errors.Is(err, domain.ErrProcessGone):
			continue
		case err != nil:
			return fmt.Errorf("control: pause of %s: identify %s: %w", keyString(t.Key), keyString(m.Key), err)
		case !p.Start.Equal(m.Key.Start):
			continue
		case p.UID != c.uid:
			return refuse(ReasonForeignUID)
		}
	}
	return nil
}

// validPID rejects PIDs that kill(2) treats specially (0 and negatives
// address process groups, -1 every process) and init/launchd.
func validPID(pid int) bool { return pid > 1 }

// continueAll continues keys in reverse order (children before parents). It
// returns, parents first, the keys that were neither continued nor
// confirmed gone, with one error each. Each call has its own deadline and
// ignores cancellation: resuming is the fail-open direction.
func (c *Controller) continueAll(ctx context.Context, keys []domain.ProcessKey) (left []domain.ProcessKey, errs []error) {
	left, errs, _ = c.continueKeys(ctx, keys, false)
	return left, errs
}

// continueKeys resumes keys child-first and also returns how many it
// continued (not counting those already gone). With stopOnErr, the first
// hard error ends the round and every key not yet tried stays left, which
// bounds how long a retry holds the lock when the OS is failing.
func (c *Controller) continueKeys(ctx context.Context, keys []domain.ProcessKey, stopOnErr bool) (left []domain.ProcessKey, errs []error, continued int) {
	for i, k := range slices.Backward(keys) {
		err := c.cont(ctx, k)
		if err == nil {
			continued++
			continue
		}
		if errors.Is(err, domain.ErrProcessGone) {
			continue
		}
		left = append(left, k)
		errs = append(errs, fmt.Errorf("continue %s: %w", keyString(k), err))
		if stopOnErr {
			for j := i - 1; j >= 0; j-- {
				left = append(left, keys[j])
			}
			break
		}
	}
	slices.Reverse(left)
	return left, errs, continued
}

// Resume continues the paused workload key, children before parents. Members
// that are continued or gone leave the record; the record is removed once
// none are left, and otherwise kept in the resuming state for RetryResumes.
// Resume runs to completion even if ctx is cancelled, so work is never left
// stopped by an abandoned request.
func (c *Controller) Resume(ctx context.Context, key domain.ProcessKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.paused[key]; !ok {
		return refuse(ReasonNotPaused)
	}
	_, err := c.resume(ctx, key)
	c.changed()
	return err
}

// resume continues one record's members and returns how many it
// continued. The caller holds mu and calls changed afterwards.
func (c *Controller) resume(ctx context.Context, key domain.ProcessKey) (int, error) {
	return c.resumeKey(ctx, key, false)
}

func (c *Controller) resumeKey(ctx context.Context, key domain.ProcessKey, stopOnErr bool) (int, error) {
	p := c.paused[key]
	left, errs, continued := c.continueKeys(ctx, p.Members, stopOnErr)
	if len(left) == 0 {
		delete(c.paused, key)
		return continued, nil
	}
	p.Members, p.State = left, StateResuming
	c.paused[key] = p
	return continued, fmt.Errorf("control: resume of %s incomplete, %d processes left to resume: %w",
		keyString(key), len(left), errors.Join(errs...))
}

// ResumeAll resumes every paused workload, in key order. It returns how
// many processes it continued, leaving out members already gone, and one
// error per workload that did not resume completely; those stay recorded.
// It is meant for daemon shutdown, so Throt fails open.
func (c *Controller) ResumeAll(ctx context.Context) (continued int, errs []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records := c.sorted()
	for _, p := range records {
		n, err := c.resume(ctx, p.Key)
		continued += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(records) > 0 {
		c.changed()
	}
	return continued, errs
}

// RetryResumes retries the records in the resuming state, in key order
// starting after the one that failed in the previous round, until one
// fails. If a pause or
// resume is in progress it does nothing and returns nil; the daemon calls it
// every tick.
func (c *Controller) RetryResumes(ctx context.Context) []error {
	if !c.mu.TryLock() {
		return nil
	}
	defer c.mu.Unlock()
	// Unlike shutdown, a retry ends at the first hard error: the OS is
	// likely still failing, and the next tick tries again. The next round
	// starts after the record that failed, so one record that keeps
	// failing cannot starve the others.
	var resuming []PausedWorkload
	for _, p := range c.sorted() {
		if p.State == StateResuming {
			resuming = append(resuming, p)
		}
	}
	if len(resuming) == 0 {
		c.retryAfter = domain.ProcessKey{}
		return nil
	}
	first := 0
	if c.retryAfter != (domain.ProcessKey{}) {
		// -1 (nothing after it) wraps around to the lowest key.
		first = max(slices.IndexFunc(resuming, func(p PausedWorkload) bool { return compareKeys(p.Key, c.retryAfter) > 0 }), 0)
	}
	c.retryAfter = domain.ProcessKey{}
	var errs []error
	for i := range resuming {
		p := resuming[(first+i)%len(resuming)]
		if _, err := c.resumeKey(ctx, p.Key, true); err != nil {
			errs = append(errs, err)
			c.retryAfter = p.Key
			break
		}
	}
	c.changed()
	return errs
}

// Paused returns a copy of the records, sorted by key. It never waits for
// a pause or resume in progress.
func (c *Controller) Paused() []PausedWorkload {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	out := make([]PausedWorkload, len(c.view))
	for i, p := range c.view {
		out[i] = p.clone()
	}
	return out
}

func (c *Controller) sorted() []PausedWorkload {
	out := make([]PausedWorkload, 0, len(c.paused))
	for _, p := range c.paused {
		out = append(out, p.clone())
	}
	slices.SortFunc(out, func(a, b PausedWorkload) int { return compareKeys(a.Key, b.Key) })
	return out
}

// Forget drops recorded members that alive reports absent and the OS
// confirms gone, and drops a record once no members are left. It sends no
// signals. A member absent from one sample but not confirmed gone is kept.
// If a pause or resume is in progress it does nothing. The round stops at
// the first member whose state cannot be determined, since the OS is then
// likely struggling; later rounds check the rest.
func (c *Controller) Forget(ctx context.Context, alive func(domain.ProcessKey) bool) {
	if !c.mu.TryLock() {
		return
	}
	defer c.mu.Unlock()
	dirty := false
	defer func() {
		if dirty {
			c.changed()
		}
	}()
	for _, p := range c.sorted() {
		kept := p.Members[:0]
		for i, m := range p.Members {
			if alive(m) {
				kept = append(kept, m)
				continue
			}
			gone, err := c.gone(ctx, m)
			if err != nil {
				// Keep this member and every one not yet checked.
				kept = append(kept, p.Members[i:]...)
				c.setMembers(p, kept, &dirty)
				return
			}
			if !gone {
				kept = append(kept, m)
			}
		}
		c.setMembers(p, kept, &dirty)
	}
}

func (c *Controller) setMembers(p PausedWorkload, members []domain.ProcessKey, dirty *bool) {
	switch {
	case len(members) == 0:
		delete(c.paused, p.Key)
	case len(members) == len(c.paused[p.Key].Members):
		return
	default:
		p.Members = members
		c.paused[p.Key] = p
	}
	*dirty = true
}

// stateVersion is the MarshalState format version. Version 2 added State;
// version 1 files are still accepted by RestoreAndResume.
const stateVersion = 2

type state struct {
	Version int              `json:"version"`
	Paused  []PausedWorkload `json:"paused"`
}

// MarshalState encodes the records for crash recovery. It never waits for a
// pause or resume in progress.
func (c *Controller) MarshalState() ([]byte, error) {
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	return encodeState(c.view)
}

func encodeState(records []PausedWorkload) ([]byte, error) {
	if records == nil {
		records = []PausedWorkload{}
	}
	return json.Marshal(state{Version: stateVersion, Paused: records})
}

// RestoreAndResume loads every record in data (as written by MarshalState)
// in the resuming state, whatever state it was saved in, and resumes it.
// Members that cannot be continued stay recorded for RetryResumes, and the
// result is published through onChange. It never pauses. Malformed state
// returns a single error wrapping ErrMalformedState, records nothing, and
// sends no signals.
func (c *Controller) RestoreAndResume(ctx context.Context, data []byte) []error {
	s, err := parseState(data)
	if err != nil {
		return []error{err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, p := range s.Paused {
		if _, ok := c.paused[p.Key]; ok {
			errs = append(errs, fmt.Errorf("control: restore of %s skipped: already recorded", keyString(p.Key)))
			continue
		}
		p.State = StateResuming
		c.paused[p.Key] = p
	}
	c.changed()
	for _, p := range s.Paused {
		if c.paused[p.Key].State != StateResuming {
			continue
		}
		if _, err := c.resume(ctx, p.Key); err != nil {
			errs = append(errs, fmt.Errorf("control: restore: %w", err))
		}
	}
	c.changed()
	return errs
}

// parseState decodes and fully validates data before anything is signaled.
func parseState(data []byte) (state, error) {
	bad := func(format string, args ...any) (state, error) {
		return state{}, fmt.Errorf("%w: %s", ErrMalformedState, fmt.Sprintf(format, args...))
	}
	var s state
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return bad("%v", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return bad("trailing data")
	}
	if s.Version != 1 && s.Version != stateVersion {
		return bad("unsupported version %d", s.Version)
	}
	if len(s.Paused) > MaxPaused {
		return bad("%d records exceeds %d", len(s.Paused), MaxPaused)
	}
	// Samplers report Start in time.Local, and ProcessKey is compared with
	// ==, which includes the location; decoded times are put back in it.
	for i := range s.Paused {
		p := &s.Paused[i]
		p.Key.Start, p.Since = p.Key.Start.Local(), p.Since.Local()
		for j := range p.Members {
			p.Members[j].Start = p.Members[j].Start.Local()
		}
	}
	seen := make(map[domain.ProcessKey]struct{}, len(s.Paused))
	for _, p := range s.Paused {
		if _, dup := seen[p.Key]; dup {
			return bad("duplicate record %s", keyString(p.Key))
		}
		seen[p.Key] = struct{}{}
		switch {
		case s.Version == 1 && p.State != "":
			return bad("%s: state field in version 1", keyString(p.Key))
		case s.Version == stateVersion && p.State != StatePausing && p.State != StatePaused && p.State != StateResuming:
			return bad("%s: unknown state %q", keyString(p.Key), p.State)
		case len(p.Members) > MaxMembers:
			return bad("%s has %d members", keyString(p.Key), len(p.Members))
		}
		for _, m := range p.Members {
			if !validPID(m.PID) || m.Start.IsZero() {
				return bad("invalid member %s", keyString(m))
			}
		}
	}
	return s, nil
}

func compareKeys(a, b domain.ProcessKey) int {
	return cmp.Or(cmp.Compare(a.PID, b.PID), a.Start.Compare(b.Start))
}

func keyString(k domain.ProcessKey) string { return "pid:" + strconv.Itoa(k.PID) }
