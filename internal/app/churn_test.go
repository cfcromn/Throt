package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// churnWorld is a fake host for TestDaemonChurnInvariants: a process table
// the test mutates, a host sampler on a fake clock, and a signaler that
// checks every signal Throt sends against what Throt itself stopped, by
// process key.
type churnWorld struct {
	mu    sync.Mutex
	uid   uint32
	procs map[int]*churnProc
	// throt holds the live keys Throt stopped and has not continued.
	throt map[domain.ProcessKey]bool
	// foreign holds every key ever created with another uid.
	foreign map[domain.ProcessKey]bool
	// violations are invariant breaches seen while signaling.
	violations []string

	now                 time.Time // fake clock for host samples
	memUsed             float64   // fraction of RAM in use
	swapUsed, swapRate  uint64    // bytes, bytes per tick
	failProcs           bool      // the next process sample fails
	faults              int       // Stop/Continue calls left to fail transiently
	stops, conts, stale int       // delivered stops and conts; refused stale keys
	stopCalls           int       // every Stop call, delivered or not
	// afterSample, when set, changes the table right after the next
	// successful process sample, under mu, so the tick acts on a stale
	// snapshot.
	afterSample func()
}

type churnProc struct {
	domain.ProcessSample
	// ext marks a process stopped by someone else (Ctrl-Z), not by Throt.
	ext bool
}

const (
	churnMemTotal  = 16 << 30
	churnSwapTotal = 64 << 30
	churnStep      = 5 * time.Second // fake time per tick
)

func newChurnWorld() *churnWorld {
	return &churnWorld{
		uid:     uint32(os.Getuid()),
		procs:   map[int]*churnProc{},
		throt:   map[domain.ProcessKey]bool{},
		foreign: map[domain.ProcessKey]bool{},
		now:     time.Unix(1_800_000_000, 0),
		memUsed: 0.3,
	}
}

func (w *churnWorld) violate(format string, args ...any) {
	w.violations = append(w.violations, fmt.Sprintf(format, args...))
}

func (w *churnWorld) SampleHost(context.Context) (domain.HostSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(churnStep)
	w.swapUsed = min(w.swapUsed+w.swapRate, churnSwapTotal)
	return domain.HostSample{
		Time: w.now, CPUUsage: 0.1, Load1: 0.1,
		MemTotal: churnMemTotal, MemAvailable: uint64((1 - w.memUsed) * churnMemTotal),
		SwapTotal: churnSwapTotal, SwapUsed: w.swapUsed,
		// The latency probe measures the real timer, which a loaded -race
		// run would make noisy; keep pressure a function of the fake only.
		Missing: []domain.Metric{domain.MetricLatency},
	}, nil
}

func (w *churnWorld) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failProcs {
		w.failProcs = false
		return nil, errors.New("ps: fork: resource temporarily unavailable")
	}
	out := make([]domain.ProcessSample, 0, len(w.procs))
	for _, p := range w.procs {
		out = append(out, p.ProcessSample)
	}
	slices.SortFunc(out, func(a, b domain.ProcessSample) int { return a.PID - b.PID })
	if f := w.afterSample; f != nil {
		w.afterSample = nil
		f()
	}
	return out, nil
}

// signal checks and delivers one Stop or Continue the way the platform
// does: identity (PID, start time, uid) is verified and a mismatch sends
// nothing. Each delivered signal is checked against Throt's own stops.
func (w *churnWorld) signal(stop bool, k domain.ProcessKey, uid uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	op := "cont"
	if stop {
		op = "stop"
		w.stopCalls++
	}
	if uid != w.uid {
		w.violate("%s %v with uid %d, not the daemon's %d", op, k, uid, w.uid)
	}
	if w.foreign[k] {
		w.violate("%s requested for %v of another uid", op, k)
	}
	if w.faults > 0 {
		w.faults--
		return errors.New("transient signal failure")
	}
	p, ok := w.procs[k.PID]
	if !ok || !p.Start.Equal(k.Start) || p.UID != uid {
		w.stale++ // exited or PID reused: the platform refuses, sending nothing
		return fmt.Errorf("%v: %w", k, domain.ErrProcessGone)
	}
	if stop {
		switch {
		case w.throt[k]:
			w.violate("stop %v (%s) twice without a cont", k, p.Name)
		case p.ext:
			w.violate("stop %v (%s) already stopped by someone else", k, p.Name)
		}
		w.stops++
		w.throt[k] = true
		if p.State != domain.StateZombie {
			p.State = domain.StateStopped
		}
		return nil
	}
	if !w.throt[k] {
		w.violate("cont %v (%s, ext=%v) that Throt did not stop", k, p.Name, p.ext)
	}
	w.conts++
	delete(w.throt, k)
	if p.State == domain.StateStopped {
		p.State, p.ext = domain.StateRunning, false
	}
	return nil
}

