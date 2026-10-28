package e2e

// The soak test runs one enforce-mode throtd for a long time under process
// churn and checks that it stays healthy: no stranded (stopped) work, flat
// memory, goroutine, thread and file descriptor counts, a responsive status
// method, an intact event log, and no more running heavy work than the limit
// allows. It is too long for CI. Run it in a container, e.g.
//
//	docker run --rm --cpus=2 -v "$PWD":/src -w /src golang:1.26.10-alpine sh -c \
//	  'go test -c -o /tmp/e2e.test ./internal/e2e && THROT_SOAK=1 /tmp/e2e.test -test.run=TestSoak -test.v -test.timeout=30m'
//
// THROT_SOAK_DURATION (default 10m) sets the churn phase's length,
// THROT_SOAK_NO_LULL=1 removes the agents' lulls, and
// THROT_SOAK_LOGDIR, when set, keeps throtd's output there.

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// soakAgentSource drives churn from inside an agent. "jobs SEED N" runs N
// workers, each repeatedly starting a make job (sleep or burn, 0.2-3s of
// work) and reporting "start PID", with a lull of 10s in every 30s unless
// THROT_SOAK_NO_LULL=1, which keeps the queue busy throughout. "lite N"
// runs N loops that start the
// light helper back to back, and finally reports "lite COUNT MAXPID WRAPS",
// where WRAPS counts how often the PID space wrapped, so PIDs were reused.
// Both stop starting work when stdin closes and exit once their children do.
const soakAgentSource = `package main

import (
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

func main() {
	self, _ := os.Executable()
	dir := filepath.Dir(self)
	stop := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(stop) }()
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	var mu sync.Mutex
	say := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf(format+"\n", args...)
	}
	n, _ := strconv.Atoi(os.Args[len(os.Args)-1])
	var wg sync.WaitGroup
	switch os.Args[1] {
	case "jobs":
		seed, _ := strconv.ParseUint(os.Args[2], 10, 64)
		lulls := os.Getenv("THROT_SOAK_NO_LULL") != "1"
		for w := range n {
			wg.Go(func() {
				r := rand.New(rand.NewPCG(seed, uint64(w)))
				for !stopped() {
					// Every agent pauses 10s in each 30s, so the queue drains,
					// unless THROT_SOAK_NO_LULL=1.
					for lulls && time.Now().Unix()%30 >= 20 && !stopped() {
						time.Sleep(100 * time.Millisecond)
					}
					mode := "sleep"
					if r.IntN(3) == 0 {
						mode = "burn"
					}
					d := 200*time.Millisecond + time.Duration(r.Int64N(int64(2800*time.Millisecond)))
					cmd := exec.Command(filepath.Join(dir, "make"), mode, d.String())
					if err := cmd.Start(); err != nil {
						say("error %v", err)
						time.Sleep(time.Second)
						continue
					}
					say("start %d", cmd.Process.Pid)
					cmd.Wait()
					select {
					case <-stop:
					case <-time.After(time.Duration(r.Int64N(int64(300 * time.Millisecond)))):
					}
				}
			})
		}
	case "lite":
		var count, last, peak, wraps int
		for range n {
			wg.Go(func() {
				for !stopped() {
					cmd := exec.Command(filepath.Join(dir, "throt-soak-lite"))
					if err := cmd.Start(); err != nil {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					pid := cmd.Process.Pid
					cmd.Wait()
					mu.Lock()
					count++
					if pid+50000 < last {
						wraps++
					}
					last, peak = pid, max(peak, pid)
					mu.Unlock()
					time.Sleep(2 * time.Millisecond)
				}
			})
		}
		wg.Wait()
		say("lite %d %d %d", count, peak, wraps)
	}
	wg.Wait()
}
`

// soakLiteSource is the light process: it exits at once.
const soakLiteSource = "package main\n\nfunc main() {}\n"

