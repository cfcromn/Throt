package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func skipUnlessControl(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS")
	}
}

// processState returns the one-letter scheduler state of pid, e.g. "T" for
// stopped.
func processState(t *testing.T, pid int) string {
	t.Helper()
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		return fields[0]
	}
	out, err := exec.Command("/bin/ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))[:1]
}

// waitState polls until pid's state satisfies ok, since signal delivery is
// asynchronous.
func waitState(t *testing.T, pid int, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := processState(t, pid)
		if ok(s) || time.Now().After(deadline) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// identifier is implemented by the darwin and linux controllers.
type identifier interface {
	identity(ctx context.Context, pid int) (time.Time, uint32, error)
}

func identityOf(t *testing.T, ctl ProcessController) identifier {
	t.Helper()
	id, ok := ctl.(identifier)
	if !ok {
		t.Fatalf("%T has no identity method", ctl)
	}
	return id
}

// stillState checks that pid stays in a state satisfying ok for a while,
// to show that no signal arrived.
func stillState(t *testing.T, pid int, ok func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s := processState(t, pid); !ok(s) {
			t.Fatalf("state changed to %q: a signal was delivered", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProcessControllerLifecycle(t *testing.T) {
	skipUnlessControl(t)
	sampler, ctl := NewProcessTools()
	testLifecycle(t, sampler, ctl)
}

func testLifecycle(t *testing.T, sampler ProcessSampler, ctl ProcessController) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	procs, err := sampler.SampleProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sample *domain.ProcessSample
	for i := range procs {
		if procs[i].PID == pid {
			sample = &procs[i]
		}
	}
	if sample == nil {
		t.Fatalf("pid %d not sampled", pid)
	}
	key, uid := sample.Key(), sample.UID
	start, got, err := identityOf(t, ctl).identity(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if (domain.ProcessKey{PID: pid, Start: start}) != key || got != uid {
		t.Fatalf("identity = %v uid %d, sample = %v uid %d", start, got, sample.Start, uid)
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", uid, os.Getuid())
	}
	if gone, err := ctl.Gone(ctx, key); err != nil || gone {
		t.Errorf("Gone(live) = %v, %v", gone, err)
	}

	// A live PID with another start time (a reused PID) or another owner
	// is reported gone and never signaled.
	reused := domain.ProcessKey{PID: pid, Start: key.Start.Add(-time.Hour)}
	if gone, err := ctl.Gone(ctx, reused); err != nil || !gone {
		t.Errorf("Gone(reused) = %v, %v", gone, err)
	}
	if err := ctl.Stop(ctx, reused, uid); !errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("Stop(reused) err = %v, want ErrProcessGone", err)
	}
	if err := ctl.Stop(ctx, key, uid+1); !errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("Stop(foreign uid) err = %v, want ErrProcessGone", err)
	}
	stillState(t, pid, func(s string) bool { return s != "T" })

	if err := ctl.Stop(ctx, key, uid); err != nil {
		t.Fatal(err)
	}
	if s := waitState(t, pid, func(s string) bool { return s == "T" }); s != "T" {
		t.Fatalf("state after Stop = %q, want T", s)
	}
	if err := ctl.Continue(ctx, reused, uid); !errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("Continue(reused) err = %v, want ErrProcessGone", err)
	}
	stillState(t, pid, func(s string) bool { return s == "T" })
	if err := ctl.Continue(ctx, key, uid); err != nil {
		t.Fatal(err)
	}
	if s := waitState(t, pid, func(s string) bool { return s != "T" }); s == "T" {
		t.Fatalf("state after Continue = %q, want not stopped", s)
	}

	// Exited race: once the child is gone, every operation reports it.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true
	if _, _, err := identityOf(t, ctl).identity(ctx, pid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("identity after exit: err = %v, want ErrProcessGone", err)
	}
	if gone, err := ctl.Gone(ctx, key); err != nil || !gone {
		t.Errorf("Gone after exit = %v, %v", gone, err)
	}
	if err := ctl.Stop(ctx, key, uid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Stop after exit: err = %v, want ErrProcessGone", err)
	}
	if err := ctl.Continue(ctx, key, uid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Continue after exit: err = %v, want ErrProcessGone", err)
	}
}

func TestProcessControllerIdentify(t *testing.T) {
	skipUnlessControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sampler, ctl := NewProcessTools()
	id, ok := ctl.(ProcessIdentifier)
	if !ok {
		t.Fatalf("%T does not implement ProcessIdentifier", ctl)
	}
	got, err := id.Identify(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	procs, err := sampler.SampleProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == os.Getpid() {
			if got.PID != p.PID || got.PPID != os.Getppid() || got.PPID != p.PPID || !got.Start.Equal(p.Start) || got.UID != p.UID {
				t.Fatalf("Identify = %+v, sample = %+v", got, p)
			}
			if _, err := id.Identify(ctx, 1<<30); !errors.Is(err, domain.ErrProcessGone) && err == nil {
				t.Errorf("Identify(missing) succeeded")
			}
			return
		}
	}
	t.Fatal("this process is not in the sample")
}

func TestProcessControllerIdentitySelf(t *testing.T) {
	skipUnlessControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Independent constructors must still agree while the clock is steady.
	start, uid, err := identityOf(t, NewProcessController()).identity(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	procs, err := NewProcessSampler().SampleProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == os.Getpid() {
			if !p.Start.Equal(start) || p.UID != uid {
				t.Fatalf("identity = %v uid %d, sample = %v uid %d", start, uid, p.Start, p.UID)
			}
			return
		}
	}
	t.Fatal("own pid not sampled")
}

func TestProcessControllerCancelled(t *testing.T) {
	skipUnlessControl(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctl := NewProcessController()
	_, _, err := identityOf(t, ctl).identity(ctx, os.Getpid())
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("identity err = %v, want context.Canceled", err)
	}
	// An undeterminable identity is an error, never "gone" and never a
	// signal.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	key := domain.ProcessKey{PID: cmd.Process.Pid, Start: time.Unix(1, 0)}
	if err := ctl.Stop(ctx, key, uint32(os.Getuid())); !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Stop err = %v, want context.Canceled", err)
	}
	if _, err := ctl.Gone(ctx, key); !errors.Is(err, context.Canceled) {
		t.Errorf("Gone err = %v, want context.Canceled", err)
	}
}