func (w *churnWorld) Stop(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal(true, k, uid)
}

func (w *churnWorld) Continue(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal(false, k, uid)
}

func (w *churnWorld) Identify(_ context.Context, pid int) (domain.ProcessSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[pid]
	if !ok {
		return domain.ProcessSample{}, domain.ErrProcessGone
	}
	return p.ProcessSample, nil
}

func (w *churnWorld) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[k.PID]
	return !ok || !p.Start.Equal(k.Start), nil
}

// live reports whether k names a process in the table. The caller holds mu.
func (w *churnWorld) live(k domain.ProcessKey) bool {
	p, ok := w.procs[k.PID]
	return ok && p.Start.Equal(k.Start)
}

// running reports whether k names a live process that is neither stopped
// nor a zombie. The caller holds mu.
func (w *churnWorld) running(k domain.ProcessKey) bool {
	p, ok := w.procs[k.PID]
	return ok && p.Start.Equal(k.Start) && p.State != domain.StateStopped && p.State != domain.StateZombie
}

// churn drives random events through one daemon.
type churn struct {
	t     *testing.T
	seed  uint64
	rng   *rand.Rand
	w     *churnWorld
	d     *Daemon
	cfg   string // config file path for reloads
	sock  string
	trace []string

	nextPID int
	starts  int // start-time sequence, so a reused PID gets a new start

	mode    string
	limit   int
	pending bool // a reload not yet applied by a tick
	// unbaselined: a policy was applied and no scheduler round has run
	// since, so that round adopts every workload as pre-existing.
	unbaselined bool
	lastSeq     uint64
	levels      map[domain.PressureLevel]int
	queued      int
	resumed     int
	ticks       int
}

func (c *churn) logf(format string, args ...any) {
	c.trace = append(c.trace, fmt.Sprintf(format, args...))
}

// fail reports an invariant breach with the seed and the event trace, so
// the run can be replayed with THROT_CHURN_SEED.
func (c *churn) fail(format string, args ...any) {
	c.t.Helper()
	tail := c.trace
	if len(tail) > 120 {
		tail = tail[len(tail)-120:]
	}
	c.t.Fatalf("seed %d (replay: THROT_CHURN_SEED=%d), tick %d: %s\nlast events:\n  %s",
		c.seed, c.seed, c.ticks, fmt.Sprintf(format, args...), strings.Join(tail, "\n  "))
}

