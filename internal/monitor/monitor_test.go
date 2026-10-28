package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

type scripted struct {
	n    int
	fail map[int]bool
}

func (s *scripted) SampleHost(context.Context) (domain.HostSample, error) {
	s.n++
	if s.fail[s.n] {
		return domain.HostSample{}, errors.New("boom")
	}
	return domain.HostSample{CPUUsage: float64(s.n)}, nil
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{fail: map[int]bool{2: true}}
	var samples, errs int
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, time.Millisecond, s, func(domain.HostSample) {
			samples++
			if samples == 3 {
				cancel()
			}
		}, func(error) { errs++ })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if samples != 3 || errs != 1 {
		t.Fatalf("samples=%d errs=%d, want 3 and 1", samples, errs)
	}
}
