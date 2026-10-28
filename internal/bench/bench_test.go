// Package bench holds Throt's real-kernel benchmark: real processes, the real
// kernel scheduler and the real daemon. It only runs with THROT_BENCH=1 in a
// disposable environment: a Linux container (see `make bench`), or a hosted
// CI runner (CI=true) on Linux or macOS (.github/workflows/bench.yml),
// because an enforce-mode daemon pauses any build attributed to an agent
// named claude.
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
const jobSource = `package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

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
	n, err := strconv.ParseUint(os.Args[1], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	spin(n)
}
`

// agentSource is the agent stub, named claude so Throt attributes its
// children to it. Each stdin line is a job's iteration count; it starts
// ./make with it and reports "start PID UNIXNANO" and "exit PID UNIXNANO".
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
	report := func(what string, pid int) {
		mu.Lock()
		fmt.Printf("%s %d %d\n", what, pid, time.Now().UnixNano())
		mu.Unlock()
	}
	var wg sync.WaitGroup
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		cmd := exec.Command(job, strings.TrimSpace(in.Text()))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		report("start", cmd.Process.Pid)
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd.Wait()
			report("exit", cmd.Process.Pid)
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
	defaultReps    = 3
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
	heavyLimit int // Throt's scheduler.heavy_limit
	reps       int
}

// sizeFor picks the contention. In a container (make bench, --cpus=2) it
// keeps the fixed scenarios of the published results: NumCPU there is the
// host's, not the quota. On a CI runner, which has no CPU limit, it scales
// to the machine: 4x NumCPU jobs at once, and a stream of as many whose
// arrival rate asks for about twice the CPUs, so both oversubscribe.
// THROT_BENCH_JOBS and THROT_BENCH_REPS override the job count of both
// scenarios and the repetitions, for a quick run.
func sizeFor(container bool) (sizing, error) {
	z := sizing{cpus: 2, burst: 8, stagger: 10, every: time.Second, heavyLimit: 2, reps: defaultReps}
	if !container {
		n := runtime.NumCPU()
		z = sizing{cpus: n, burst: 4 * n, stagger: 4 * n, heavyLimit: max(2, n/2), reps: defaultReps}
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
}

func TestBench(t *testing.T) {
	if os.Getenv("THROT_BENCH") != "1" {
		t.Skip("set THROT_BENCH=1 and run inside a container (make bench) or on a CI runner (bench.yml)")
	}
	// An enforce-mode daemon pauses builds of any process named claude, so
	// only a disposable environment qualifies: a Linux container, whose PID
	// namespace is isolated, or a hosted CI runner, which is a throwaway VM.
	container := runtime.GOOS == "linux" && inContainer()
	ci := os.Getenv("CI") == "true" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin")
	if !container && !ci {
		t.Skip("the benchmark only runs in a Linux container (make bench) or on a Linux or macOS CI runner with CI=true (gh workflow run bench.yml)")
	}
	z, err := sizeFor(container)
	if err != nil {
		t.Fatal(err)
	}
	bin := buildHelpers(t)
	iters := calibrate(t, bin)
	t.Logf("calibration: %d iterations per CPU second; GOMAXPROCS %d, NumCPU %d", iters, runtime.GOMAXPROCS(0), runtime.NumCPU())
	work := uint64(float64(iters) * jobCPU.Seconds())

	var table strings.Builder
	fmt.Fprintf(&table, "\n%s/%s, %s, %d CPUs, heavy_limit %d, %s of CPU per job, medians of %d runs\n\n",
		runtime.GOOS, runtime.GOARCH, kernel(), z.cpus, z.heavyLimit, jobCPU, z.reps)
	table.WriteString("| Scenario | Mode | Probe p50 | p95 | p99 | Makespan | Makespan range | Peak running | Interventions | Max pressure |\n")
	table.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, sc := range scenarios(z) {
		for _, mode := range []string{"off", "enforce"} {
			t.Run(sc.name+"/"+mode, func(t *testing.T) {
				var runs []result
				for i := range z.reps {
					t.Run("run"+strconv.Itoa(i+1), func(t *testing.T) {
						runs = append(runs, runScenario(t, bin, sc, mode, work, z.heavyLimit))
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
	if out := os.Getenv("THROT_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(table.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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
// private socket, and stops it at cleanup.
func startDaemon(t *testing.T, mode string, heavyLimit int) string {
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
	cfg.SampleInterval = config.Duration(sampleInterval)
	cfg.Scheduler.Mode = mode
	cfg.Scheduler.HeavyLimit = heavyLimit
	cfg.Scheduler.ResumeCooldown = config.Duration(time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	procs, ctl := platform.NewProcessTools()
	d := app.NewDaemon(cfg, platform.NewHostSampler(), procs, ctl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("daemon: %v", err)
		}
	})
	return cfg.SocketPath
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
	first, last time.Time
	done        chan struct{} // closed when every expected job has exited
}

func (j *jobTracker) read(r io.Reader, want int) {
	in := bufio.NewScanner(r)
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) != 3 {
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

func runScenario(t *testing.T, bin string, sc scenario, mode string, work uint64, heavyLimit int) result {
	socket := startDaemon(t, mode, heavyLimit)
	// The scheduler never queues while pressure is unknown, so wait out the
	// warm-up before any job starts.
	waitFor(t, "known pressure", func() bool {
		st, err := status(socket)
		return err == nil && st.Pressure != domain.PressureUnknown
	})
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

	begin := time.Now()
	line := strconv.FormatUint(work, 10) + "\n"
	for _, at := range sc.arrivals {
		time.Sleep(time.Until(begin.Add(at)))
		if _, err := io.WriteString(agentIn, line); err != nil {
			t.Fatalf("launch job: %v", err)
		}
	}
	select {
	case <-jobs.done:
	case <-time.After(runTimeout):
		t.Fatalf("jobs did not finish within %s", runTimeout)
	}
	close(stopSampling)
	probeIn.Close()
	<-probeRead
	agentIn.Close()

	var r result
	r.peak = <-peak
	jobs.mu.Lock()
	r.makespan = jobs.last.Sub(jobs.first)
	jobs.mu.Unlock()

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
	t.Logf("%d probe samples, %d jobs", len(late), len(sc.arrivals))
	return r
}