const (
	soakAgentExe = "throt-soak-agt"
	soakLiteExe  = "throt-soak-lite"
	// soakInterval is throtd's sample interval.
	soakInterval   = 500 * time.Millisecond
	soakHeavyLimit = 2
	// soakTransient is how long a new heavy job may run before throtd
	// queues it. The documented bound is one sample interval; this allows a
	// tick's own duration and scheduling delay on a loaded 2-CPU container.
	soakTransient = 4 * soakInterval
	// soakStuck is how long a job may stay stopped while throtd runs. Queued
	// work outranks everything after max_queue_wait (30s here), so this is
	// generous.
	soakStuck = 2 * time.Minute
	// soakDrain bounds the wait for work to finish after the churn stops.
	soakDrain = 2 * time.Minute
)

func soakConfig(mode string) string {
	return fmt.Sprintf(`{"sample_interval":%q,"agents":[{"name":"soak","executables":[%q]}],`+
		`"scheduler":{"mode":%q,"heavy_limit":%d,"resume_cooldown":"1s","max_queue_wait":"30s","adaptive":false}}`,
		soakInterval.String(), soakAgentExe, mode, soakHeavyLimit)
}

// procStat returns the state letter and start time (clock ticks since
// boot) of pid, or ok false if it is gone.
func procStat(pid int) (state byte, start uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, 0, false
	}
	start, err = strconv.ParseUint(f[19], 10, 64) // field 22
	return f[0][0], start, err == nil
}

// soakJob is one heavy job the test started, directly (throt run) or
// through an agent.
type soakJob struct {
	pid   int
	start uint64
	// stderr is a throt run's stderr file; "" for agent jobs.
	stderr string
	// firstRun is when the checker first saw it running as make.
	firstRun time.Time
	// stoppedAt is when the checker first saw it stopped, zero if running.
	stoppedAt time.Time
	// exempt jobs may exceed the limit: they were running when throtd
	// rebaselined (start, restart, reload) or the test resumed them, or a
	// throt run started without a lease (fail-open).
	exempt, checked bool
	// manual is set while the test holds it paused.
	manual, stuck bool
}

type soakSample struct {
	at       time.Duration // since the churn started
	instance int
	rss      int64 // KiB
	threads  int
	fds      int
}

type soakLatency struct {
	at time.Duration
	d  time.Duration
}

type soak struct {
	t     *testing.T
	e     *env
	dur   time.Duration
	every time.Duration // memory and goroutine sampling
	begin time.Time
	// runDir holds the throt run callers' stderr files.
	runDir string
	// phase is held shared by every client action and exclusively by the
	// restart, so no manual pause or reload straddles a crash.
	phase sync.RWMutex

	mu sync.Mutex
	// Guarded by mu.
	jobs        map[[2]uint64]*soakJob
	daemonPID   int
	instance    int
	settleUntil time.Time
	holds       int
	maxRunning  int
	limitChecks int
	// maxCounted is the most running jobs neither exempt nor new.
	maxCounted  int
	violations  []string
	maxStopped  time.Duration
	samples     []soakSample
	latencies   []soakLatency
	statusFails []string
	pressures   map[string]int
	pauses      map[string]int
	runs        map[string]int
	lite        string
	spawned     int
}

func soakDuration(t *testing.T) time.Duration {
	d := 10 * time.Minute
	if v := os.Getenv("THROT_SOAK_DURATION"); v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d < time.Minute {
			t.Fatalf("THROT_SOAK_DURATION=%q: want a duration of at least 1m", v)
		}
	}
	return d
}

