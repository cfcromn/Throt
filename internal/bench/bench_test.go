// Package bench holds Throt's real-kernel benchmark: real processes, the real
// kernel scheduler and the real daemon. It only runs with THROT_BENCH=1 in a
// disposable environment: a Linux container (see `make bench`), or a hosted
// CI runner (CI=true) on Linux or macOS (.github/workflows/bench.yml),
// because an enforce-mode daemon pauses any build attributed to an agent
// named claude.
//
// TestBenchMemory deliberately drives the machine past its RAM, so it also
// needs THROT_BENCH_MEMORY=1, and outside CI it runs only in a Linux
// container with a memory limit (docker run --memory), which it sizes from.
package bench

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/platform"
	"github.com/cfcromn/Throt/internal/transport"
)

// jobSource is the heavy job, named make so Throt classifies it as a build.
// It burns one core for a fixed number of iterations, so its work is CPU
// time rather than wall time: a paused or starved job takes longer but does
// the same work. With -calibrate it prints iterations per CPU second.
//
// With -mem BYTES PASSES CAP it is a memory job: it allocates BYTES, writes
// every word with incompressible data (so neither zswap nor the macOS
// compressor can shrink it), then writes one word per page for PASSES passes
// over the buffer, keeping its whole working set hot, and exits. A swapped
// out or paused job takes longer but does the same work. CAP is a hard
// limit on wall time spent not stopped: the job exits with status 3 when it
// is reached, so a lost job cannot hold memory for long. Time stopped
// (queued by Throt) does not count: a stopped job holds its memory either
// way, and counting it would report a queued job that never ran as one that
// ran out of time. With -mem-calibrate BYTES it prints passes per second
// for a buffer of that size.
const jobSource = `package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

func fill(size uint64) []uint64 {
	buf := make([]uint64, size/8)
	x := uint64(0x9e3779b97f4a7c15)
	for i := range buf {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf[i] = x
	}
	return buf
}

func touch(buf []uint64) {
	stride := os.Getpagesize() / 8
	for i := 0; i < len(buf); i += stride {
		buf[i]++
	}
}

func arg(i int) uint64 {
	n, err := strconv.ParseUint(os.Args[i], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return n
}

// capAfter exits with status 3 once limit of wall time has passed outside
// SIGSTOP. A stop shows as the tick gap that a SIGCONT ends; the overdue
// tick may arrive before the signal, so a long gap waits one tick for it.
// Thrashing delays ticks without a SIGCONT, so it still counts.
func capAfter(limit time.Duration) {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	tick := time.NewTicker(100 * time.Millisecond)
	deadline, last := time.Now().Add(limit), time.Now()
	var gap time.Duration
	for {
		select {
		case <-tick.C:
			// Not the tick's own time: a tick sent just before the stop
			// waits in the channel and would hide the gap.
			now := time.Now()
			gap, last = now.Sub(last), now
			if gap > time.Second {
				continue
			}
		case <-cont:
			now := time.Now()
			deadline = deadline.Add(max(gap, now.Sub(last)))
			gap, last = 0, now
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "make: memory job hit its %s cap\n", limit)
			os.Exit(3)
		}
	}
}

func memJob() {
	go capAfter(time.Duration(arg(4)) * time.Second)
	buf := fill(arg(2))
	for range arg(3) {
		touch(buf)
	}
	runtime.KeepAlive(buf)
}

func memCalibrate() {
	time.AfterFunc(2*time.Minute, func() { os.Exit(3) })
	buf := fill(arg(2))
	touch(buf)
	start := time.Now()
	n := 0
	for time.Since(start) < time.Second {
		touch(buf)
		n++
	}
	fmt.Println(uint64(float64(n) / time.Since(start).Seconds()))
	runtime.KeepAlive(buf)
}

var sink uint64

func spin(n uint64) {
	x := sink
	for i := uint64(0); i < n; i++ {
		x = x*6364136223846793005 + 1442695040888963407
	}
	sink = x
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-calibrate" {
		const n = 500_000_000
		start := cpuTime()
		spin(n)
		fmt.Println(uint64(float64(n) / (cpuTime() - start).Seconds()))
		return
	}
	if len(os.Args) == 5 && os.Args[1] == "-mem" {
		memJob()
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "-mem-calibrate" {
		memCalibrate()
		return
	}
	spin(arg(1))
}
`

