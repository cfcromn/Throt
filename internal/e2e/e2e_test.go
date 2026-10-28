// Package e2e runs the shipped throtd and throt binaries against real
// processes. It only runs with THROT_E2E=1 in a disposable environment (CI
// or a container on Linux, CI only on macOS), because an enforce-mode daemon pauses
// the builds of every agent it recognizes, built-in ones such as claude
// included. The test daemon's agent is a custom adapter for a stub with a
// name no real agent has.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// jobSource is the heavy job, named make so Throt classifies it as a build.
// "make sleep" sleeps until killed; "make burn" burns one core until killed.
// An optional duration ("make burn 2s") makes it exit after that much work:
// CPU time for burn, 50ms sleep steps for sleep, so time spent stopped
// (queued or paused) does not count.
const jobSource = `package main

import (
	"os"
	"syscall"
	"time"
)

var sink uint64

func main() {
	var d time.Duration
	if len(os.Args) == 3 {
		d, _ = time.ParseDuration(os.Args[2])
	}
	if len(os.Args) >= 2 && os.Args[1] == "burn" {
		x := sink
		for i := 1; ; i++ {
			x = x*6364136223846793005 + 1442695040888963407
			sink = x
			if d > 0 && i%(1<<20) == 0 && cpuTime() >= d {
				return
			}
		}
	}
	if d <= 0 {
		for {
			time.Sleep(time.Hour)
		}
	}
	const step = 50 * time.Millisecond
	for range max(d/step, 1) {
		time.Sleep(step)
	}
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
`

// agentSource is the agent stub, named agentExe so the test config's
// custom adapter attributes its children to it. Each stdin line is an argument for ./make; it starts the
// job, reports "start PID", and reaps it when it exits.
const agentSource = `package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	// As a job of an interactive shell: its own process group, which
	// Ctrl-Z (SIGTSTP to the group) stops as a whole.
	if os.Getenv("THROT_E2E_SETPGID") == "1" {
		syscall.Setpgid(0, 0)
	}
	self, _ := os.Executable()
	job := filepath.Join(filepath.Dir(self), "make")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		cmd := exec.Command(job, strings.TrimSpace(in.Text()))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("start %d\n", cmd.Process.Pid)
		go cmd.Wait()
	}
}
`

const (
	// waitTimeout bounds every polled condition; CI machines can be slow.
	waitTimeout = 90 * time.Second
	// cliTimeout bounds one throt invocation.
	cliTimeout = 30 * time.Second
	// agentExe is the stub agent's executable; it fits the 15-byte Linux
	// comm and the 16-byte macOS one. agentLabel is its first instance, from the "e2e" adapter.
	agentExe   = "throt-e2e-agent"
	agentLabel = "e2e-1"
	// baseConfig is shared by every test daemon: a short sample interval
	// keeps the tests quick, and the custom adapter recognizes the stub.
	baseConfig = `"sample_interval":"250ms","agents":[{"name":"e2e","executables":["` + agentExe + `"]}]`
)

// binDir holds throt, throtd and the helpers, built once by TestMain.
var binDir string

// gate is an opt-in switch for tests that run real enforce-mode daemons.
type gate struct {
	env, what, run string // variable, test description, example test flags
	// linuxOnly gates tests that read /proc and container state directly.
	linuxOnly bool
}

var (
	e2eGate  = gate{"THROT_E2E", "the end-to-end tests", "-test.v", false}
	soakGate = gate{"THROT_SOAK", "the soak test", "-test.run=TestSoak -test.v -test.timeout=30m", true}
)

func enabled() bool { return e2eGate.skipReason() == "" || soakGate.skipReason() == "" }

