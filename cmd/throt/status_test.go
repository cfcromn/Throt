package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

func TestPrintStatusPSI(t *testing.T) {
	host := domain.HostSample{
		MemTotal: 16 << 30, MemAvailable: 8 << 30,
		MemoryStall: &domain.Stall{Some: 0.031, Full: 0.012},
		IOStall:     &domain.Stall{Some: 0.2, Full: 0},
		CPUStall:    &domain.Stall{Some: 0.125, Full: 0.5},
	}
	tests := []struct {
		name    string
		missing []domain.Metric
		want    string // "" means no PSI line
	}{
		{name: "all present", want: "PSI       mem 3% (full 1%)  io 20% (full 0%)  cpu 12%\n"},
		{name: "io missing", missing: []domain.Metric{domain.MetricIOStall}, want: "PSI       mem 3% (full 1%)  cpu 12%\n"},
		{name: "none", missing: []domain.Metric{domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := host
			h.Missing = tt.missing
			var buf bytes.Buffer
			printStatus(&buf, app.StatusResult{Host: &h})
			out := buf.String()
			if tt.want == "" {
				if strings.Contains(out, "PSI") {
					t.Errorf("PSI shown without readings:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, out)
			}
		})
	}
}