// agentSource is the agent stub, named claude so Throt attributes its
// children to it. Each stdin line is a job's arguments; it starts ./make
// with them and reports "start PID UNIXNANO" and "exit PID UNIXNANO STATUS",
// where STATUS is the exit status, or -1 when a signal killed the job.
const agentSource = `package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	self, _ := os.Executable()
	job := filepath.Join(filepath.Dir(self), "make")
	var mu sync.Mutex
	report := func(what string, pid int, extra string) {
		mu.Lock()
		fmt.Printf("%s %d %d%s\n", what, pid, time.Now().UnixNano(), extra)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		cmd := exec.Command(job, strings.Fields(in.Text())...)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report("start", cmd.Process.Pid, "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd.Wait()
			report("exit", cmd.Process.Pid, fmt.Sprintf(" %d", cmd.ProcessState.ExitCode()))
		}()
	}
	wg.Wait()
}
`

// probeSource is the foreground responsiveness probe. It sleeps 10ms in a
// loop and, when stdin closes, prints how late each wakeup was in
// microseconds, one per line.
const probeSource = `package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

func main() {
	var done atomic.Bool
	go func() {
		io.Copy(io.Discard, os.Stdin)
		done.Store(true)
	}()
	const d = 10 * time.Millisecond
	var late []int64
	for !done.Load() {
		start := time.Now()
		time.Sleep(d)
		late = append(late, max(time.Since(start)-d, 0).Microseconds())
	}
	for _, l := range late {
		fmt.Println(l)
	}
}
`

const (
	sampleInterval = 500 * time.Millisecond
	jobCPU         = 5 * time.Second
	runTimeout     = 10 * time.Minute
	// settleTimeout bounds the wait for pressure below high before a run.
	settleTimeout = 3 * time.Minute
	defaultReps   = 3

	// The memory scenarios: memJobs jobs of memShare percent of RAM each,
	// so all of them together want 8 x 22% = 176% of it. Each keeps its
	// working set hot for about memHold of uncontended work, and exits at
	// memJobCap of wall time outside SIGSTOP however far it got. The last
	// job may wait out the scheduler's max_queue_wait (5m by default) and
	// then run to its cap: memRunTimeout leaves room for that.
	memJobs       = 8
	memShare      = 22
	memHold       = 6 * time.Second
	memEvery      = time.Second
	memJobCap     = 3 * time.Minute
	memRunTimeout = 10 * time.Minute
)

// scenario is a set of job arrivals, as offsets from the first launch.
type scenario struct {
	name     string
	arrivals []time.Duration
}

// sizing is the contention for one machine.
type sizing struct {
	cpus       int // CPUs the jobs compete for
	burst      int // jobs launched at once
	stagger    int // jobs in the staggered stream
	every      time.Duration
	heavyLimit int // Throt's scheduler.heavy_limit; 0 keeps the default profile
	reps       int
}

// sizeFor picks the contention. In a container (make bench, --cpus=2) it
// keeps the fixed scenarios of the published results: NumCPU there is the
// host's, not the quota. On a CI runner, which has no CPU limit, it scales
// to the machine: 4x NumCPU jobs at once, and a stream of as many whose
// arrival rate asks for about twice the CPUs, so both oversubscribe.
// THROT_BENCH_DEFAULTS=1 sizes a container the same way, with the shipped
// defaults; give it a cpuset (docker run --cpuset-cpus) so NumCPU, and so
// the balanced profile, matches its CPUs. THROT_BENCH_JOBS and
// THROT_BENCH_REPS override the job count of both scenarios and the
// repetitions, for a quick run.
func sizeFor(container bool) (sizing, error) {
	z := sizing{cpus: 2, burst: 8, stagger: 10, every: time.Second, heavyLimit: 2, reps: defaultReps}
	if !container || os.Getenv("THROT_BENCH_DEFAULTS") == "1" {
		n := runtime.NumCPU()
		// On a real machine measure what a user gets: the default profile
		// (adaptive, ceiling = CPUs), sample interval and cooldown.
		z = sizing{cpus: n, burst: 4 * n, stagger: 4 * n, reps: defaultReps}
		z.every = (jobCPU / time.Duration(2*n)).Round(10 * time.Millisecond)
	}
	positive := func(env string) (int, error) {
		v := os.Getenv(env)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s=%q: want a positive integer", env, v)
		}
		return n, nil
	}
	jobs, err := positive("THROT_BENCH_JOBS")
	if err != nil {
		return z, err
	}
	if jobs > 0 {
		z.burst, z.stagger = jobs, jobs
	}
	reps, err := positive("THROT_BENCH_REPS")
	if err != nil {
		return z, err
	}
	if reps > 0 {
		z.reps = reps
	}
	return z, nil
}