// skipReason says why the tests must not run here, or "" when they may.
// The test daemons control real processes, so outside a disposable
// environment they could stop the builds of the developer's own agents.
// A Mac has no container to isolate them, so on macOS only CI qualifies
// (hosted macOS runners are disposable VMs).
func (g gate) skipReason() string {
	switch {
	case os.Getenv(g.env) != "1":
		return "set " + g.env + "=1 to run " + g.what + " (CI or a container only)"
	case runtime.GOOS == "darwin" && !g.linuxOnly:
		if os.Getenv("CI") != "true" {
			return g.env + " is set, but this is not CI (CI=true). The test daemons pause real " +
				"processes, so on macOS run " + g.what + " only on a disposable CI runner"
		}
		return ""
	case g.linuxOnly && runtime.GOOS != "linux":
		return g.what + " only runs on Linux"
	case runtime.GOOS != "linux":
		return g.what + " only run on Linux and macOS"
	case os.Getenv("CI") != "true" && !inContainer():
		return g.env + " is set, but this is neither CI (CI=true) nor a container. The test daemons " +
			"pause real processes, so run " + g.what + " only in a disposable environment, e.g. " +
			"docker run --rm --init -v \"$PWD\":/src -w /src golang:1.26.10-alpine sh -c " +
			"'go test -c -o /tmp/e2e.test ./internal/e2e && " + g.env + "=1 /tmp/e2e.test " + g.run + "' " +
			"(run the binary directly: under go test, the go process would own every job)"
	}
	return ""
}

// inContainer reports a Docker or Podman marker file, or a cgroup v2 root
// path, which a process sees in its own cgroup namespace.
func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	return err == nil && slices.Contains(strings.Split(strings.TrimSpace(string(b)), "\n"), "0::/")
}

func TestMain(m *testing.M) {
	if !enabled() {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "throte2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binDir = filepath.Join(dir, "bin")
	err = build(dir)
	code := 1
	if err == nil {
		code = m.Run()
	} else {
		fmt.Fprintln(os.Stderr, "e2e build:", err)
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// build compiles throt and throtd from this module, and the helpers from
// their own throwaway modules.
func build(dir string) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("go toolchain not found: %w", err)
	}
	for _, name := range []string{"throt", "throtd"} {
		if err := goBuild(goBin, ".", "build", "-o", filepath.Join(binDir, name), "github.com/cfcromn/Throt/cmd/"+name); err != nil {
			return err
		}
	}
	for name, src := range map[string]string{"make": jobSource, agentExe: agentSource} {
		if err := buildHelper(goBin, dir, name, src); err != nil {
			return err
		}
	}
	return nil
}

func goBuild(goBin, srcDir string, args ...string) error {
	cmd := exec.Command(goBin, args...)
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// buildHelper compiles src, a main package, in its own throwaway module
// under dir and installs it as binDir/name.
func buildHelper(goBin, dir, name, src string) error {
	srcDir := filepath.Join(dir, "src", name)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	for file, body := range map[string]string{"main.go": src, "go.mod": "module " + name + "\n\ngo 1.26\n"} {
		if err := os.WriteFile(filepath.Join(srcDir, file), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return goBuild(goBin, srcDir, "build", "-o", filepath.Join(binDir, name), ".")
}

func requireGate(t *testing.T, g gate) {
	t.Helper()
	if r := g.skipReason(); r != "" {
		t.Skip(r)
	}
}

func requireE2E(t *testing.T) { t.Helper(); requireGate(t, e2eGate) }

// lockedBuffer collects daemon output from its stderr goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is one test's private daemon environment.
type env struct {
	t    *testing.T
	home string
	vars []string
	log  *lockedBuffer
	// out, when set, receives the daemon's output instead of log.
	out io.Writer

	daemon  *exec.Cmd
	exited  chan struct{} // closed when daemon has been waited for
	waitErr error
}

// newEnv creates private HOME, XDG_RUNTIME_DIR and XDG_CONFIG_HOME
// directories, writes config (if any), and starts throtd.
func newEnv(t *testing.T, config string) *env {
	t.Helper()
	requireE2E(t)
	e := setupEnv(t, config)
	e.startDaemon()
	return e
}

// setupEnv is newEnv without starting throtd.
func setupEnv(t *testing.T, config string) *env {
	t.Helper()
	home := filepath.Join(shortTempDir(t), "home")
	e := &env{t: t, home: home, log: &lockedBuffer{}}
	runDir, cfgDir := filepath.Join(home, "run"), filepath.Join(home, "config")
	for _, d := range []string{runDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	override := map[string]string{"HOME": home, "XDG_RUNTIME_DIR": runDir, "XDG_CONFIG_HOME": cfgDir}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); override[k] == "" {
			e.vars = append(e.vars, kv)
		}
	}
	for k, v := range override {
		e.vars = append(e.vars, k+"="+v)
	}
	if config != "" {
		e.writeConfig(config)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("throtd output:\n%s", e.log.String())
		}
	})
	return e
}

// shortTempDir is t.TempDir, except on macOS: its socket paths are limited
// to 104 bytes and $TMPDIR is long, so the daemon's socket under a test's
// t.TempDir would not fit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("/tmp", "throte2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func (e *env) configPath() string { return filepath.Join(e.home, "config", "throt", "config.json") }

func (e *env) writeConfig(config string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.configPath()), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath(), []byte(config), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// startDaemon runs throtd and waits until it answers ping. Cleanup sends
// SIGTERM and waits for it to exit.
func (e *env) startDaemon() {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "throtd"))
	cmd.Env = e.vars
	cmd.Stdout, cmd.Stderr = e.log, e.log
	if e.out != nil {
		cmd.Stdout, cmd.Stderr = e.out, e.out
	}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	exited := make(chan struct{})
	e.daemon, e.exited = cmd, exited
	go func() {
		e.waitErr = cmd.Wait()
		close(exited)
	}()
	e.t.Cleanup(func() { e.stopDaemon(syscall.SIGTERM) })
	e.waitFor("throtd to answer ping", func() bool {
		_, err := e.throt("ping")
		return err == nil
	})
}

