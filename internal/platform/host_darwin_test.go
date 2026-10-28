//go:build darwin

package platform

import (
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestKernelMemoryPressure(t *testing.T) {
	tests := []struct {
		in      uint32
		want    domain.MemoryPressure
		wantErr bool
	}{
		{1, domain.MemoryPressureNormal, false},
		{2, domain.MemoryPressureWarning, false},
		{4, domain.MemoryPressureCritical, false},
		{0, domain.MemoryPressureUnknown, true},
		{3, domain.MemoryPressureUnknown, true},
		{8, domain.MemoryPressureUnknown, true},
	}
	for _, tt := range tests {
		got, err := kernelMemoryPressure(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("kernelMemoryPressure(%d) = %v, %v; want %v, err %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestDarwinMemoryPressureReads(t *testing.T) {
	p, err := darwinMemoryPressure()
	if err != nil {
		t.Fatalf("darwinMemoryPressure: %v", err)
	}
	if !p.Known() {
		t.Fatalf("verdict = %v, want known", p)
	}
}