func TestProcessControllerRefuses(t *testing.T) {
	skipUnlessControl(t)
	ctl := NewProcessController()
	ctx := context.Background()
	uid := uint32(os.Getuid())
	for _, pid := range []int{0, 1, -1, -os.Getpid(), os.Getpid()} {
		key := domain.ProcessKey{PID: pid, Start: time.Unix(1, 0)}
		if err := ctl.Stop(ctx, key, uid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("Stop(%d) err = %v, want refusal", pid, err)
		}
		if err := ctl.Continue(ctx, key, uid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("Continue(%d) err = %v, want refusal", pid, err)
		}
	}
	for _, pid := range []int{0, -1} {
		if _, _, err := identityOf(t, ctl).identity(ctx, pid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("identity(%d) err = %v, want refusal", pid, err)
		}
		if _, err := ctl.Gone(ctx, domain.ProcessKey{PID: pid}); err == nil {
			t.Errorf("Gone(%d) succeeded, want refusal", pid)
		}
	}
}

func TestCheckSignalTarget(t *testing.T) {
	for _, tc := range []struct {
		pid int
		ok  bool
	}{
		{-2, false}, {-1, false}, {0, false}, {1, false}, {os.Getpid(), false},
		{2, true}, {os.Getpid() + 1, true},
	} {
		if err := checkSignalTarget(tc.pid); (err == nil) != tc.ok {
			t.Errorf("checkSignalTarget(%d) = %v, want ok=%v", tc.pid, err, tc.ok)
		}
	}
}