// stopDaemon signals throtd and waits for it to exit, killing it if it
// does not. It returns the exit error; calling it again is a no-op.
func (e *env) stopDaemon(sig syscall.Signal) error {
	e.t.Helper()
	select {
	case <-e.exited:
		return e.waitErr
	default:
	}
	e.daemon.Process.Signal(sig)
	select {
	case <-e.exited:
	case <-time.After(waitTimeout):
		e.daemon.Process.Kill()
		<-e.exited
		e.t.Errorf("throtd did not exit within %s of %v", waitTimeout, sig)
	}
	return e.waitErr
}

// throt runs the client with the private environment and returns stdout,
// or an error carrying stderr.
func (e *env) throt(args ...string) (string, error) {
	return e.throtStdin(nil, args...)
}

func (e *env) throtStdin(stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "throt"), args...)
	cmd.Env = e.vars
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("throt %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (e *env) mustThrot(args ...string) string {
	e.t.Helper()
	out, err := e.throt(args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// waitFor polls cond until it holds or waitTimeout passes.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// heavyRow is one row of the HEAVY WORKLOADS table in throt status.
type heavyRow struct {
	owner, name, class, state string
	pid                       int
}

// statusView is the parsed text of throt status.
type statusView struct {
	pressure string
	mode     string
	agents   map[string]int // label -> PID
	heavy    []heavyRow
}

func (s statusView) workload(pid int) (heavyRow, bool) {
	for _, h := range s.heavy {
		if h.pid == pid {
			return h, true
		}
	}
	return heavyRow{}, false
}

func parseStatus(out string) statusView {
	s := statusView{agents: map[string]int{}}
	section := ""
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "PRESSURE":
			if len(f) > 1 {
				s.pressure = f[1]
			}
			continue
		case "SCHEDULER":
			if len(f) > 1 {
				s.mode = f[1]
			}
			continue
		case "AGENT", "OWNER":
			section = f[0]
			continue
		}
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		switch section {
		case "AGENT":
			s.agents[f[0]] = pid
		case "OWNER":
			s.heavy = append(s.heavy, heavyRow{owner: f[0], pid: pid, name: f[2], class: f[3], state: f[len(f)-1]})
		}
	}
	return s
}

func (e *env) status() (statusView, bool) {
	out, err := e.throt("status")
	if err != nil {
		return statusView{}, false
	}
	return parseStatus(out), true
}

