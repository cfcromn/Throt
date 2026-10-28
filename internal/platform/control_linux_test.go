package platform

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPidfdAvailable reports whether TestProcessControllerLifecycle ran the
// pidfd path on this kernel.
func TestPidfdAvailable(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		t.Skipf("pidfd unavailable (%v): the lifecycle test used the kill(2) fallback", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
}

// TestProcessControllerLifecycleWithoutPidfd runs the lifecycle through the
// verify-then-kill fallback used on kernels without pidfd_open.
func TestProcessControllerLifecycleWithoutPidfd(t *testing.T) {
	boot := &bootClock{}
	ctl := linuxProcessController{boot: boot, pidfdOpen: func(int, int) (int, error) { return -1, unix.ENOSYS }}
	testLifecycle(t, &linuxProcessSampler{boot: boot}, ctl)
}