func TestSoak(t *testing.T) {
	requireGate(t, soakGate)
	dur := soakDuration(t)
	if dl, ok := t.Deadline(); ok && time.Until(dl) < dur+3*soakDrain+time.Minute {
		t.Fatalf("-test.timeout is too short for a %s soak; pass -test.timeout=%s", dur, dur+10*time.Minute)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{soakAgentExe: soakAgentSource, soakLiteExe: soakLiteSource} {
		if err := buildHelper(goBin, filepath.Dir(binDir), name, src); err != nil {
			t.Fatal(err)
		}
	}

	s := &soak{t: t, dur: dur, every: min(30*time.Second, dur/12), runDir: t.TempDir(),
		jobs: map[[2]uint64]*soakJob{}, pressures: map[string]int{}, pauses: map[string]int{}, runs: map[string]int{}}
	s.e = setupEnv(t, soakConfig("enforce"))
	s.e.vars = append(s.e.vars, "THROT_DEBUG_GOROUTINES="+s.every.String())
	logDir := os.Getenv("THROT_SOAK_LOGDIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	logPath := filepath.Join(logDir, "throtd.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	s.e.out = logFile
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("throtd output is in %s; its tail:\n%s", logPath, tail(logPath, 60))
		}
	})
	s.run()
	s.report(logPath)
}

func (s *soak) errorf(format string, args ...any) {
	s.t.Helper()
	s.t.Errorf("[%s] "+format, append([]any{time.Since(s.begin).Round(time.Second)}, args...)...)
}

// settle suspends the limit check for d and exempts every job seen running
// meanwhile: throtd treats work running at a rebaseline as pre-existing.
func (s *soak) settle(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleUntil = later(s.settleUntil, time.Now().Add(d))
}

// hold suspends the limit check until release.
func (s *soak) hold() { s.mu.Lock(); s.holds++; s.mu.Unlock() }

func (s *soak) release(after time.Duration) {
	s.mu.Lock()
	s.holds--
	s.mu.Unlock()
	s.settle(after)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *soak) addJob(pid int, stderr string) {
	_, start, ok := procStat(pid)
	if !ok {
		return // already gone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[[2]uint64{uint64(pid), start}] = &soakJob{pid: pid, start: start, stderr: stderr}
	s.spawned++
}

func (s *soak) run() {
	s.startDaemon()
	s.e.waitStartable()
	s.begin = time.Now()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	every := func(d time.Duration, f func()) {
		wg.Go(func() {
			tk := time.NewTicker(d)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					f()
				}
			}
		})
	}

	follower := s.followEvents()
	agents := make([]*soakAgent, 0, 5)
	for i := range 4 {
		agents = append(agents, s.startAgent("jobs", strconv.Itoa(i+1), "2"))
	}
	agents = append(agents, s.startAgent("lite", "4"))

	s.sample()
	every(s.every, s.sample)
	every(100*time.Millisecond, s.checkHeavy)
	every(time.Second, s.pollStatus)
	rng := rand.New(rand.NewPCG(1, 2))
	var rngMu sync.Mutex
	intn := func(n int) int { rngMu.Lock(); defer rngMu.Unlock(); return rng.IntN(n) }
	cycle := 0
	every(3*time.Second, func() {
		cycle++
		s.pauseResume(cycle%4 == 0, intn)
	})
	runs := make(chan struct{}, 2) // at most two throt run callers at once
	var runWG sync.WaitGroup
	every(5*time.Second, func() {
		select {
		case runs <- struct{}{}:
			runWG.Go(func() { defer func() { <-runs }(); s.throtRun() })
		default:
		}
	})
	reloadEvery := max(s.dur/5, 20*time.Second)
	every(reloadEvery, func() { s.toggleMode(min(10*time.Second, reloadEvery/2)) })

	time.Sleep(s.dur / 2)
	s.restart()
	time.Sleep(time.Until(s.begin.Add(s.dur)))

	// Stop the drivers, then shut throtd down while the agents still have
	// work queued: graceful shutdown must resume all of it.
	close(stop)
	wg.Wait()
	s.mu.Lock()
	queued := 0
	for _, j := range s.jobs {
		if st, start, ok := procStat(j.pid); ok && start == j.start && st == 'T' {
			queued++
		}
	}
	s.mu.Unlock()
	follower.stop()
	if err := s.e.stopDaemon(syscall.SIGTERM); err != nil {
		s.errorf("throtd exit after SIGTERM: %v", err)
	}
	if stopped := stoppedProcesses(); len(stopped) > 0 {
		s.errorf("after graceful shutdown (%d jobs were stopped just before it), stopped processes remain: %v", queued, stopped)
	}
	s.t.Logf("graceful shutdown with %d stopped jobs left none stopped", queued)

	// Without a daemon nothing holds the work back, so it must drain.
	deadline := time.After(soakDrain)
	for _, a := range agents {
		a.stdin.Close()
	}
	for _, a := range agents {
		select {
		case <-a.done:
		case <-deadline:
			s.errorf("agent %s (pid %d) did not finish its work within %s", a.mode, a.pid, soakDrain)
		}
	}
	done := make(chan struct{})
	go func() { runWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(soakDrain):
		s.errorf("throt run callers did not finish within %s", soakDrain)
	}
	follower.check(s)
	s.mu.Lock()
	s.lite = agents[len(agents)-1].lite
	s.mu.Unlock()
}