// waitStartable waits until pressure is known and lets the scheduler start
// new work: unknown pressure never queues, and high or critical blocks starts.
func (e *env) waitStartable() {
	e.t.Helper()
	e.waitFor("normal or elevated pressure", func() bool {
		st, ok := e.status()
		return ok && (st.pressure == "NORMAL" || st.pressure == "ELEVATED")
	})
}

// running reports whether pid is alive and not stopped.
func running(pid int) bool { s := procState(pid); return s == 'S' || s == 'R' || s == 'D' }

func (e *env) waitStopped(pid int) {
	e.t.Helper()
	e.waitFor(fmt.Sprintf("pid %d to be stopped (T)", pid), func() bool { return procState(pid) == 'T' })
}

func (e *env) waitRunning(pid int) {
	e.t.Helper()
	e.waitFor(fmt.Sprintf("pid %d to be running (S/R)", pid), func() bool { return running(pid) })
}

// startGroup starts cmd in its own process group and kills the whole group
// at cleanup, stopped members included.
func startGroup(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	})
}

// agent is a running stub agent.
type agent struct {
	e      *env
	pid    int
	stdin  io.WriteCloser
	starts chan int
}

func (e *env) startAgent() *agent {
	e.t.Helper()
	return e.startAgentCmd(exec.Command(filepath.Join(binDir, agentExe)))
}

// startShellAgent runs the stub agent as a job of sh, in its own process
// group as an interactive shell would put it. The returned agent's pid is
// the shell's; the agent's own PID is its process group.
func (e *env) startShellAgent() *agent {
	e.t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `"$0"; :`, filepath.Join(binDir, agentExe))
	cmd.Env = append(os.Environ(), "THROT_E2E_SETPGID=1")
	return e.startAgentCmd(cmd)
}

func (e *env) startAgentCmd(cmd *exec.Cmd) *agent {
	e.t.Helper()
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	startGroup(e.t, cmd)
	a := &agent{e: e, pid: cmd.Process.Pid, stdin: stdin, starts: make(chan int, 16)}
	go func() {
		in := bufio.NewScanner(stdout)
		for in.Scan() {
			if f := strings.Fields(in.Text()); len(f) == 2 && f[0] == "start" {
				if pid, err := strconv.Atoi(f[1]); err == nil {
					a.starts <- pid
				}
			}
		}
	}()
	return a
}

// job starts "make mode" under the agent and returns its PID.
func (a *agent) job(mode string) int {
	a.e.t.Helper()
	if _, err := io.WriteString(a.stdin, mode+"\n"); err != nil {
		a.e.t.Fatal(err)
	}
	select {
	case pid := <-a.starts:
		return pid
	case <-time.After(waitTimeout):
		a.e.t.Fatal("agent stub did not start a job")
		return 0
	}
}

// waitOwned waits until throt status lists pid as a heavy workload owned by
// the agent label ("-" for unattributed), and returns its row.
func (e *env) waitOwned(pid int, label string) heavyRow {
	e.t.Helper()
	var row heavyRow
	e.waitFor(fmt.Sprintf("pid %d as a heavy workload of %s", pid, label), func() bool {
		st, ok := e.status()
		if !ok {
			return false
		}
		var found bool
		row, found = st.workload(pid)
		return found && row.owner == label
	})
	return row
}

