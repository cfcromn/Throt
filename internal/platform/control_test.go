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

func TestProcessControllerLifecycle(t *testing.T) {
	skipUnlessControl(t)
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

	sampler, ctl := NewProcessTools()
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
	start, uid, err := ctl.Identity(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if (domain.ProcessKey{PID: pid, Start: start}) != sample.Key() || uid != sample.UID {
		t.Fatalf("Identity = %v uid %d, sample = %v uid %d", start, uid, sample.Start, sample.UID)
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", uid, os.Getuid())
	}

	if err := ctl.Stop(pid); err != nil {
		t.Fatal(err)
	}
	if s := waitState(t, pid, func(s string) bool { return s == "T" }); s != "T" {
		t.Fatalf("state after Stop = %q, want T", s)
	}
	if err := ctl.Continue(pid); err != nil {
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
	if _, _, err := ctl.Identity(ctx, pid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Identity after exit: err = %v, want ErrProcessGone", err)
	}
	if err := ctl.Stop(pid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Stop after exit: err = %v, want ErrProcessGone", err)
	}
	if err := ctl.Continue(pid); !errors.Is(err, domain.ErrProcessGone) {
		t.Errorf("Continue after exit: err = %v, want ErrProcessGone", err)
	}
}

func TestProcessControllerIdentitySelf(t *testing.T) {
	skipUnlessControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Independent constructors must still agree while the clock is steady.
	start, uid, err := NewProcessController().Identity(ctx, os.Getpid())
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
				t.Fatalf("Identity = %v uid %d, sample = %v uid %d", start, uid, p.Start, p.UID)
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
	_, _, err := NewProcessController().Identity(ctx, os.Getpid())
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestProcessControllerRefuses(t *testing.T) {
	skipUnlessControl(t)
	ctl := NewProcessController()
	for _, pid := range []int{0, 1, -1, -os.Getpid(), os.Getpid()} {
		if err := ctl.Stop(pid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("Stop(%d) err = %v, want refusal", pid, err)
		}
		if err := ctl.Continue(pid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("Continue(%d) err = %v, want refusal", pid, err)
		}
	}
	for _, pid := range []int{0, -1} {
		if _, _, err := ctl.Identity(context.Background(), pid); err == nil || errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("Identity(%d) err = %v, want refusal", pid, err)
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