func (c *churn) writeConfig() {
	m := map[string]any{
		"socket_path":     c.sock,
		"sample_interval": "2s",
		"profile":         "performance",
		"scheduler": map[string]any{
			"mode": c.mode, "heavy_limit": c.limit, "start_limit": c.limit,
			"resume_cooldown": "0s", "max_queue_wait": "1h",
			"adaptive": false, "pause_on_critical": true,
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(c.cfg, b, 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// spawn adds a process; pid 0 allocates a fresh PID.
func (c *churn) spawn(pid, ppid int, name string, uid uint32) domain.ProcessKey {
	w := c.w
	if pid == 0 {
		c.nextPID++
		pid = c.nextPID
	}
	c.starts++
	p := &churnProc{ProcessSample: domain.ProcessSample{
		PID: pid, PPID: ppid, UID: uid, Name: name, State: domain.StateRunning,
		Start: time.Unix(1_700_000_000, 0).Add(time.Duration(c.starts) * time.Millisecond),
		RSS:   uint64(1+c.rng.IntN(64)) << 20,
	}}
	w.procs[pid] = p
	if uid != w.uid {
		w.foreign[p.Key()] = true
	}
	return p.Key()
}

// remove takes a process out of the table (it exited and was reaped);
// its children are reparented to init.
func (c *churn) remove(pid int) {
	w := c.w
	p, ok := w.procs[pid]
	if !ok {
		return
	}
	delete(w.procs, pid)
	delete(w.throt, p.Key())
	c.reparent(pid)
}

func (c *churn) reparent(pid int) {
	for _, q := range c.w.procs {
		if q.PPID == pid {
			q.PPID = 1
		}
	}
}

// pick returns a random PID among the processes f accepts, in PID order so
// a seed replays exactly; 0 if there is none.
func (c *churn) pick(f func(*churnProc) bool) int {
	var pids []int
	for pid, p := range c.w.procs {
		if f(p) {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return 0
	}
	slices.Sort(pids)
	return pids[c.rng.IntN(len(pids))]
}

func isAgentName(n string) bool { return n == "claude" || n == "codex" }

// descendants returns pid's descendants, parents first.
func (c *churn) descendants(pid int) []int {
	var out []int
	frontier := []int{pid}
	for len(frontier) > 0 {
		var next []int
		for _, p := range frontier {
			var kids []int
			for cp, q := range c.w.procs {
				if q.PPID == p {
					kids = append(kids, cp)
				}
			}
			slices.Sort(kids)
			next = append(next, kids...)
		}
		out = append(out, next...)
		frontier = next
	}
	return out
}

var (
	buildNames = []string{"go", "make", "cargo"}
	childNames = []string{"cc1", "rustc", "ld", "sh", "compile", "link"}
)

// spawnBuild starts a heavy build with up to three children under parent.
func (c *churn) spawnBuild(parent int, uid uint32) {
	top := c.spawn(0, parent, buildNames[c.rng.IntN(len(buildNames))], uid)
	n := c.rng.IntN(4)
	at := top.PID
	for range n {
		cuid := uid
		if c.rng.IntN(25) == 0 {
			cuid = uid + 1 // a member of another user: pausing it is refused
		}
		k := c.spawn(0, at, childNames[c.rng.IntN(len(childNames))], cuid)
		if cuid == c.w.uid && c.rng.IntN(6) == 0 {
			// Stopped by someone else (a debugger, a Ctrl-Z'd sub-job)
			// before Throt ever sees it.
			p := c.w.procs[k.PID]
			p.State, p.ext = domain.StateStopped, true
			c.logf("child pid %d starts stopped by someone else", k.PID)
		}
		if c.rng.IntN(2) == 0 {
			at = k.PID
		}
	}
	c.logf("build %s pid %d under %d (+%d children, uid %d)", c.w.procs[top.PID].Name, top.PID, parent, n, uid)
}

// step applies one random event to the world.
func (c *churn) step() {
	w := c.w
	w.mu.Lock()
	defer w.mu.Unlock()
	agents := func(p *churnProc) bool { return isAgentName(p.Name) && p.UID == w.uid }
	switch r := c.rng.IntN(106); {
	case r < 8: // an agent starts
		if c.pick(agents) != 0 && len(w.procs) > 60 {
			return
		}
		name := []string{"claude", "codex"}[c.rng.IntN(2)]
		k := c.spawn(0, 1, name, w.uid)
		c.logf("agent %s pid %d", name, k.PID)
	case r < 30: // a build starts under an agent, or unattributed
		parent := c.pick(agents)
		switch {
		case parent == 0 || c.rng.IntN(8) == 0:
			parent = 1
			if c.rng.IntN(2) == 0 {
				parent = c.spawn(0, 1, "zsh", w.uid).PID
			}
		case c.rng.IntN(4) == 0:
			parent = c.spawn(0, parent, "zsh", w.uid).PID
		}
		c.spawnBuild(parent, w.uid)
	case r < 32: // a foreign user's agent and build
		a := c.spawn(0, 1, "claude", w.uid+1)
		c.spawnBuild(a.PID, w.uid+1)
	case r < 40: // a build spawns another child
		pid := c.pick(func(p *churnProc) bool { return slices.Contains(buildNames, p.Name) && p.State != domain.StateZombie })
		if pid == 0 {
			return
		}
		k := c.spawn(0, pid, childNames[c.rng.IntN(len(childNames))], w.uid)
		c.logf("child pid %d under %d", k.PID, pid)
	case r < 55: // a process exits (in any state, stopped or queued too)
		c.exitOne("")
	case r < 63: // a whole build finishes: top and descendants
		pid := c.pick(func(p *churnProc) bool { return slices.Contains(buildNames, p.Name) })
		if pid == 0 {
			return
		}
		c.logf("build pid %d finishes with its subtree", pid)
		for _, d := range slices.Backward(c.descendants(pid)) {
			c.remove(d)
		}
		c.remove(pid)
	case r < 67: // an agent exits; its builds are reparented to init
		pid := c.pick(agents)
		if pid == 0 {
			return
		}
		c.logf("agent pid %d exits", pid)
		c.remove(pid)
	case r < 72: // PID reuse: a process exits and its PID is reused at once
		c.reuseOne("")
	case r < 78: // Ctrl-Z: someone else stops a running job
		c.ctrlZ("")
	case r < 82: // fg: the user continues a job they stopped
		pid := c.pick(func(p *churnProc) bool { return p.ext })
		if pid == 0 {
			return
		}
		for _, d := range append([]int{pid}, c.descendants(pid)...) {
			if q := w.procs[d]; q.ext {
				q.State, q.ext = domain.StateRunning, false
			}
		}
		c.logf("fg pid %d", pid)
	case r < 84: // a process becomes a zombie
		pid := c.pick(func(p *churnProc) bool { return !isAgentName(p.Name) })
		if pid == 0 {
			return
		}
		p := w.procs[pid]
		p.State, p.ext = domain.StateZombie, false
		c.reparent(pid)
		c.logf("zombie pid %d (%s, throt=%v)", pid, p.Name, w.throt[p.Key()])
	case r < 91: // memory pressure moves
		levels := []float64{0.3, 0.3, 0.3, 0.3, 0.86, 0.93, 0.97}
		w.memUsed = levels[c.rng.IntN(len(levels))]
		c.logf("memory used %.2f", w.memUsed)
	case r < 93: // swap growth starts or stops
		w.swapRate = []uint64{0, 0, 0, 50 << 20, 200 << 20}[c.rng.IntN(5)]
		c.logf("swap growth %d MiB per tick", w.swapRate>>20)
	case r < 96: // reload with another policy
		c.mode = "enforce"
		if c.rng.IntN(4) == 0 {
			c.mode = "observe"
		}
		c.limit = 1 + c.rng.IntN(3)
		c.writeConfig()
		w.mu.Unlock()
		_, err := c.d.Reload()
		w.mu.Lock()
		if err != nil {
			c.fail("reload: %v", err)
		}
		c.pending = true
		c.logf("reload mode %s heavy_limit %d", c.mode, c.limit)
	case r < 98:
		w.failProcs = true
		c.logf("next process sample fails")
	case r < 100:
		w.faults = 1 + c.rng.IntN(3)
		c.logf("next %d signals fail", w.faults)
	default:
		// A race: the next tick's process sample is already stale when
		// the daemon acts on it, because a process exits, its PID is
		// reused, or it is Ctrl-Z'd right after ps ran.
		what := []func(string){c.exitOne, c.reuseOne, c.ctrlZ}[c.rng.IntN(3)]
		w.afterSample = func() { what("after sample: ") }
		c.logf("next process sample goes stale at once")
	}
}

// exitOne makes a random non-agent process exit, in any state. The caller
// holds w.mu.
func (c *churn) exitOne(prefix string) {
	w := c.w
	pid := c.pick(func(p *churnProc) bool { return !isAgentName(p.Name) })
	if pid == 0 {
		return
	}
	p := w.procs[pid]
	c.logf("%sexit pid %d (%s, %s, throt=%v ext=%v)", prefix, pid, p.Name, p.State, w.throt[p.Key()], p.ext)
	c.remove(pid)
}

// reuseOne makes a random non-agent process exit and reuses its PID at
// once for a new build. The caller holds w.mu.
func (c *churn) reuseOne(prefix string) {
	w := c.w
	pid := c.pick(func(p *churnProc) bool { return !isAgentName(p.Name) })
	if pid == 0 {
		return
	}
	old := w.procs[pid]
	c.logf("%spid %d (%s, %s, throt=%v) reused", prefix, pid, old.Name, old.State, w.throt[old.Key()])
	c.remove(pid)
	parent := c.pick(func(p *churnProc) bool { return isAgentName(p.Name) && p.UID == w.uid })
	if parent == 0 {
		parent = 1
	}
	c.spawn(pid, parent, buildNames[c.rng.IntN(len(buildNames))], w.uid)
}

// ctrlZ makes someone else stop a random running job: a process of the
// user and its running descendants. The caller holds w.mu.
func (c *churn) ctrlZ(prefix string) {
	w := c.w
	pid := c.pick(func(p *churnProc) bool {
		return !isAgentName(p.Name) && p.UID == w.uid && p.State == domain.StateRunning
	})
	if pid == 0 {
		return
	}
	var n int
	job := []int{pid}
	if c.rng.IntN(2) == 0 {
		job = append(job, c.descendants(pid)...) // the whole job, not one process
	}
	for _, d := range job {
		if q := w.procs[d]; q.UID == w.uid && q.State == domain.StateRunning {
			q.State, q.ext = domain.StateStopped, true
			n++
		}
	}
	c.logf("%sctrl-z pid %d (%s): %d stopped by someone else", prefix, pid, w.procs[pid].Name, n)
}

// heavyNow returns the heavy workloads of the latest successful snapshot.
func (c *churn) heavyNow() map[domain.ProcessKey]string {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	out := make(map[domain.ProcessKey]string, len(c.d.status.Heavy))
	for _, h := range c.d.status.Heavy {
		out[h.Key] = h.Owner
	}
	return out
}

// tick runs one daemon tick and checks every invariant after it.
func (c *churn) tick(ctx context.Context) {
	c.t.Helper()
	w := c.w
	before := c.heavyNow()
	w.mu.Lock()
	// Running heavy workloads the scheduler already knows: pre-existing.
	known := map[domain.ProcessKey]bool{}
	for k := range before {
		if w.running(k) {
			known[k] = true
		}
	}
	failing := w.failProcs
	stopCalls := w.stopCalls
	w.mu.Unlock()
	if c.pending {
		c.pending, c.unbaselined = false, true
	}

	c.d.tick(ctx)
	c.ticks++

	level := c.d.engine.Level()
	c.levels[level]++
	policy := c.d.sched.Policy()
	limit := c.d.sched.Limit()
	baseline := c.unbaselined
	if !failing {
		c.unbaselined = false
	}
	events := c.d.events.Since(c.lastSeq, 0)
	if len(events) > 0 {
		c.lastSeq = events[len(events)-1].Seq
	}
	queuedPIDs := map[int]bool{}
	for _, e := range events {
		if e.Kind != domain.EventSchedulerDecision || e.Attrs["observed"] == "true" {
			continue
		}
		c.logf("  event %s", e.Message)
		switch e.Attrs["decision"] {
		case string(scheduler.ActionQueue), string(scheduler.ActionPreempt):
			c.queued++
			pid, _ := strconv.Atoi(e.Attrs["pid"])
			queuedPIDs[pid] = true
		case string(scheduler.ActionResume):
			c.resumed++
			// A scheduler resume fills a free slot: the count it reports
			// includes the resumed workload. The daemon's own resumes
			// (top_exited, enforce_disabled) carry no count.
			if r, ok := e.Attrs["running"]; ok {
				running, _ := strconv.Atoi(r)
				lim, _ := strconv.Atoi(e.Attrs["limit"])
				if running > lim {
					c.fail("resume decision over the limit: %s", e.Message)
				}
			}
		}
	}

	after := c.heavyNow()
	paused := c.d.ctl.Paused()
	recorded := map[domain.ProcessKey]bool{}
	for _, p := range paused {
		for _, m := range p.Members {
			recorded[m] = true
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// 1-3: checked on every signal by the world.
	if len(w.violations) > 0 {
		c.fail("%s", strings.Join(w.violations, "; "))
	}
	// Observe mode never stops anything.
	if policy.Mode == scheduler.ModeObserve && w.stopCalls != stopCalls {
		c.fail("observe-mode tick sent %d stops", w.stopCalls-stopCalls)
	}
	// 4: nothing Throt stopped is without a record.
	for k := range w.throt {
		if w.live(k) && !recorded[k] {
			c.fail("%v (%s) stopped by Throt but in no paused record %+v", k, w.procs[k.PID].Name, paused)
		}
	}
	// 6: a round admits or resumes attributed workloads only into free
	// slots. Workloads already running before it, and unattributed ones,
	// count toward the limit but are never queued; neither are workloads
	// it decided to queue but could not pause (refused or failed). A round
	// right after a policy change adopts everything, and unknown pressure
	// fails open, so neither is checked.
	if failing || baseline || policy.Mode != scheduler.ModeEnforce || level == domain.PressureUnknown {
		return
	}
	var pre, unattributed, admitted, unpaused int
	var admittedKeys []domain.ProcessKey
	for k, owner := range after {
		switch {
		case known[k]:
			pre++ // running when the round started; it may have been preempted since
		case !w.running(k):
		case owner == "":
			unattributed++
		case queuedPIDs[k.PID]:
			unpaused++
		default:
			admitted++
			admittedKeys = append(admittedKeys, k)
		}
	}
	if free := max(0, limit-pre-unattributed); admitted > free {
		c.fail("round admitted or resumed %d workloads %v with %d free slots (limit %d, %d running before, %d unattributed, %d not pausable)",
			admitted, admittedKeys, free, limit, pre, unattributed, unpaused)
	}
}

// runChurn drives one seed: warm-up, random events, then a drain that must
// leave nothing stopped by Throt.
func runChurn(t *testing.T, seed uint64, events int) {
	dir := t.TempDir()
	c := &churn{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		w: newChurnWorld(), cfg: filepath.Join(dir, "config.json"), sock: filepath.Join(dir, "throtd.sock"),
		nextPID: 100, mode: "enforce", limit: 1 + int(seed%3),
		levels: map[domain.PressureLevel]int{},
	}
	c.writeConfig()
	cfg, err := config.Load(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.d = NewDaemon(cfg, c.w, c.w, c.w, slog.New(slog.DiscardHandler))
	c.d.SetConfigPath(c.cfg)
	ctx := context.Background()

	// Warm up: the scheduler never queues while pressure is unknown.
	for range 10 {
		c.tick(ctx)
		if c.d.engine.Level() == domain.PressureNormal {
			break
		}
	}
	if c.d.engine.Level() != domain.PressureNormal {
		t.Fatalf("pressure %s after warm-up", c.d.engine.Level())
	}

	for done := 0; done < events; {
		for n := 1 + c.rng.IntN(4); n > 0 && done < events; n-- {
			c.step()
			done++
		}
		c.tick(ctx)
	}

	// Drain: calm host, no faults; running work exits round after round,
	// so every slot frees and everything queued must be resumed. Processes
	// stopped by someone else stay stopped: Throt must leave them alone.
	w := c.w
	w.mu.Lock()
	w.memUsed, w.swapRate, w.failProcs, w.faults, w.afterSample = 0.3, 0, false, 0, nil
	w.mu.Unlock()
	c.logf("drain")
	for round := 0; ; round++ {
		w.mu.Lock()
		for _, pid := range slices.Sorted(func(yield func(int) bool) {
			for pid := range w.procs {
				if !yield(pid) {
					return
				}
			}
		}) {
			p, ok := w.procs[pid]
			if ok && !isAgentName(p.Name) && (p.State == domain.StateRunning || p.State == domain.StateZombie) {
				c.remove(pid)
			}
		}
		left := len(w.throt)
		w.mu.Unlock()
		if left == 0 && len(c.d.ctl.Paused()) == 0 {
			break
		}
		if round == 60 {
			w.mu.Lock()
			var keys []string
			for k := range w.throt {
				keys = append(keys, fmt.Sprintf("%v %s", k, w.procs[k.PID].Name))
			}
			w.mu.Unlock()
			c.fail("drain: still stopped by Throt after %d rounds at %s: %v; records %+v",
				round, c.d.engine.Level(), keys, c.d.ctl.Paused())
		}
		c.tick(ctx)
	}
	// 5: shutdown has nothing left to resume, and nothing someone else
	// stopped was ever continued (checked on every signal).
	if n := c.d.resumeAll(); n != 0 {
		c.fail("shutdown resumed %d processes after the drain", n)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.violations) > 0 {
		c.fail("%s", strings.Join(w.violations, "; "))
	}
	if w.stops == 0 || c.resumed == 0 {
		c.fail("vacuous run: %d stops, %d resumes", w.stops, c.resumed)
	}
	t.Logf("seed %d: %d ticks, %d stops, %d conts, %d stale keys refused, %d queue/preempt and %d resume decisions, pressure rounds %v",
		seed, c.ticks, w.stops, w.conts, w.stale, c.queued, c.resumed, c.levels)
}

// TestDaemonChurnInvariants drives seeded random process churn, pressure
// changes, reloads and failures through the real daemon (graph, scheduler,
// controller, state file), one tick at a time, and checks Throt's safety
// invariants after every tick: it never continues what it did not stop
// (a Ctrl-Z'd process above all), never signals another user's process or
// a reused PID, never double-stops, records everything it stopped,
// respects the heavy limit, and leaves nothing stopped once work drains.
// THROT_CHURN_SEED replays one seed.
func TestDaemonChurnInvariants(t *testing.T) {
	seeds := []uint64{1, 2, 3, 42, 1009}
	if s := os.Getenv("THROT_CHURN_SEED"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("THROT_CHURN_SEED=%q: %v", s, err)
		}
		seeds = []uint64{n}
	}
	baseline := runtime.NumGoroutine()
	t.Run("seeds", func(t *testing.T) {
		for _, seed := range seeds {
			t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
				t.Parallel()
				runChurn(t, seed, 300)
			})
		}
	})
	// 7: no goroutine outlives the daemons.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d after, %d before\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