func TestObserve(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	a := e.startAgent()
	job := a.job("sleep")

	row := e.waitOwned(job, agentLabel)
	if row.name != "make" || row.class != "build" || row.state != "running" {
		t.Errorf("heavy row = %+v, want make, build, running", row)
	}
	st, _ := e.status()
	if st.agents[agentLabel] != a.pid {
		t.Errorf("agents = %v, want %s at pid %d", st.agents, agentLabel, a.pid)
	}

	out := e.mustThrot("events", "-n", "1000")
	var agentSeen, jobSeen bool
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var ev struct {
			Kind  string            `json:"kind"`
			Attrs map[string]string `json:"attrs"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("events line %q: %v", line, err)
		}
		switch {
		case ev.Kind == "agent_discovered" && ev.Attrs["agent"] == agentLabel && ev.Attrs["pid"] == strconv.Itoa(a.pid):
			agentSeen = true
		case ev.Kind == "process_discovered" && ev.Attrs["name"] == "make" && ev.Attrs["pid"] == strconv.Itoa(job):
			jobSeen = true
		}
	}
	if !agentSeen || !jobSeen {
		t.Errorf("events: agent_discovered %v, process_discovered %v; want both\n%s", agentSeen, jobSeen, out)
	}

	doc := e.mustThrot("doctor")
	var report map[string]any
	if err := json.Unmarshal([]byte(doc), &report); err != nil {
		t.Fatalf("doctor is not JSON: %v\n%s", err, doc)
	}
	if strings.Contains(doc, e.home) {
		t.Errorf("doctor leaks the home path %s:\n%s", e.home, doc)
	}
	if u, err := user.Current(); err == nil && u.Username != "" && strings.Contains(doc, u.Username) {
		t.Errorf("doctor leaks the username %q:\n%s", u.Username, doc)
	}
}

func TestManualControl(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	// An unattributed build. Control outcomes are only asserted once a
	// sample shows the target, or the refusal reason would be "not a heavy
	// workload" instead of the one under test.
	stray := exec.Command(filepath.Join(binDir, "make"), "sleep")
	startGroup(t, stray)
	e.waitOwned(stray.Process.Pid, "-")
	a := e.startAgent()
	job := a.job("sleep")
	e.waitOwned(job, agentLabel)
	if !running(job) {
		t.Fatalf("job state %c before pause, want S or R", procState(job))
	}

	e.mustThrot("pause", strconv.Itoa(job))
	e.waitStopped(job)
	if out := e.mustThrot("explain", strconv.Itoa(job)); !strings.Contains(out, "PAUSED") {
		t.Errorf("explain does not say PAUSED:\n%s", out)
	}
	e.mustThrot("resume", strconv.Itoa(job))
	e.waitRunning(job)

	// PID 1 is rejected as invalid params, the stray build as unattributed.
	for pid, want := range map[int]string{1: "pid must be > 1", stray.Process.Pid: "refused: unattributed"} {
		if out, err := e.throt("pause", strconv.Itoa(pid)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("pause %d: got %q, %v; want an error containing %q", pid, out, err, want)
		}
	}
	if s := procState(stray.Process.Pid); s == 'T' {
		t.Errorf("unattributed pid %d is stopped", stray.Process.Pid)
	}
	if s := procState(1); s == 'T' {
		t.Error("pid 1 is stopped")
	}
}

func TestCrashRecovery(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	a := e.startAgent()
	job := a.job("sleep")
	e.waitOwned(job, agentLabel)
	e.mustThrot("pause", strconv.Itoa(job))
	e.waitStopped(job)

	e.stopDaemon(syscall.SIGKILL)
	// Nothing is left to resume it.
	if s := procState(job); s != 'T' {
		t.Fatalf("job state %c after throtd was killed, want T", s)
	}
	if _, err := os.Stat(filepath.Join(e.home, "run", "throt", "paused.json")); err != nil {
		t.Errorf("paused.json after crash: %v", err)
	}

	e.startDaemon()
	e.waitRunning(job)
}

// TestOrphanStopped stops an agent's job behind throtd's back, as a throtd
// killed before it wrote paused.json would leave it. throtd must report it
// without resuming it, and throt resume must resume it on request.
func TestOrphanStopped(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	a := e.startAgent()
	job := a.job("sleep")
	e.waitOwned(job, agentLabel)
	if err := syscall.Kill(job, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(job)

	type entry struct {
		PID   int    `json:"pid"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
		Hint  string `json:"hint"`
	}
	var listed entry
	e.waitFor(fmt.Sprintf("throt doctor to list pid %d", job), func() bool {
		out, err := e.throt("doctor")
		if err != nil {
			return false
		}
		var r struct {
			Orphans struct {
				Source  string  `json:"source"`
				Count   int     `json:"count"`
				Entries []entry `json:"entries"`
			} `json:"orphans"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("doctor is not JSON: %v\n%s", err, out)
		}
		for _, en := range r.Orphans.Entries {
			if en.PID == job && r.Orphans.Source == "throtd" && r.Orphans.Count >= 1 {
				listed = en
				return true
			}
		}
		return false
	})
	if listed.Name != "make" || listed.Owner != agentLabel || !strings.Contains(listed.Hint, "left paused by an earlier throtd") {
		t.Errorf("doctor entry = %+v, want make of %s left paused", listed, agentLabel)
	}
	if out := e.mustThrot("status"); !strings.Contains(out, "STOPPED ") || !strings.Contains(out, "not paused by throtd (see throt doctor)") {
		t.Errorf("status has no STOPPED line:\n%s", out)
	}
	e.waitFor("an orphan_stopped event", func() bool {
		out, err := e.throt("events", "-n", "1000")
		return err == nil && strings.Contains(out, `"orphan_stopped"`) && strings.Contains(out, strconv.Itoa(job))
	})
	// Reported, never resumed on its own.
	if s := procState(job); s != 'T' {
		t.Fatalf("job state %c after being reported, want T", s)
	}

	e.mustThrot("resume", strconv.Itoa(job))
	e.waitRunning(job)
	e.waitFor("the STOPPED line to clear", func() bool {
		out, err := e.throt("status")
		return err == nil && !strings.Contains(out, "STOPPED ")
	})
}

// TestCtrlZJob suspends an agent run from a shell the way Ctrl-Z does:
// SIGTSTP to its process group, which stops the agent and its job. Every
// member is job control, so doctor says so, no orphan_stopped event is
// logged, and resume without --force is refused.
func TestCtrlZJob(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	a := e.startShellAgent()
	job := a.job("sleep")
	e.waitOwned(job, agentLabel)
	st, _ := e.status()
	agentPID := st.agents[agentLabel]
	if agentPID == 0 || agentPID == a.pid {
		t.Fatalf("agent pid %d (shell %d), want the stub under the shell", agentPID, a.pid)
	}
	// The job, then the agent, is killed while its parent is alive to reap
	// it. Killed together, they would be reparented to PID 1, and a
	// container's PID 1 shell never reaps them: the agent's zombie would
	// then be e2e-1 in every later test's daemon.
	t.Cleanup(func() {
		syscall.Kill(-agentPID, syscall.SIGCONT)
		for _, pid := range []int{job, agentPID} {
			syscall.Kill(pid, syscall.SIGKILL)
			deadline := time.Now().Add(waitTimeout)
			for procState(pid) != 0 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
		}
		syscall.Kill(-agentPID, syscall.SIGKILL)
	})
	if err := syscall.Kill(-agentPID, syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	e.waitStopped(agentPID)
	e.waitStopped(job)

	e.waitFor("throt doctor to list the agent and its job as job control", func() bool {
		out, err := e.throt("doctor")
		if err != nil {
			return false
		}
		var r struct {
			Orphans struct {
				Entries []struct {
					PID  int    `json:"pid"`
					Hint string `json:"hint"`
				} `json:"entries"`
			} `json:"orphans"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("doctor is not JSON: %v\n%s", err, out)
		}
		seen := 0
		for _, en := range r.Orphans.Entries {
			if en.PID != agentPID && en.PID != job {
				continue
			}
			if !strings.Contains(en.Hint, "job control (Ctrl-Z)") {
				t.Fatalf("pid %d hint %q, want job control", en.PID, en.Hint)
			}
			seen++
		}
		return seen == 2
	})
	if _, err := e.throt("resume", strconv.Itoa(job)); err == nil || !strings.Contains(err.Error(), "Ctrl-Z") {
		t.Errorf("resume of a Ctrl-Z'd job: err = %v, want a Ctrl-Z refusal", err)
	}
	if s := procState(job); s != 'T' {
		t.Errorf("job state %c after the refused resume, want T", s)
	}
	if out := e.mustThrot("events", "-n", "1000"); strings.Contains(out, `"orphan_stopped"`) {
		t.Errorf("orphan_stopped event for a shell job:\n%s", out)
	}
}

