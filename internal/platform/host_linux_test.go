//go:build linux

package platform

import (
	"context"
	"runtime"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestSampleHostLinuxLoad(t *testing.T) {
	s, err := NewHostSampler().SampleHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, quotaBinds := cgroupCPULimit(locateCgroup(), runtime.NumCPU())
	if s.Has(domain.MetricLoad) == quotaBinds {
		t.Errorf("load present = %v with CPU quota binding = %v; want exactly one", s.Has(domain.MetricLoad), quotaBinds)
	}
}
