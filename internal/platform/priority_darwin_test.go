//go:build darwin

package platform

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

const validToken = 0

// TestMain makes Background lower every process, so the tests of the
// background state also run where the test process is itself under a QoS
// clamp, as on hosted CI runners. TestBackgroundLeavesUtilityClamp puts the
// real check back.
func TestMain(m *testing.M) {
	isClamped = func(int) bool { return false }
	os.Exit(m.Run())
}

// psPri reads pid's scheduling priority with ps: 4 in the background
// state, about 31 normally. getpriority(PRIO_DARWIN_PROCESS) cannot read
// the state back (it returns 0 either way).
func psPri(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-o", "pri=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func inBackground(t *testing.T, pid int) bool { return psPri(t, pid) <= 4 }

func restored(t *testing.T, pid int) bool { return !inBackground(t, pid) }

// TestKinfoIdentityMatchesPS checks that the sysctl identity the priority
// path verifies with agrees with the ps identity signals use.
func TestKinfoIdentityMatchesPS(t *testing.T) {
	_, key, _ := startSleep(t)
	start, uid, err := kinfoIdentity(context.Background(), key.PID)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(key.Start) || uid != uint32(os.Getuid()) {
		t.Fatalf("kinfo identity = %v uid %d, ps = %v uid %d", start, uid, key.Start, os.Getuid())
	}
}

// TestBackgroundInherited records how macOS treats children: a process
// forked after its parent was lowered starts in the background, and
// restoring the parent does not restore it.
func TestBackgroundInherited(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "read x; /bin/sleep 30")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	_, ctl := NewProcessTools()
	p := ctl.(prioritizer)
	start, _, err := kinfoIdentity(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	parent := keyOf(cmd.Process.Pid, start)
	uid := uint32(os.Getuid())
	if _, err := p.Background(context.Background(), parent, uid); err != nil {
		t.Fatal(err)
	}
	stdin.Write([]byte("\n"))
	child := waitChild(t, cmd.Process.Pid)
	t.Cleanup(func() { killPID(child) })
	if !inBackground(t, child) {
		t.Fatal("a child forked after lowering is not in the background")
	}
	if err := p.Foreground(context.Background(), parent, uid, 0); err != nil {
		t.Fatal(err)
	}
	if !inBackground(t, child) {
		t.Fatal("restoring the parent restored the child")
	}
}

func waitChild(t *testing.T, ppid int) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		out, _ := exec.Command("/usr/bin/pgrep", "-P", strconv.Itoa(ppid)).Output()
		if f := strings.Fields(string(out)); len(f) == 1 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				return pid
			}
		}
	}
	t.Fatal("no child appeared")
	return 0
}

func keyOf(pid int, start time.Time) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: start}
}

// killPID kills a grandchild the test started through its own shell.
func killPID(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

// TestBackgroundLeavesUtilityClamp checks that a process started under the
// utility QoS clamp is recognized, a child of it too, and that Background
// leaves it out of the background state.
func TestBackgroundLeavesUtilityClamp(t *testing.T) {
	if utilityClamped(os.Getpid()) || inBackground(t, os.Getpid()) {
		t.Skip("the test itself runs at lowered priority")
	}
	isClamped = utilityClamped
	t.Cleanup(func() { isClamped = func(int) bool { return false } })
	cmd := exec.Command("/usr/sbin/taskpolicy", "-c", "utility", "/bin/sh", "-c", "/bin/sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Skip("taskpolicy unavailable:", err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	child := waitChild(t, cmd.Process.Pid)
	t.Cleanup(func() { killPID(child) })
	if !utilityClamped(cmd.Process.Pid) || !utilityClamped(child) {
		t.Fatalf("utilityClamped = %v (child %v), want both; ps pri %d",
			utilityClamped(cmd.Process.Pid), utilityClamped(child), psPri(t, cmd.Process.Pid))
	}
	_, key, p := startSleep(t)
	if utilityClamped(key.PID) {
		t.Fatal("an ordinary process reads as clamped")
	}
	start, _, err := kinfoIdentity(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Background(context.Background(), keyOf(child, start), uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if inBackground(t, child) {
		t.Fatal("Background put a utility-clamped process in the background state")
	}
}
