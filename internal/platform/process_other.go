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

type unsupportedProcessSampler struct{}

func (unsupportedProcessSampler) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return nil, fmt.Errorf("sample processes on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