func (s *soak) startDaemon() {
	s.e.startDaemon()
	s.mu.Lock()
	s.daemonPID = s.e.daemon.Process.Pid
	s.mu.Unlock()
}

// restart SIGKILLs throtd and starts a new one, as a crash would. Work it
// held stopped stays stopped until the new daemon restores it.
func (s *soak) restart() {
	s.phase.Lock()
	s.hold()
	s.mu.Lock()
	s.daemonPID = 0
	s.mu.Unlock()
	s.e.stopDaemon(syscall.SIGKILL)
	var held []*soakJob
	s.mu.Lock()
	for _, j := range s.jobs {
		if st, start, ok := procStat(j.pid); ok && start == j.start && st == 'T' {
			held = append(held, j)
		}
	}
	s.mu.Unlock()
	time.Sleep(2 * time.Second)
	for _, j := range held {
		if st, start, ok := procStat(j.pid); ok && start == j.start && st != 'T' {
			s.errorf("pid %d was resumed while no throtd ran", j.pid)
		}
	}
	s.startDaemon()
	s.mu.Lock()
	s.instance++
	for _, j := range s.jobs {
		j.stoppedAt = time.Time{} // nothing could resume them while it was down
	}
	s.mu.Unlock()
	s.release(4 * soakInterval)
	s.phase.Unlock()
	s.t.Logf("[%s] throtd SIGKILLed and restarted with %d jobs stopped", time.Since(s.begin).Round(time.Second), len(held))

	// The new daemon resumes them from paused.json, at once.
	s.e.waitFor("jobs stopped at the crash to be resumed", func() bool {
		for _, j := range held {
			if st, start, ok := procStat(j.pid); ok && start == j.start && st == 'T' {
				return false
			}
		}
		return true
	})
}

// checkHeavy compares the heavy jobs running now with the limit.
func (s *soak) checkHeavy() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	settling := s.holds > 0 || now.Before(s.settleUntil)
	var running, exempt, young int
	var pids []string
	for k, j := range s.jobs {
		st, start, ok := procStat(j.pid)
		if !ok || start != j.start || st == 'Z' || st == 'X' {
			delete(s.jobs, k)
			continue
		}
		if st == 'T' {
			if j.stoppedAt.IsZero() {
				j.stoppedAt = now
			}
			if s.holds == 0 && !j.manual {
				held := now.Sub(j.stoppedAt)
				s.maxStopped = max(s.maxStopped, held)
				if held > soakStuck && !j.stuck {
					j.stuck = true
					s.t.Errorf("pid %d has been stopped for %s while throtd runs", j.pid, held.Round(time.Second))
				}
			}
			continue
		}
		j.stoppedAt = time.Time{}
		if comm(j.pid) != "make" {
			continue // a throt run still waiting for its lease
		}
		if j.firstRun.IsZero() {
			j.firstRun = now
		}
		if j.stderr != "" && !j.checked {
			// throt run writes this before it execs, so it is complete.
			b, _ := os.ReadFile(j.stderr)
			j.exempt = j.exempt || strings.Contains(string(b), "without waiting") || strings.Contains(string(b), "running anyway")
			j.checked = true
		}
		if settling {
			j.exempt = true
		}
		running++
		pids = append(pids, strconv.Itoa(j.pid))
		switch {
		case j.exempt:
			exempt++
		case now.Sub(j.firstRun) < soakTransient:
			young++
		}
	}
	s.maxRunning = max(s.maxRunning, running)
	if !settling {
		s.limitChecks++
		s.maxCounted = max(s.maxCounted, running-exempt-young)
	}
	if !settling && running > soakHeavyLimit+exempt+young && len(s.violations) < 20 {
		s.violations = append(s.violations, fmt.Sprintf("[%s] %d heavy jobs running (%s), %d exempt, %d new; limit %d",
			now.Sub(s.begin).Round(100*time.Millisecond), running, strings.Join(pids, " "), exempt, young, soakHeavyLimit))
	}
}

