package platform

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// sampleTwice takes two samples from a fresh sampler on the real machine.
func sampleTwice(t *testing.T) (first, second domain.HostSample) {
	t.Helper()
	s := NewHostSampler()
	ctx := context.Background()
	first, err := s.SampleHost(ctx)
	if err != nil {
		t.Fatalf("first SampleHost: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	second, err = s.SampleHost(ctx)
	if err != nil {
		t.Fatalf("second SampleHost: %v", err)
	}
	return first, second
}

// checkRanges sanity-checks every metric the sample reports as present.
func checkRanges(t *testing.T, s domain.HostSample) {
	t.Helper()
	if s.Time.IsZero() {
		t.Error("Time is zero")
	}
	if s.Has(domain.MetricCPU) && (s.CPUUsage < 0 || s.CPUUsage > 1) {
		t.Errorf("CPUUsage = %v, want 0..1", s.CPUUsage)
	}
	if s.Has(domain.MetricLoad) && s.Load1 < 0 {
		t.Errorf("Load1 = %v, want >= 0", s.Load1)
	}
	if s.Has(domain.MetricMemory) {
		if s.MemTotal == 0 {
			t.Error("MemTotal = 0")
		}
		if s.MemAvailable > s.MemTotal {
			t.Errorf("MemAvailable %d > MemTotal %d", s.MemAvailable, s.MemTotal)
		}
	}
	if s.Has(domain.MetricSwap) && s.SwapUsed > s.SwapTotal {
		t.Errorf("SwapUsed %d > SwapTotal %d", s.SwapUsed, s.SwapTotal)
	}
	for _, m := range []domain.Metric{domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall} {
		if st, ok := s.StallFor(m); ok && (st.Some < 0 || st.Some > 1 || st.Full < 0 || st.Full > 1) {
			t.Errorf("%s = %+v, want fractions 0..1", m, st)
		}
	}
}

func TestSampleHostDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	first, second := sampleTwice(t)
	for _, s := range []domain.HostSample{first, second} {
		checkRanges(t, s)
		want := []domain.Metric{domain.MetricCPU, domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall}
		if !slices.Equal(s.Missing, want) {
			t.Errorf("Missing = %v, want %v", s.Missing, want)
		}
		if !s.HasMemoryPressure() {
			t.Errorf("MemoryPressure = %v, want a known kernel verdict", s.MemoryPressure)
		}
	}
	t.Logf("kernel memory pressure: %v", second.MemoryPressure)
}

func TestSampleHostLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	first, second := sampleTwice(t)
	if first.Has(domain.MetricCPU) {
		t.Error("first sample has CPU, want missing until a delta exists")
	}
	for _, s := range []domain.HostSample{first, second} {
		checkRanges(t, s)
		// Load is checked in host_linux_test.go: it is missing under a CPU quota.
		if !s.Has(domain.MetricMemory) {
			t.Errorf("metric %s missing", domain.MetricMemory)
		}
		if s.Has(domain.MetricMemoryPressure) {
			t.Error("memory pressure present, want missing on linux")
		}
		for _, m := range []domain.Metric{domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall} {
			st, ok := s.StallFor(m)
			if ok == !s.Has(m) {
				t.Errorf("%s: StallFor ok = %v but Has = %v", m, ok, s.Has(m))
			}
			t.Logf("%s: %+v present=%v", m, st, ok)
		}
	}
}

func TestSampleHostUnsupported(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("supported OS")
	}
	_, err := NewHostSampler().SampleHost(context.Background())
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestSampleHostCanceled(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHostSampler().SampleHost(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSampleErrorsFinish(t *testing.T) {
	var partial sampleErrors
	partial.fail(domain.MetricCPU, nil)
	partial.fail(domain.MetricSwap, errors.New("boom"))
	var s domain.HostSample
	if err := partial.finish(&s); err != nil {
		t.Fatalf("partial finish: %v", err)
	}
	if s.Has(domain.MetricCPU) || s.Has(domain.MetricSwap) || !s.Has(domain.MetricLoad) {
		t.Errorf("Missing = %v", s.Missing)
	}

	var all sampleErrors
	boom := errors.New("boom")
	for _, m := range []domain.Metric{domain.MetricCPU, domain.MetricLoad, domain.MetricMemory, domain.MetricSwap, domain.MetricMemoryPressure,
		domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall} {
		all.fail(m, boom)
	}
	if err := all.finish(&s); !errors.Is(err, boom) {
		t.Fatalf("all-missing finish err = %v, want wrapping boom", err)
	}
}
