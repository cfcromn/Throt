//go:build darwin || linux

package platform

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/cfcromn/Throt/internal/domain"
)

// unixSignaler implements ProcessController's Stop and Continue.
type unixSignaler struct{}

func (unixSignaler) Stop(pid int) error     { return signalProcess(pid, syscall.SIGSTOP) }
func (unixSignaler) Continue(pid int) error { return signalProcess(pid, syscall.SIGCONT) }

// signalProcess sends sig to the single process pid.
func signalProcess(pid int, sig syscall.Signal) error {
	if err := checkSignalTarget(pid); err != nil {
		return err
	}
	err := syscall.Kill(pid, sig)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return fmt.Errorf("signal %v to pid %d: %w", sig, pid, domain.ErrProcessGone)
	case err != nil:
		return fmt.Errorf("signal %v to pid %d: %w", sig, pid, err)
	}
	return nil
}