func scenarios(z sizing) []scenario {
	burst := make([]time.Duration, z.burst)
	stagger := make([]time.Duration, z.stagger)
	for i := range stagger {
		stagger[i] = time.Duration(i) * z.every
	}
	return []scenario{
		{fmt.Sprintf("%d at once", z.burst), burst},
		{fmt.Sprintf("%d staggered %s", z.stagger, z.every), stagger},
	}
}

// result is one run's measurements.
type result struct {
	p50, p95, p99 time.Duration
	makespan      time.Duration
	peak          int
	interventions int
	maxPressure   domain.PressureLevel
	// Swap as the daemon's host sampler saw it: the peak used during the
	// run, and its growth over the last sample before the first job.
	swapKnown  bool
	peakSwap   uint64
	swapGrowth uint64
	capped     int // jobs that hit their hard cap (memory jobs only)
	killed     int // jobs killed by a signal, e.g. the OOM killer
}

// benchEnv skips unless the benchmark may run here, and reports whether
// this is a Linux container. An enforce-mode daemon pauses builds of any
// process named claude, so only a disposable environment qualifies: a Linux
// container, whose PID namespace is isolated, or a hosted CI runner, which
// is a throwaway VM.
func benchEnv(t *testing.T) (container bool) {
	t.Helper()
	if os.Getenv("THROT_BENCH") != "1" {
		t.Skip("set THROT_BENCH=1 and run inside a container (make bench) or on a CI runner (bench.yml)")
	}
	container = runtime.GOOS == "linux" && inContainer()
	ci := os.Getenv("CI") == "true" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin")
	if !container && !ci {
		t.Skip("the benchmark only runs in a Linux container (make bench) or on a Linux or macOS CI runner with CI=true (gh workflow run bench.yml)")
	}
	return container
}