func TestGracefulShutdownResumes(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	a := e.startAgent()
	job := a.job("sleep")
	e.waitOwned(job, agentLabel)
	e.mustThrot("pause", strconv.Itoa(job))
	e.waitStopped(job)

	if err := e.stopDaemon(syscall.SIGTERM); err != nil {
		t.Errorf("throtd exit: %v", err)
	}
	if s := procState(job); s == 'T' || s == 0 {
		t.Errorf("job state %q after SIGTERM, want running", s)
	}
}

func TestEnforceScheduler(t *testing.T) {
	// Adaptive is off so the limit cannot grow during the test.
	const policy = `"heavy_limit":%d,"resume_cooldown":"0s","adaptive":false`
	e := newEnv(t, `{`+baseConfig+`,"scheduler":{"mode":"off",`+fmt.Sprintf(policy, 1)+`}}`)
	e.waitStartable()
	// Unattributed heavy workloads (e.g. the go command running this test)
	// take slots too, so give the agent exactly one slot beyond them.
	st, _ := e.status()
	others := len(st.heavy)
	enforce := func(mode string) string {
		return `{` + baseConfig + `,"scheduler":{"mode":"` + mode + `",` + fmt.Sprintf(policy, 1+others) + `}}`
	}
	e.writeConfig(enforce("enforce"))
	e.mustThrot("reload")
	e.waitFor("scheduler mode enforce", func() bool {
		st, ok := e.status()
		return ok && st.mode == "enforce"
	})

	a := e.startAgent()
	first := a.job("sleep")
	if row := e.waitOwned(first, agentLabel); row.state != "running" {
		t.Fatalf("first job state %s, want running", row.state)
	}
	e.waitStartable()
	second := a.job("sleep")
	e.waitStopped(second)
	e.waitFor("second job QUEUED in status", func() bool {
		st, ok := e.status()
		row, found := st.workload(second)
		return ok && found && row.state == "QUEUED"
	})
	if !running(first) {
		t.Errorf("first job state %c, want running", procState(first))
	}

	syscall.Kill(first, syscall.SIGKILL)
	e.waitRunning(second)
	e.waitFor("second job running in status", func() bool {
		st, ok := e.status()
		row, found := st.workload(second)
		return ok && found && row.state == "running"
	})

	// Turning enforcement off resumes whatever is still queued.
	e.waitStartable()
	third := a.job("sleep")
	e.waitStopped(third)
	e.writeConfig(enforce("off"))
	if out := e.mustThrot("reload"); !strings.Contains(out, "scheduler.mode") {
		t.Errorf("reload output does not mention scheduler.mode:\n%s", out)
	}
	e.waitRunning(third)
}

