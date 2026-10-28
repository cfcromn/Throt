package platform

import (
	"context"

	"github.com/cfcromn/Throt/internal/domain"
)

// ProcessSampler enumerates the process table.
type ProcessSampler interface {
	// SampleProcesses returns every process visible to the current user.
	// Processes that cannot be read (for example because they exit during
	// the scan) are skipped; an error is returned only when the process list
	// as a whole cannot be read.
	SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error)
}