func TestBench(t *testing.T) {
	container := benchEnv(t)
	z, err := sizeFor(container)
	if err != nil {
		t.Fatal(err)
	}
	bin := buildHelpers(t)
	iters := calibrate(t, bin)
	t.Logf("calibration: %d iterations per CPU second; GOMAXPROCS %d, NumCPU %d", iters, runtime.GOMAXPROCS(0), runtime.NumCPU())
	line := strconv.FormatUint(uint64(float64(iters)*jobCPU.Seconds()), 10)
	tune := func(c *config.Config) {
		if z.heavyLimit == 0 {
			return // defaults, as shipped
		}
		c.SampleInterval = config.Duration(sampleInterval)
		c.Scheduler.HeavyLimit = z.heavyLimit
		c.Scheduler.StartLimit = min(c.Scheduler.StartLimit, z.heavyLimit)
		c.Scheduler.ResumeCooldown = config.Duration(time.Second)
	}

	var table strings.Builder
	limits := fmt.Sprintf("heavy_limit %d, 500ms sampling, 1s cooldown", z.heavyLimit)
	if z.heavyLimit == 0 {
		d := config.Default()
		limits = fmt.Sprintf("default profile (%s: adaptive, start %d, ceiling %d, %s sampling)",
			d.Profile, d.Scheduler.StartLimit, d.Scheduler.HeavyLimit, time.Duration(d.SampleInterval))
	}
	fmt.Fprintf(&table, "\n%s/%s, %s, %d CPUs, %s, %s of CPU per job, medians of %d runs\n\n",
		runtime.GOOS, runtime.GOARCH, kernel(), z.cpus, limits, jobCPU, z.reps)
	table.WriteString("| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure |\n")
	table.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, sc := range scenarios(z) {
		for _, mode := range []string{"off", "enforce"} {
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				var runs []result
				for i := range z.reps {
					t.Run("run"+strconv.Itoa(i+1), func(t *testing.T) {
						runs = append(runs, runScenario(t, bin, sc, mode, line, tune, runTimeout))
					})
				}
				if len(runs) != z.reps {
					t.Fatalf("%d of %d runs finished", len(runs), z.reps)
				}
				r := medianResult(runs)
				label := "baseline"
				if mode == "enforce" {
					label = "Throt"
				}
				lo := slices.MinFunc(runs, byMakespan).makespan
				hi := slices.MaxFunc(runs, byMakespan).makespan
				fmt.Fprintf(&table, "| %s | %s | %s | %s | %s | %s | %s–%s | %d | %d | %s |\n",
					sc.name, label, ms(r.p50), ms(r.p95), ms(r.p99), secs(r.makespan), secs(lo), secs(hi),
					r.peak, r.interventions, r.maxPressure)
			})
		}
	}
	t.Log(table.String())
	writeOut(t, table.String())
}

// TestBenchMemory measures the case Throt exists for: builds that together
// exceed RAM, so the machine swaps. Each job takes memShare percent of the
// memory limit (the cgroup's in a container, the machine's on a CI runner)
// and memJobs of them run at once or staggered, with the scheduler off or
// enforcing the default balanced profile.
func TestBenchMemory(t *testing.T) {
	container := benchEnv(t)
	if os.Getenv("THROT_BENCH_MEMORY") != "1" {
		t.Skip("set THROT_BENCH_MEMORY=1 as well: these scenarios drive the machine past its RAM")
	}
	total, source, err := memLimit(container)
	if err != nil {
		t.Skip(err)
	}
	reps := defaultReps
	if v := os.Getenv("THROT_BENCH_REPS"); v != "" {
		if reps, err = strconv.Atoi(v); err != nil || reps < 1 {
			t.Fatalf("THROT_BENCH_REPS=%q: want a positive integer", v)
		}
	}
	size := total / 100 * memShare
	bin := buildHelpers(t)
	rate := calibrateMemory(t, bin, size)
	passes := max(1, uint64(float64(rate)*memHold.Seconds()))
	t.Logf("memory: %s from %s, %s per job, %d passes per second alone", mib(total), source, mib(size), rate)
	line := fmt.Sprintf("-mem %d %d %d", size, passes, int(memJobCap.Seconds()))
	heavyLimit := config.Default().Scheduler.HeavyLimit

	burst := make([]time.Duration, memJobs)
	stagger := make([]time.Duration, memJobs)
	for i := range stagger {
		stagger[i] = time.Duration(i) * memEvery
	}
	var table strings.Builder
	fmt.Fprintf(&table, "\n%s/%s, %s, %s memory (%s), %d jobs of %s (%d%% each), balanced profile (heavy_limit %d), medians of %d runs\n\n",
		runtime.GOOS, runtime.GOARCH, kernel(), mib(total), source, memJobs, mib(size), memShare, heavyLimit, reps)
	table.WriteString("| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure | Peak swap | Swap growth | Capped / killed |\n")
	table.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, sc := range []scenario{
		{fmt.Sprintf("memory burst, %d at once", memJobs), burst},
		{fmt.Sprintf("memory staggered, %d one per %s", memJobs, memEvery), stagger},
	} {
		for _, mode := range []string{"off", "enforce"} {
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				var runs []result
				for i := range reps {
					t.Run("run"+strconv.Itoa(i+1), func(t *testing.T) {
						runs = append(runs, runScenario(t, bin, sc, mode, line, nil, memRunTimeout))
					})
				}
				if len(runs) != reps {
					t.Fatalf("%d of %d runs finished", len(runs), reps)
				}
				r := medianResult(runs)
				label := "baseline"
				if mode == "enforce" {
					label = "Throt"
				}
				lo := slices.MinFunc(runs, byMakespan).makespan
				hi := slices.MaxFunc(runs, byMakespan).makespan
				peakSwap, growth := "n/a", "n/a"
				if r.swapKnown {
					peakSwap, growth = mib(r.peakSwap), mib(r.swapGrowth)
				}
				fmt.Fprintf(&table, "| %s | %s | %s | %s | %s | %s | %s–%s | %d | %d | %s | %s | %s | %d / %d |\n",
					sc.name, label, ms(r.p50), ms(r.p95), ms(r.p99), secs(r.makespan), secs(lo), secs(hi),
					r.peak, r.interventions, r.maxPressure, peakSwap, growth, r.capped, r.killed)
			})
		}
	}
	t.Log(table.String())
	writeOut(t, table.String())
}