// sample records throtd's RSS, thread and file descriptor counts.
func (s *soak) sample() {
	s.mu.Lock()
	pid, instance := s.daemonPID, s.instance
	s.mu.Unlock()
	if pid == 0 {
		return
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return
	}
	smp := soakSample{at: time.Since(s.begin), instance: instance}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "VmRSS":
			smp.rss, _ = strconv.ParseInt(f[0], 10, 64)
		case "Threads":
			smp.threads, _ = strconv.Atoi(f[0])
		}
	}
	fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	smp.fds = len(fds)
	s.mu.Lock()
	s.samples = append(s.samples, smp)
	s.mu.Unlock()
}

// pollStatus times one throt status.
func (s *soak) pollStatus() {
	s.phase.RLock()
	defer s.phase.RUnlock()
	t0 := time.Now()
	out, err := s.e.throt("status")
	d := time.Since(t0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if len(s.statusFails) < 10 {
			s.statusFails = append(s.statusFails, err.Error())
		}
		return
	}
	s.latencies = append(s.latencies, soakLatency{at: t0.Sub(s.begin), d: d})
	p := parseStatus(out).pressure
	s.pressures[p]++
	if p == "UNKNOWN" {
		// Unknown pressure fails open: nothing new is queued.
		s.settleUntil = later(s.settleUntil, time.Now().Add(1500*time.Millisecond))
	}
}

