// Package platform holds OS-specific adapters behind small, capability-shaped
// interfaces.
package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/cfcromn/Throt/internal/domain"
)

// HostSampler observes host-wide resources.
//
// Implementations may keep state between calls (for example, CPU counters to
// compute a busy fraction from deltas) and are not safe for concurrent use;
// a single goroutine should own a sampler.
type HostSampler interface {
	// SampleHost returns one observation. Metrics that could not be read are
	// listed in HostSample.Missing; an error is returned only when no metric
	// could be read at all.
	SampleHost(ctx context.Context) (domain.HostSample, error)
}

// metricCount is the number of sampled metrics a HostSample can carry (cpu,
// load, memory, swap, memory pressure; latency is probed by the daemon).
const metricCount = 5

// sampleErrors accumulates per-metric read failures for one sample.
type sampleErrors struct {
	missing []domain.Metric
	errs    []error
}

// fail records that m could not be read because of err (which may be nil
// when the metric is simply not yet defined, such as a first CPU sample).
func (e *sampleErrors) fail(m domain.Metric, err error) {
	e.missing = append(e.missing, m)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", m, err))
	}
}

// finish sets s.Missing and returns an error if every metric is missing.
func (e *sampleErrors) finish(s *domain.HostSample) error {
	s.Missing = e.missing
	if len(e.missing) >= metricCount {
		return fmt.Errorf("sample host: no metric available: %w", errors.Join(e.errs...))
	}
	return nil
}
