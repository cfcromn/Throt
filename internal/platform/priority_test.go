//go:build darwin || linux

package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// prioritizer is implemented by the darwin and linux controllers.
type prioritizer interface {
	Background(ctx context.Context, key domain.ProcessKey, uid uint32) (uint64, error)
	Foreground(ctx context.Context, key domain.ProcessKey, uid uint32, token uint64) error
}

// startSleep starts a child of the test and returns its identity, as the
// controller's own identity reports it.
func startSleep(t *testing.T) (*exec.Cmd, domain.ProcessKey, prioritizer) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	_, ctl := NewProcessTools()
	start, _, err := identityOf(t, ctl).identity(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, domain.ProcessKey{PID: cmd.Process.Pid, Start: start}, ctl.(prioritizer)
}

func TestPriorityLowerAndRestore(t *testing.T) {
	_, key, p := startSleep(t)
	uid := uint32(os.Getuid())
	ctx := context.Background()
	if inBackground(t, key.PID) {
		t.Skip("the test itself runs at background priority")
	}
	token, err := p.Background(ctx, key, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !inBackground(t, key.PID) {
		t.Fatal("Background left the child at normal priority")
	}
	// Lowering again is harmless.
	if _, err := p.Background(ctx, key, uid); err != nil {
		t.Fatal(err)
	}
	if err := p.Foreground(ctx, key, uid, token); err != nil {
		t.Fatal(err)
	}
	if !restored(t, key.PID) {
		t.Fatal("Foreground left the child in the background")
	}
}

func TestPriorityRefusesMismatch(t *testing.T) {
	_, key, p := startSleep(t)
	uid := uint32(os.Getuid())
	ctx := context.Background()
	reused := key
	reused.Start = key.Start.Add(-time.Hour)
	for name, tc := range map[string]struct {
		key domain.ProcessKey
		uid uint32
	}{
		"pid reused": {reused, uid},
		"other uid":  {key, uid + 1},
	} {
		if _, err := p.Background(ctx, tc.key, tc.uid); !errors.Is(err, domain.ErrProcessGone) {
			t.Errorf("%s: Background = %v, want ErrProcessGone", name, err)
		}
		if inBackground(t, key.PID) {
			t.Fatalf("%s: Background acted on a mismatched process", name)
		}
	}
	for _, pid := range []int{0, 1, -1, os.Getpid()} {
		if _, err := p.Background(ctx, domain.ProcessKey{PID: pid, Start: key.Start}, uid); err == nil {
			t.Errorf("Background(pid %d) succeeded", pid)
		}
	}
}

func TestPriorityGone(t *testing.T) {
	cmd, key, p := startSleep(t)
	cmd.Process.Kill()
	cmd.Wait()
	if _, err := p.Background(context.Background(), key, uint32(os.Getuid())); !errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("Background of an exited process = %v, want ErrProcessGone", err)
	}
	if err := p.Foreground(context.Background(), key, uint32(os.Getuid()), validToken); !errors.Is(err, domain.ErrProcessGone) {
		t.Fatalf("Foreground of an exited process = %v, want ErrProcessGone", err)
	}
}