// pauseResume pauses a random running agent job and resumes it, or with
// queued set, resumes a random job the scheduler queued.
func (s *soak) pauseResume(queued bool, intn func(int) int) {
	s.phase.RLock()
	defer s.phase.RUnlock()
	s.mu.Lock()
	var pick []*soakJob
	for _, j := range s.jobs {
		st, _, _ := procStat(j.pid)
		if j.stderr == "" && !j.manual && (st == 'T') == queued && st != 0 {
			pick = append(pick, j)
		}
	}
	slices.SortFunc(pick, func(a, b *soakJob) int { return cmp.Compare(a.pid, b.pid) })
	var j *soakJob
	if len(pick) > 0 {
		j = pick[intn(len(pick))]
		if queued {
			j.exempt = true // resumed by hand, beyond the scheduler's limit
		} else {
			j.manual = true
		}
	}
	s.mu.Unlock()
	if j == nil {
		return
	}
	pid := strconv.Itoa(j.pid)
	count := func(what string, err error) {
		key := what + " ok"
		if err != nil {
			key = what + " refused"
		}
		s.mu.Lock()
		s.pauses[key]++
		s.mu.Unlock()
	}
	if queued {
		_, err := s.e.throt("resume", pid)
		count("resume queued", err)
		return
	}
	_, err := s.e.throt("pause", pid)
	count("pause", err)
	if err == nil {
		time.Sleep(time.Duration(500+intn(1500)) * time.Millisecond)
		s.mu.Lock()
		j.exempt = true // its slot may have been given away meanwhile
		s.mu.Unlock()
		for range 10 {
			if _, err = s.e.throt("resume", pid); err == nil {
				break
			}
			if _, start, ok := procStat(j.pid); !ok || start != j.start {
				err = nil // gone
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		count("resume paused", err)
		if st, start, ok := procStat(j.pid); err != nil && ok && start == j.start && st == 'T' {
			s.errorf("pid %d stays stopped after throt resume: %v", j.pid, err)
		}
	}
	s.mu.Lock()
	j.manual = false
	s.mu.Unlock()
}

// throtRun runs one throt run caller to completion.
func (s *soak) throtRun() {
	s.phase.RLock()
	f, err := os.CreateTemp(s.runDir, "run")
	if err != nil {
		s.phase.RUnlock()
		s.errorf("%v", err)
		return
	}
	cmd := exec.Command(filepath.Join(binDir, "throt"), "run", "--timeout", "2m", "--", filepath.Join(binDir, "make"), "sleep", "1s")
	cmd.Env = s.e.vars
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	f.Close()
	if err == nil {
		s.addJob(cmd.Process.Pid, f.Name())
	}
	s.phase.RUnlock()
	if err != nil {
		s.errorf("throt run: %v", err)
		return
	}
	err = cmd.Wait()
	b, _ := os.ReadFile(f.Name())
	outcome := "admitted"
	switch {
	case err != nil:
		outcome = "failed"
		s.errorf("throt run: %v: %s", err, b)
	case strings.Contains(string(b), "without waiting"):
		outcome = "fail-open"
	case strings.Contains(string(b), "running anyway"):
		outcome = "timed out"
	case strings.Contains(string(b), "capacity available after"):
		outcome = "waited"
	}
	s.mu.Lock()
	s.runs[outcome]++
	s.mu.Unlock()
}

// toggleMode switches the scheduler to observe for d, then back to enforce.
func (s *soak) toggleMode(d time.Duration) {
	s.phase.RLock()
	defer s.phase.RUnlock()
	s.hold()
	defer s.release(2*soakInterval + time.Second)
	for _, mode := range []string{"observe", "enforce"} {
		s.e.writeConfig(soakConfig(mode))
		if out, err := s.e.throt("reload"); err != nil || !strings.Contains(out, "scheduler.mode") {
			s.errorf("reload to %s: %v\n%s", mode, err, out)
		}
		if mode == "observe" {
			time.Sleep(d)
		}
	}
}

// soakAgent is one running churn agent.
type soakAgent struct {
	mode  string
	pid   int
	stdin io.WriteCloser
	done  chan struct{}
	lite  string // the lite agent's final report
}

func (s *soak) startAgent(args ...string) *soakAgent {
	s.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, soakAgentExe), args...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	a := &soakAgent{mode: args[0], pid: cmd.Process.Pid, stdin: stdin, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		in := bufio.NewScanner(stdout)
		for in.Scan() {
			f := strings.Fields(in.Text())
			switch {
			case len(f) == 2 && f[0] == "start":
				if pid, err := strconv.Atoi(f[1]); err == nil {
					s.addJob(pid, "")
				}
			case len(f) == 4 && f[0] == "lite":
				a.lite = fmt.Sprintf("%s light processes, highest PID %s, PID space wrapped %s times", f[1], f[2], f[3])
			case len(f) > 0 && f[0] == "error":
				s.errorf("agent %d: %s", a.pid, in.Text())
			}
		}
		cmd.Wait()
	}()
	s.t.Cleanup(func() {
		syscall.Kill(-a.pid, syscall.SIGKILL)
		<-a.done
	})
	return a
}

// stoppedProcesses lists every process in state T, which in the test's
// container are all processes the test started.
func stoppedProcesses() []string {
	ents, _ := os.ReadDir("/proc")
	var out []string
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, _, ok := procStat(pid); ok && st == 'T' {
			out = append(out, fmt.Sprintf("%d (%s)", pid, comm(pid)))
		}
	}
	return out
}

// eventFollower runs throt events -f across the whole soak and checks the
// sequence numbers it prints.
type eventFollower struct {
	cmd    *exec.Cmd
	stderr *lockedBuffer
	done   chan struct{}

	// Written by the reader goroutine until done is closed.
	events, instances, gaps int
	backwards               []string
	kinds, decisions        map[string]int
}

