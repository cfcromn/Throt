package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
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

func TestPrintStatusReserved(t *testing.T) {
	at := time.Date(2026, 10, 28, 14, 3, 4, 0, time.Local)
	st := app.StatusResult{Host: &domain.HostSample{MemTotal: 1 << 30}, Scheduler: app.SchedulerStatus{Mode: scheduler.ModeEnforce, HeavyLimit: 2, Leases: []app.Lease{
		{ID: "1", Process: domain.ProcessKey{PID: 500}, Agent: "claude-1", Granted: at},
		{ID: "2", Process: domain.ProcessKey{PID: 501}, Granted: at},
	}}}
	var buf bytes.Buffer
	printStatus(&buf, st)
	for _, want := range []string{
		"SCHEDULER enforce (heavy limit 2, 0 queued, 2 reserved)",
		"RESERVED  pid 500 (claude-1) since 14:03:04",
		"RESERVED  pid 501 (-) since 14:03:04",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestPrintStatusStopped(t *testing.T) {
	for n, want := range map[int]string{
		0: "",
		1: "STOPPED   1 process not paused by throtd (see throt doctor)\n",
		3: "STOPPED   3 processes not paused by throtd (see throt doctor)\n",
	} {
		var buf bytes.Buffer
		printStatus(&buf, app.StatusResult{OrphanCount: n})
		out := buf.String()
		if want == "" && strings.Contains(out, "STOPPED") || want != "" && !strings.Contains(out, want) {
			t.Errorf("count %d: output\n%s\nwant %q", n, out, want)
		}
	}
}

func TestParseResumeArgs(t *testing.T) {
	for _, tt := range []struct {
		args  []string
		pid   string
		force bool
		ok    bool
	}{
		{args: []string{"42"}, pid: "42", ok: true},
		{args: []string{"--force", "42"}, pid: "42", force: true, ok: true},
		{args: []string{"42", "--force"}, pid: "42", force: true, ok: true},
		{args: []string{"-force", "42"}, pid: "42", force: true, ok: true},
		{args: nil},
		{args: []string{"--force"}},
		{args: []string{"42", "43"}},
		{args: []string{"--forced", "42"}},
	} {
		pid, force, err := parseResumeArgs(tt.args)
		if (err == nil) != tt.ok || pid != tt.pid || force != tt.force {
			t.Errorf("parseResumeArgs(%q) = %q, %v, %v", tt.args, pid, force, err)
		}
	}
}