// leaseEvents returns the lease events in the daemon's log, oldest first.
func (e *env) leaseEvents() []map[string]string {
	e.t.Helper()
	var out []map[string]string
	for line := range strings.SplitSeq(strings.TrimSpace(e.mustThrot("events", "-n", "1000")), "\n") {
		var ev struct {
			Kind  string            `json:"kind"`
			Attrs map[string]string `json:"attrs"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			e.t.Fatalf("events line %q: %v", line, err)
		}
		if strings.HasPrefix(ev.Kind, "lease_") {
			ev.Attrs["kind"] = ev.Kind
			out = append(out, ev.Attrs)
		}
	}
	return out
}

func TestRunLease(t *testing.T) {
	// Adaptive is off so the limit cannot grow during the test.
	const policy = `"heavy_limit":%d,"resume_cooldown":"0s","adaptive":false`
	e := newEnv(t, `{`+baseConfig+`,"scheduler":{"mode":"off",`+fmt.Sprintf(policy, 1)+`}}`)
	e.waitStartable()
	// Unattributed heavy workloads take slots too, so leave exactly one free.
	st, _ := e.status()
	e.writeConfig(`{` + baseConfig + `,"scheduler":{"mode":"enforce",` + fmt.Sprintf(policy, 1+len(st.heavy)) + `}}`)
	e.mustThrot("reload")
	e.waitFor("scheduler mode enforce", func() bool {
		st, ok := e.status()
		return ok && st.mode == "enforce"
	})
	e.waitStartable()

	// Two `throt run` commands start together and race for the one slot.
	type run struct {
		cmd    *exec.Cmd
		stderr *lockedBuffer
	}
	runs := make([]run, 2)
	for i := range runs {
		cmd := exec.Command(filepath.Join(binDir, "throt"), "run", "--timeout", "5m", "--", filepath.Join(binDir, "make"), "sleep")
		cmd.Env = e.vars
		runs[i] = run{cmd: cmd, stderr: &lockedBuffer{}}
		cmd.Stderr = runs[i].stderr
	}
	for _, r := range runs {
		startGroup(t, r.cmd)
	}
	// throt run execs in place, so a started command keeps its PID and its
	// comm becomes make.
	started := func() (pids []int) {
		for _, r := range runs {
			if comm(r.cmd.Process.Pid) == "make" {
				pids = append(pids, r.cmd.Process.Pid)
			}
		}
		return pids
	}
	e.waitFor("one throt run to start its command", func() bool { return len(started()) > 0 })
	first := started()[0]
	var winner, waiting run
	for _, r := range runs {
		if r.cmd.Process.Pid == first {
			winner = r
		} else {
			waiting = r
		}
	}
	// The winner's lease ends once its command is a running heavy workload,
	// which then holds the slot itself.
	if row := e.waitOwned(first, "-"); row.state != "running" {
		t.Fatalf("first command state %s, want running", row.state)
	}
	e.waitFor("lease_released (became_workload)", func() bool {
		for _, ev := range e.leaseEvents() {
			if ev["kind"] == "lease_released" && ev["pid"] == strconv.Itoa(first) && ev["reason"] == "became_workload" {
				return true
			}
		}
		return false
	})
	// The other keeps waiting: it was never granted a slot.
	e.waitFor("the other throt run to report waiting", func() bool {
		return strings.Contains(waiting.stderr.String(), "waiting for a heavy slot")
	})
	time.Sleep(2 * time.Second) // several sample intervals
	if got := started(); len(got) != 1 {
		t.Fatalf("started commands %v, want exactly one; stderr:\n%s", got, waiting.stderr.String())
	}
	for _, ev := range e.leaseEvents() {
		if ev["kind"] == "lease_granted" && ev["pid"] == strconv.Itoa(waiting.cmd.Process.Pid) {
			t.Fatalf("waiting command was granted a lease: %v", ev)
		}
	}

	// The winner finishes; the waiting command gets the slot and starts.
	syscall.Kill(-first, syscall.SIGKILL)
	winner.cmd.Wait() // reap it, or its zombie would keep the slot
	e.waitFor("the waiting command to start", func() bool { return comm(waiting.cmd.Process.Pid) == "make" })
	if out := waiting.stderr.String(); !strings.Contains(out, "capacity available after") {
		t.Errorf("waiting command stderr:\n%s", out)
	}
}

func TestMCP(t *testing.T) {
	e := newEnv(t, `{`+baseConfig+`}`)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"throt_capacity","arguments":{}}}`,
	}, "\n") + "\n"
	out, err := e.throtStdin(strings.NewReader(in), "mcp")
	if err != nil {
		t.Fatal(err)
	}
	type response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	var resps []response
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var r response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("mcp line %q: %v", line, err)
		}
		if r.JSONRPC != "2.0" || r.Error != nil {
			t.Errorf("mcp response %s: want a JSON-RPC 2.0 result", line)
		}
		resps = append(resps, r)
	}
	if len(resps) != 2 || resps[0].ID != 1 || resps[1].ID != 2 {
		t.Fatalf("mcp responses:\n%s\nwant ids 1 and 2", out)
	}
	var call struct {
		IsError           bool                       `json:"isError"`
		StructuredContent map[string]json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(resps[1].Result, &call); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := json.Unmarshal(call.StructuredContent["ok_to_start"], &ok); call.IsError || err != nil {
		t.Errorf("throt_capacity result %s: want a boolean ok_to_start", resps[1].Result)
	}
}