func (s *soak) followEvents() *eventFollower {
	s.t.Helper()
	f := &eventFollower{stderr: &lockedBuffer{}, done: make(chan struct{}), kinds: map[string]int{}, decisions: map[string]int{}}
	f.cmd = exec.Command(filepath.Join(binDir, "throt"), "events", "-f", "-n", "1000")
	f.cmd.Env = s.e.vars
	f.cmd.Stderr = f.stderr
	stdout, err := f.cmd.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	if err := f.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() {
		defer close(f.done)
		var instance string
		var last uint64
		in := bufio.NewScanner(stdout)
		in.Buffer(make([]byte, 64<<10), 1<<20)
		for in.Scan() {
			var ev struct {
				Seq      uint64            `json:"seq"`
				Instance string            `json:"instance"`
				Kind     string            `json:"kind"`
				Attrs    map[string]string `json:"attrs"`
			}
			if err := json.Unmarshal(in.Bytes(), &ev); err != nil {
				f.backwards = append(f.backwards, "unparsable line: "+in.Text())
				continue
			}
			f.events++
			f.kinds[ev.Kind]++
			if ev.Kind == "scheduler_decision" && ev.Attrs["observed"] == "false" {
				f.decisions[ev.Attrs["decision"]+" "+ev.Attrs["reason"]]++
			}
			if ev.Kind == "lease_released" {
				f.decisions["lease "+ev.Attrs["reason"]]++
			}
			switch {
			case ev.Instance != instance:
				instance = ev.Instance
				f.instances++
			case ev.Seq <= last:
				if len(f.backwards) < 10 {
					f.backwards = append(f.backwards, fmt.Sprintf("seq %d after %d in instance %s", ev.Seq, last, instance))
				}
			case ev.Seq > last+1:
				f.gaps += int(ev.Seq - last - 1)
			}
			last = ev.Seq
		}
		f.cmd.Wait()
	}()
	s.t.Cleanup(func() { f.stop() })
	return f
}

func (f *eventFollower) stop() {
	select {
	case <-f.done:
		return
	default:
	}
	f.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		f.cmd.Process.Kill()
		<-f.done
	}
}

func (f *eventFollower) check(s *soak) {
	<-f.done
	if len(f.backwards) > 0 {
		s.errorf("event seq not monotonic within an instance: %v", f.backwards)
	}
	if f.instances != 2 {
		s.errorf("events came from %d daemon instances, want 2 (one restart)", f.instances)
	}
	if f.gaps > 0 {
		s.errorf("throt events -f missed %d events", f.gaps)
	}
	if !strings.Contains(f.stderr.String(), "throtd restarted") {
		s.errorf("throt events -f did not report the restart:\n%s", f.stderr.String())
	}
	s.t.Logf("events: %d from %d instances; kinds %v", f.events, f.instances, f.kinds)
	s.t.Logf("scheduler actions: %v", f.decisions)
}

// thirds splits xs into its first, middle and last third.
func thirds[T any](xs []T) [3][]T {
	n := len(xs)
	return [3][]T{xs[:n/3], xs[n/3 : n-n/3], xs[n-n/3:]}
}

func maxOf[T any](xs []T, f func(T) int64) int64 {
	var m int64
	for _, x := range xs {
		m = max(m, f(x))
	}
	return m
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	ds = slices.Sorted(slices.Values(ds))
	return ds[min(len(ds)-1, int(float64(len(ds))*p))]
}

// goroutineSamples reads the THROT_DEBUG_GOROUTINES lines from throtd's
// log, per daemon instance (each starts with "listening on").
func goroutineSamples(logPath string) [][]int64 {
	b, _ := os.ReadFile(logPath)
	var out [][]int64
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(line, "msg=\"listening on") {
			out = append(out, nil)
		}
		if _, n, ok := strings.Cut(line, "msg=goroutines n="); ok && len(out) > 0 {
			if v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
				out[len(out)-1] = append(out[len(out)-1], v)
			}
		}
	}
	return out
}

