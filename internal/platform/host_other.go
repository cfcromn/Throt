//go:build !darwin && !linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/cfcromn/Throt/internal/domain"
)

// NewHostSampler returns a sampler for the current OS. This OS has no host
// metrics support, so every sample fails with errors.ErrUnsupported.
func NewHostSampler() HostSampler { return unsupportedSampler{} }

type unsupportedSampler struct{}

func (unsupportedSampler) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{}, fmt.Errorf("sample host on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