// writeOut appends a results table to THROT_BENCH_OUT, if set, so that
// TestBench and TestBenchMemory share one file.
func writeOut(t *testing.T, s string) {
	t.Helper()
	out := os.Getenv("THROT_BENCH_OUT")
	if out == "" {
		return
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// memLimit returns the memory the jobs compete for and where it came from.
// In a container that is the cgroup limit, never the host's memory, and a
// container without one is refused: the scenarios would exhaust the whole
// VM or machine. On a CI runner it is hw.memsize on macOS and MemTotal from
// /proc/meminfo on Linux.
func memLimit(container bool) (uint64, string, error) {
	if container {
		for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			v := strings.TrimSpace(string(b))
			n, err := strconv.ParseUint(v, 10, 64)
			// cgroup v1 reports no limit as a huge number.
			if v == "max" || err != nil || n == 0 || n >= 1<<62 {
				break
			}
			return n, "cgroup " + filepath.Base(f), nil
		}
		return 0, "", fmt.Errorf("the memory scenarios need a container memory limit (docker run --memory=1g --memory-swap=2g)")
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, "", fmt.Errorf("sysctl hw.memsize: %w", err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil || n == 0 {
			return 0, "", fmt.Errorf("hw.memsize %q: %v", out, err)
		}
		return n, "hw.memsize", nil
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, "", err
		}
		for line := range strings.Lines(string(b)) {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == "MemTotal:" && f[2] == "kB" {
				n, err := strconv.ParseUint(f[1], 10, 64)
				if err == nil && n > 0 {
					return n << 10, "/proc/meminfo", nil
				}
			}
		}
		return 0, "", fmt.Errorf("/proc/meminfo has no MemTotal")
	}
	return 0, "", fmt.Errorf("no memory size on %s", runtime.GOOS)
}

// calibrateMemory measures a lone memory job's passes per second over a
// buffer of size bytes, best of two, before any contention starts.
func calibrateMemory(t *testing.T, bin string, size uint64) uint64 {
	t.Helper()
	var best uint64
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		out, err := exec.CommandContext(ctx, filepath.Join(bin, "make"), "-mem-calibrate", strconv.FormatUint(size, 10)).Output()
		cancel()
		if err != nil {
			t.Fatalf("calibrate memory: %v", err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			t.Fatalf("calibrate memory: %v", err)
		}
		best = max(best, n)
	}
	return best
}

func mib(b uint64) string { return strconv.FormatUint(b>>20, 10) + " MiB" }

// swapRecorder passes the daemon's host samples through and records the
// swap they report, so the benchmark measures swap with the same sampler
// (cgroup-aware in a container) that Throt decides from.
type swapRecorder struct {
	platform.HostSampler
	mu               sync.Mutex
	known            bool
	last, base, peak uint64
}

func (s *swapRecorder) SampleHost(ctx context.Context) (domain.HostSample, error) {
	h, err := s.HostSampler.SampleHost(ctx)
	if err == nil && h.Has(domain.MetricSwap) {
		s.mu.Lock()
		s.known, s.last = true, h.SwapUsed
		s.peak = max(s.peak, h.SwapUsed)
		s.mu.Unlock()
	}
	return h, err
}

// mark starts the measured window at the latest sample.
func (s *swapRecorder) mark() {
	s.mu.Lock()
	s.base, s.peak = s.last, s.last
	s.mu.Unlock()
}

// result reports the window's peak swap and its growth over the mark.
func (s *swapRecorder) result() (known bool, peak, growth uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.known, s.peak, s.peak - s.base
}

func byMakespan(a, b result) int { return cmp.Compare(a.makespan, b.makespan) }

// medianResult takes the median of each measurement separately, so one
// row's columns may come from different runs.
func medianResult(runs []result) result {
	med := func(f func(result) int64) int64 {
		v := make([]int64, len(runs))
		for i, r := range runs {
			v[i] = f(r)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	return result{
		p50:           time.Duration(med(func(r result) int64 { return int64(r.p50) })),
		p95:           time.Duration(med(func(r result) int64 { return int64(r.p95) })),
		p99:           time.Duration(med(func(r result) int64 { return int64(r.p99) })),
		makespan:      time.Duration(med(func(r result) int64 { return int64(r.makespan) })),
		peak:          int(med(func(r result) int64 { return int64(r.peak) })),
		interventions: int(med(func(r result) int64 { return int64(r.interventions) })),
		maxPressure:   domain.PressureLevel(med(func(r result) int64 { return int64(r.maxPressure) })),
		swapKnown:     slices.ContainsFunc(runs, func(r result) bool { return r.swapKnown }),
		peakSwap:      uint64(med(func(r result) int64 { return int64(r.peakSwap) })),
		swapGrowth:    uint64(med(func(r result) int64 { return int64(r.swapGrowth) })),
		capped:        int(med(func(r result) int64 { return int64(r.capped) })),
		killed:        int(med(func(r result) int64 { return int64(r.killed) })),
	}
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + "ms"
}

func secs(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }

// kernel is `uname -sr`, for the results header.
func kernel() string {
	out, err := exec.Command("uname", "-sr").Output()
	if err != nil {
		return "unknown kernel"
	}
	return strings.TrimSpace(string(out))
}

func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// buildHelpers compiles the job, agent stub and probe into a temp dir.
func buildHelpers(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	for name, src := range map[string]string{"make": jobSource, "claude": agentSource, "probe": probeSource} {
		srcDir := filepath.Join(dir, "src", name)
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, body := range map[string]string{"main.go": src, "go.mod": "module " + name + "\n\ngo 1.26\n"} {
			if err := os.WriteFile(filepath.Join(srcDir, file), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(gobin, "build", "-o", filepath.Join(bin, name), ".")
		cmd.Dir = srcDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}
	return bin
}

// calibrate measures job iterations per CPU second, best of three, before
// any contention starts.
func calibrate(t *testing.T, bin string) uint64 {
	t.Helper()
	var best uint64
	for range 3 {
		out, err := exec.Command(filepath.Join(bin, "make"), "-calibrate").Output()
		if err != nil {
			t.Fatalf("calibrate: %v", err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			t.Fatalf("calibrate: %v", err)
		}
		best = max(best, n)
	}
	return best
}

// startGroup starts cmd in its own process group and kills the whole group
// at cleanup, including stopped members.
func startGroup(t *testing.T, cmd *exec.Cmd) (stdin io.WriteCloser, stdout io.Reader) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
	return stdin, stdout
}

// startDaemon runs throtd in process with the real platform adapters and a
// private socket, and stops it at cleanup. tune, if set, changes the
// default config; mode and the sample interval are always set.
func startDaemon(t *testing.T, mode string, tune func(*config.Config)) (string, *swapRecorder) {
	t.Helper()
	// macOS limits socket paths to 104 bytes, and its $TMPDIR is long.
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	runtimeDir, err := os.MkdirTemp(base, "throtbench")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	cfg := config.Default()
	cfg.SocketPath = config.DefaultSocketPath()
	cfg.Scheduler.Mode = mode
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	procs, ctl := platform.NewProcessTools()
	swap := &swapRecorder{HostSampler: platform.NewHostSampler()}
	d := app.NewDaemon(cfg, swap, procs, ctl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon: %v", err)
		}
	})
	return cfg.SocketPath, swap
}

func status(socket string) (app.StatusResult, error) {
	var st app.StatusResult
	err := transport.Call(context.Background(), socket, "status", nil, &st)
	return st, err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// jobTracker follows the agent stub's reports.
type jobTracker struct {
	mu          sync.Mutex
	live        map[int]bool
	started     int
	exited      int
	capped      int // exit status 3: the job's hard cap
	killed      int // exit status -1: killed by a signal
	first, last time.Time
	done        chan struct{} // closed when every expected job has exited
}

func (j *jobTracker) read(r io.Reader, want int) {
	in := bufio.NewScanner(r)
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		ns, _ := strconv.ParseInt(f[2], 10, 64)
		at := time.Unix(0, ns)
		j.mu.Lock()
		switch f[0] {
		case "start":
			j.live[pid] = true
			if j.started == 0 {
				j.first = at
			}
			j.started++
		case "exit":
			delete(j.live, pid)
			j.exited++
			if len(f) == 4 {
				switch f[3] {
				case "3":
					j.capped++
				case "-1":
					j.killed++
				}
			}
			j.last = at
			if j.exited == want {
				close(j.done)
			}
		}
		j.mu.Unlock()
	}
}

// running counts live jobs that are not stopped: from /proc on Linux, from
// ps elsewhere.
func (j *jobTracker) running() int {
	j.mu.Lock()
	pids := make([]int, 0, len(j.live))
	for pid := range j.live {
		pids = append(pids, pid)
	}
	j.mu.Unlock()
	if runtime.GOOS != "linux" {
		return runningPS(pids)
	}
	n := 0
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			continue
		}
		i := bytes.LastIndexByte(b, ')')
		if i < 0 || i+2 >= len(b) {
			continue
		}
		switch b[i+2] {
		case 'R', 'S', 'D':
			n++
		}
	}
	return n
}

// runningPS counts the pids that ps reports as a job in a running or
// sleeping state (R, S, I or U on macOS; T is stopped, Z has exited). ps
// exits non-zero when a pid is already gone, so its output is used anyway.
func runningPS(pids []int) int {
	if len(pids) == 0 {
		return 0
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	out, _ := exec.Command("ps", "-o", "state=,comm=", "-p", strings.Join(list, ",")).Output()
	n := 0
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) < 2 || filepath.Base(f[len(f)-1]) != "make" {
			continue
		}
		switch f[0][0] {
		case 'R', 'S', 'I', 'U', 'D':
			n++
		}
	}
	return n
}

