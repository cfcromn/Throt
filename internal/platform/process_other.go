//go:build !darwin && !linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/cfcromn/Throt/internal/domain"
)

// NewProcessSampler returns a process sampler for the current OS. This OS
// has no process enumeration support, so every sample fails with
// errors.ErrUnsupported.
func NewProcessSampler() ProcessSampler { return unsupportedProcessSampler{} }

// NewProcessController returns a process controller for the current OS. This
// OS has no process control support, so every call fails with
// errors.ErrUnsupported.
func NewProcessController() ProcessController { return unsupportedProcessController{} }

// NewProcessTools returns a sampler and a controller; on this OS both are
// unsupported.
func NewProcessTools() (ProcessSampler, ProcessController) {
	return NewProcessSampler(), NewProcessController()
}

type unsupportedProcessSampler struct{}

func (unsupportedProcessSampler) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return nil, fmt.Errorf("sample processes on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

type unsupportedProcessController struct{}

func (unsupportedProcessController) Stop(context.Context, domain.ProcessKey, uint32) error {
	return fmt.Errorf("stop process on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (unsupportedProcessController) Continue(context.Context, domain.ProcessKey, uint32) error {
	return fmt.Errorf("continue process on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (unsupportedProcessController) Identify(context.Context, int) (domain.ProcessSample, error) {
	return domain.ProcessSample{}, fmt.Errorf("process lookup on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (unsupportedProcessController) Gone(context.Context, domain.ProcessKey) (bool, error) {
	return false, fmt.Errorf("process lookup on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
