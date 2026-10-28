// Package e2e runs the shipped throtd and throt binaries against real
// processes. It only runs with THROT_E2E=1 on Linux in a disposable
// environment (CI, or a container), because an enforce-mode daemon pauses
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
const jobSource = `package main

import (
	"os"
	"time"
)

var sink uint64

func main() {
	if len(os.Args) == 2 && os.Args[1] == "burn" {
		x := sink
		for {
			x = x*6364136223846793005 + 1442695040888963407
			sink = x
		}
	}
	for {
		time.Sleep(time.Hour)
	}
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
)

func main() {
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
	// comm. agentLabel is its first instance, from the "e2e" adapter.
	agentExe   = "throt-e2e-agent"
	agentLabel = "e2e-1"
	// baseConfig is shared by every test daemon: a short sample interval
	// keeps the tests quick, and the custom adapter recognizes the stub.
	baseConfig = `"sample_interval":"250ms","agents":[{"name":"e2e","executables":["` + agentExe + `"]}]`
)

// binDir holds throt, throtd and the helpers, built once by TestMain.
var binDir string

func enabled() bool { return skipReason() == "" }

// skipReason says why the suite must not run here, or "" when it may. The
// test daemons control real processes, so outside a disposable environment
// they could stop the builds of the developer's own agents.
func skipReason() string {
	switch {
	case os.Getenv("THROT_E2E") != "1":
		return "set THROT_E2E=1 to run the end-to-end tests (CI or a container only)"
	case runtime.GOOS != "linux":
		return "the end-to-end tests only run on Linux"
	case os.Getenv("CI") != "true" && !inContainer():
		return "THROT_E2E is set, but this is neither CI (CI=true) nor a container. The test daemons " +
			"pause real processes, so run the suite only in a disposable environment, e.g. " +
			"docker run --rm -v \"$PWD\":/src -w /src golang:1.26.10-alpine sh -c " +
			"'go test -c -o /tmp/e2e.test ./internal/e2e && THROT_E2E=1 /tmp/e2e.test -test.v' " +
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
	run := func(srcDir string, args ...string) error {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = srcDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	for _, name := range []string{"throt", "throtd"} {
		if err := run(".", "build", "-o", filepath.Join(binDir, name), "github.com/cfcromn/Throt/cmd/"+name); err != nil {
			return err
		}
	}
	for name, src := range map[string]string{"make": jobSource, agentExe: agentSource} {
		srcDir := filepath.Join(dir, "src", name)
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return err
		}
		for file, body := range map[string]string{"main.go": src, "go.mod": "module " + name + "\n\ngo 1.26\n"} {
			if err := os.WriteFile(filepath.Join(srcDir, file), []byte(body), 0o644); err != nil {
				return err
			}
		}
		if err := run(srcDir, "build", "-o", filepath.Join(binDir, name), "."); err != nil {
			return err
		}
	}
	return nil
}

func requireE2E(t *testing.T) {
	t.Helper()
	if r := skipReason(); r != "" {
		t.Skip(r)
	}
}

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

	daemon  *exec.Cmd
	exited  chan struct{} // closed when daemon has been waited for
	waitErr error
}

// newEnv creates private HOME, XDG_RUNTIME_DIR and XDG_CONFIG_HOME
// directories, writes config (if any), and starts throtd.
func newEnv(t *testing.T, config string) *env {
	t.Helper()
	requireE2E(t)
	home := filepath.Join(t.TempDir(), "home")
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
	e.startDaemon()
	return e
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

// procState returns the state letter from /proc/PID/stat, or 0 if the
// process is gone.
func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0
	}
	return b[i+2]
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
	cmd := exec.Command(filepath.Join(binDir, agentExe))
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