// runScenario launches sc's jobs, each with the arguments in line, under a
// daemon in mode, and measures them. It fails if they take over timeout;
// cleanup kills the agent's whole process group, jobs included.
func runScenario(t *testing.T, bin string, sc scenario, mode, line string, tune func(*config.Config), timeout time.Duration) result {
	socket, swap := startDaemon(t, mode, tune)
	// The scheduler never queues while pressure is unknown, so wait out the
	// warm-up before any job starts.
	waitFor(t, "known pressure", func() bool {
		st, err := status(socket)
		return err == nil && st.Pressure != domain.PressureUnknown
	})
	// The previous run's thrashing outlasts it (PSI averages, swap-in), and
	// a run started at high pressure measures that run: Throt would queue
	// every job on it. Give the machine time to settle first.
	for settle := time.Now().Add(settleTimeout); ; time.Sleep(500 * time.Millisecond) {
		st, err := status(socket)
		if err == nil && st.Pressure < domain.PressureHigh {
			break
		}
		if time.Now().After(settle) {
			t.Logf("pressure still %s (%s) after %s; starting anyway", st.Pressure, st.Reason, settleTimeout)
			break
		}
	}
	if st, _ := status(socket); len(st.Heavy) > 0 {
		// e.g. running under "go test", whose go process is itself a build.
		t.Fatalf("a heavy workload (%s) is already running; it would take a slot", st.Heavy[0].Name)
	}

	agentIn, agentOut := startGroup(t, exec.Command(filepath.Join(bin, "claude")))
	waitFor(t, "agent attribution", func() bool {
		st, err := status(socket)
		return err == nil && len(st.Agents) > 0
	})
	jobs := &jobTracker{live: map[int]bool{}, done: make(chan struct{})}
	go jobs.read(agentOut, len(sc.arrivals))

	probeCmd := exec.Command(filepath.Join(bin, "probe"))
	probeIn, probeOut := startGroup(t, probeCmd)
	var probeData []byte
	probeRead := make(chan struct{})
	go func() {
		probeData, _ = io.ReadAll(probeOut)
		close(probeRead)
	}()

	stopSampling := make(chan struct{})
	peak := make(chan int, 1)
	go func() {
		p := 0
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopSampling:
				peak <- p
				return
			case <-tick.C:
				p = max(p, jobs.running())
			}
		}
	}()

	swap.mark()
	begin := time.Now()
	line += "\n"
	for _, at := range sc.arrivals {
		time.Sleep(time.Until(begin.Add(at)))
		if _, err := io.WriteString(agentIn, line); err != nil {
			t.Fatalf("launch job: %v", err)
		}
	}
	select {
	case <-jobs.done:
	case <-time.After(timeout):
		jobs.mu.Lock()
		started, exited := jobs.started, jobs.exited
		jobs.mu.Unlock()
		if st, err := status(socket); err == nil {
			t.Logf("at timeout: pressure %s (%s), %d heavy, %d paused, scheduler %+v",
				st.Pressure, st.Reason, len(st.Heavy), len(st.Paused), st.Scheduler)
		}
		t.Fatalf("jobs did not finish within %s: %d started, %d exited", timeout, started, exited)
	}
	close(stopSampling)
	probeIn.Close()
	<-probeRead
	agentIn.Close()

	var r result
	r.peak = <-peak
	jobs.mu.Lock()
	r.makespan = jobs.last.Sub(jobs.first)
	r.capped, r.killed = jobs.capped, jobs.killed
	jobs.mu.Unlock()
	r.swapKnown, r.peakSwap, r.swapGrowth = swap.result()

	var late []time.Duration
	for _, f := range strings.Fields(string(probeData)) {
		us, err := strconv.ParseInt(f, 10, 64)
		if err == nil {
			late = append(late, time.Duration(us)*time.Microsecond)
		}
	}
	if len(late) == 0 {
		t.Fatal("probe recorded nothing")
	}
	slices.Sort(late)
	pct := func(p float64) time.Duration { return late[min(len(late)-1, int(p*float64(len(late))))] }
	r.p50, r.p95, r.p99 = pct(0.50), pct(0.95), pct(0.99)

	var events []domain.Event
	if err := transport.Call(context.Background(), socket, "events", app.EventsParams{Limit: app.MaxEventsPerCall}, &events); err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range events {
		switch e.Kind {
		case domain.EventSchedulerDecision, domain.EventSchedulerLimit, domain.EventPressureChanged,
			domain.EventControlFailed, domain.EventControlRefused, domain.EventDegraded, domain.EventRecovered:
			// The timeline is what explains a result, so the CI log keeps it.
			msg := e.Message
			if e.Kind == domain.EventPressureChanged {
				msg += " (" + e.Attrs["reason"] + ")"
			}
			t.Logf("%8s %s", secs(e.Time.Sub(begin)), msg)
		}
		switch e.Kind {
		case domain.EventSchedulerDecision:
			if e.Attrs["observed"] != "true" {
				r.interventions++
			}
		case domain.EventPressureChanged:
			var l domain.PressureLevel
			if err := l.UnmarshalText([]byte(e.Attrs["to"])); err == nil {
				r.maxPressure = max(r.maxPressure, l)
			}
		}
	}
	if st, err := status(socket); err == nil {
		r.maxPressure = max(r.maxPressure, st.Pressure)
	}
	t.Logf("%d probe samples, %d jobs (%d capped, %d killed)", len(late), len(sc.arrivals), r.capped, r.killed)
	return r
}
