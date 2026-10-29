//go:build darwin

package platform

import (
	"errors"
	"math"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

const iostatHeader = `      cpu    load average\n us sy id   1m   5m   15m\n`

// fakeIostat returns an iostatCPU that runs the shell script instead of
// iostat, and closes it when the test ends.
func fakeIostat(t *testing.T, script string) *iostatCPU {
	t.Helper()
	c := &iostatCPU{path: "/bin/sh", args: []string{"-c", script}, maxAge: time.Hour, retry: time.Hour}
	t.Cleanup(c.close)
	return c
}

// waitFor polls cond under c.mu until it holds or 5 s pass.
func waitFor(t *testing.T, c *iostatCPU, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gone reports whether pid no longer exists (the child was reaped).
func gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestIostatCPUAveragesSinceLastCall(t *testing.T) {
	// The since-boot line (99% busy) must be skipped.
	c := fakeIostat(t, `printf '`+iostatHeader+` 98  1  1  9.00 9.00 9.00\n 40 10 50  1.00 1.00 1.00\n 20 10 70  1.00 1.00 1.00\n'; exec sleep 60`)
	if _, ok, _ := c.busy(time.Now()); ok {
		t.Fatal("busy ok before any reading")
	}
	waitFor(t, c, "two readings", func() bool { return c.n == 2 })
	if v, ok, err := c.busy(time.Now()); !ok || math.Abs(v-0.4) > 1e-9 {
		t.Fatalf("busy = %v, %v, %v; want the mean 0.4", v, ok, err)
	}
	if v, ok, _ := c.busy(time.Now()); !ok || math.Abs(v-0.3) > 1e-9 {
		t.Fatalf("busy with no new reading = %v, %v; want the latest 0.3", v, ok)
	}
	if _, ok, err := c.busy(time.Now().Add(2 * time.Hour)); ok || !errors.Is(err, errIostatNoReading) {
		t.Fatalf("stale busy = %v, %v; want missing", ok, err)
	}
	pid := c.pid
	c.close()
	if !gone(pid) {
		t.Errorf("child %d still exists after close", pid)
	}
	started := c.started
	if _, ok, _ := c.busy(time.Now().Add(2 * time.Hour)); ok || c.running || c.started != started {
		t.Error("busy after close started iostat again")
	}
}

func TestIostatCPUGarbageStopsAndRetries(t *testing.T) {
	c := fakeIostat(t, `printf '`+iostatHeader+` 1 1 98 1 1 1\n 50 50 50 1 1 1\n'; exec sleep 60`)
	now := time.Now()
	c.busy(now)
	pid := c.pid
	waitFor(t, c, "the reader to stop", func() bool { return !c.running })
	if !gone(pid) {
		t.Errorf("child %d still exists after garbage", pid)
	}
	if _, ok, err := c.busy(now.Add(time.Minute)); ok || err == nil || !strings.Contains(err.Error(), "us+sy+id") {
		t.Fatalf("busy = %v, %v; want missing with the parse error", ok, err)
	}
	if c.running || c.pid != pid {
		t.Fatal("restarted before the retry period")
	}
	if c.busy(now.Add(2 * time.Hour)); c.pid == pid {
		t.Fatal("not restarted after the retry period")
	}
}

func TestIostatCPUExitAndOverlongLine(t *testing.T) {
	for name, script := range map[string]string{
		"exit":     `printf '` + iostatHeader + `'`,
		"overlong": `printf '%0300d\n' 0; exec sleep 60`,
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeIostat(t, script)
			c.busy(time.Now())
			pid := c.pid
			waitFor(t, c, "the reader to stop", func() bool { return !c.running })
			if !gone(pid) {
				t.Errorf("child %d still exists", pid)
			}
			if _, ok, err := c.busy(time.Now()); ok || err == nil {
				t.Errorf("busy = %v, %v; want missing with an error", ok, err)
			}
		})
	}
}

func TestIostatCPUMissingBinary(t *testing.T) {
	c := &iostatCPU{path: "/nonexistent/iostat", maxAge: time.Hour, retry: time.Hour}
	now := time.Now()
	if _, ok, err := c.busy(now); ok || err == nil || !strings.Contains(err.Error(), "start iostat") {
		t.Fatalf("busy = %v, %v; want a start error", ok, err)
	}
	c.busy(now.Add(time.Second))
	if !c.started.Equal(now) {
		t.Error("retried before the retry period")
	}
	c.close()
}

func TestIostatCPUCloseKillsStubbornChild(t *testing.T) {
	// Ignores SIGTERM: WaitDelay must kill it.
	c := fakeIostat(t, `trap '' TERM; printf '`+iostatHeader+` 1 1 98 1 1 1\n 1 1 98 1 1 1\n'; while :; do sleep 0.1; done`)
	c.busy(time.Now())
	waitFor(t, c, "the trap to be set", func() bool { return c.n == 1 })
	pid := c.pid
	start := time.Now()
	c.close()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("close took %v", d)
	}
	if !gone(pid) {
		t.Errorf("child %d still exists after close", pid)
	}
}

func TestIostatCPUNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 5 {
		c := &iostatCPU{path: "/bin/sh", args: []string{"-c", `printf '` + iostatHeader + `'; exec sleep 60`}, maxAge: time.Hour, retry: time.Hour}
		c.busy(time.Now())
		c.close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIostatCPUReal(t *testing.T) {
	if _, err := os.Stat(iostatPath); err != nil {
		t.Skip("no iostat")
	}
	c := newIostatCPU()
	defer c.close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok, err := c.busy(time.Now())
		if ok {
			if v < 0 || v > 1 {
				t.Fatalf("busy = %v, want 0..1", v)
			}
			t.Logf("host CPU busy %.0f%%", v*100)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reading from iostat: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	pid := c.pid
	c.close()
	if !gone(pid) {
		t.Errorf("iostat %d still exists after close", pid)
	}
}

func TestSampleHostDarwinUsesIostat(t *testing.T) {
	s := &darwinSampler{cpu: fakeIostat(t, `printf '`+iostatHeader+` 1 1 98 1 1 1\n 60 10 30 1 1 1\n'; exec sleep 60`)}
	s.cpu.busy(time.Now())
	waitFor(t, s.cpu, "a reading", func() bool { return s.cpu.n == 1 })
	h, err := s.SampleHost(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Has(domain.MetricCPU) || math.Abs(h.CPUUsage-0.7) > 1e-9 {
		t.Errorf("CPUUsage = %v (missing %v), want 0.7", h.CPUUsage, h.Missing)
	}
	if err := CloseHostSampler(s); err != nil {
		t.Fatal(err)
	}
}