func tail(path string, n int) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// plateau checks that a resource does not grow: the last third's maximum
// may exceed the first third's by ratio, or by slack for small counts.
// The first sample of each daemon is start-up and is skipped.
func (s *soak) plateau(what string, instance int, xs []int64, ratio float64, slack int64) {
	if len(xs) < 4 {
		s.t.Logf("%s of daemon %d: only %d samples, not checked", what, instance, len(xs))
		return
	}
	th := thirds(xs[1:])
	first, mid, last := maxOf(th[0], id), maxOf(th[1], id), maxOf(th[2], id)
	s.t.Logf("%s of daemon %d: max %d / %d / %d over thirds (%d samples)", what, instance, first, mid, last, len(xs))
	if float64(last) > float64(first)*ratio && last > first+slack {
		s.errorf("%s of daemon %d grows: last third max %d > %.1f x first third max %d", what, instance, last, ratio, first)
	}
}

func id(v int64) int64 { return v }

func (s *soak) report(logPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.violations {
		s.t.Errorf("heavy limit exceeded: %s", v)
	}
	if len(s.statusFails) > 0 {
		s.t.Errorf("throt status failed while throtd ran: %v", s.statusFails)
	}
	if s.pauses["pause ok"] == 0 {
		s.t.Errorf("no manual pause succeeded: %v", s.pauses)
	}

	// Memory, threads and file descriptors, per daemon instance, since a
	// restarted daemon starts small again.
	for inst := 0; inst <= s.instance; inst++ {
		var rss, threads, fds []int64
		for _, smp := range s.samples {
			if smp.instance == inst {
				rss, threads, fds = append(rss, smp.rss), append(threads, int64(smp.threads)), append(fds, int64(smp.fds))
			}
		}
		// The Go heap is GC-paced at 2x live data, so RSS of a stable
		// working set wanders well within 1.5x; a leak proportional to
		// churn shows as steady growth past it over the run.
		s.plateau("RSS KiB", inst, rss, 1.5, 0)
		// Thread counts are small and rise with blocking syscalls in
		// flight; allow a few.
		s.plateau("threads", inst, threads, 1.5, 4)
		s.plateau("fds", inst, fds, 1.5, 8)
	}
	for inst, gs := range goroutineSamples(logPath) {
		// Each client connection holds a goroutine or two while it is
		// served, so allow a handful above the first third.
		s.plateau("goroutines", inst, gs, 1.5, 16)
	}

	th := thirds(s.latencies)
	var all []time.Duration
	parts := make([]string, 3)
	for i, part := range th {
		var ds []time.Duration
		for _, l := range part {
			ds = append(ds, l.d)
		}
		all = append(all, ds...)
		parts[i] = fmt.Sprintf("p50 %s p99 %s max %s", percentile(ds, 0.5).Round(time.Millisecond),
			percentile(ds, 0.99).Round(time.Millisecond), percentile(ds, 1).Round(time.Millisecond))
	}
	s.t.Logf("throt status latency (%d calls) by third: %s", len(all), strings.Join(parts, " | "))
	if p99 := percentile(all, 0.99); p99 >= time.Second {
		s.t.Errorf("throt status p99 latency %s, want < 1s", p99)
	}
	s.t.Logf("jobs started %d, max running %d (limit %d; at most %d neither exempt nor new, over %d checks), longest stopped %s; pressure polls %v",
		s.spawned, s.maxRunning, soakHeavyLimit, s.maxCounted, s.limitChecks, s.maxStopped.Round(100*time.Millisecond), s.pressures)
	s.t.Logf("manual control %v; throt run %v; %s", s.pauses, s.runs, s.lite)
	var sm []string
	for _, smp := range s.samples {
		sm = append(sm, fmt.Sprintf("%s:%d/%dKiB/%dthr/%dfd", smp.at.Round(time.Second), smp.instance, smp.rss, smp.threads, smp.fds))
	}
	s.t.Logf("samples (time:daemon/RSS/threads/fds): %s", strings.Join(sm, " "))
	s.t.Logf("goroutines per daemon: %v", goroutineSamples(logPath))
}
