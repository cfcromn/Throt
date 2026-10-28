// Package doctor builds the sanitized diagnostic report printed by
// `throt doctor`. The report is safe to paste into a public issue: it holds
// no process names, arguments, or environment, and home paths and the user
// name are redacted.
package doctor

import (
	"context"
	"os"
	"os/user"
	"runtime"
	"strings"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// Report is the diagnostic output.
type Report struct {
	Version    string  `json:"version"`
	Go         string  `json:"go"`
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
	CPUs       int     `json:"cpus"`
	ConfigPath string  `json:"config_path"`
	SocketPath string  `json:"socket_path"`
	Config     string  `json:"config"` // "ok", "default (no file)", or the error
	Daemon     Daemon  `json:"daemon"`
	Host       Check   `json:"host_metrics"`
	Processes  Check   `json:"process_table"`
	Status     *Status `json:"status,omitempty"`
}

// Daemon describes whether throtd answered.
type Daemon struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Protocol  int    `json:"protocol,omitempty"`
	Uptime    string `json:"uptime,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Check is the result of running one sampler locally.
type Check struct {
	OK      bool            `json:"ok"`
	Missing []domain.Metric `json:"missing,omitempty"`
	Count   int             `json:"count,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Status summarizes the daemon's view without naming any process.
type Status struct {
	Pressure domain.PressureLevel `json:"pressure"`
	Reason   string               `json:"reason,omitempty"`
	Degraded string               `json:"degraded,omitempty"`
	Agents   int                  `json:"agents"`
	Heavy    int                  `json:"heavy_workloads"`
}

// Inputs are the facts and probes doctor needs; cmd/throt supplies them.
type Inputs struct {
	ConfigPath string
	SocketPath string
	ConfigErr  error
	ConfigFile bool // whether a config file exists
	Ping       func(ctx context.Context) (app.PingResult, error)
	Status     func(ctx context.Context) (app.StatusResult, error)
	Host       app.HostSampler
	Processes  app.ProcessSampler
}

// Collect runs every probe and returns the redacted report.
func Collect(ctx context.Context, in Inputs) Report {
	r := Report{
		Version:    app.Version,
		Go:         runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		CPUs:       runtime.NumCPU(),
		ConfigPath: in.ConfigPath,
		SocketPath: in.SocketPath,
		Config:     "ok",
	}
	switch {
	case in.ConfigErr != nil:
		r.Config = in.ConfigErr.Error()
	case !in.ConfigFile:
		r.Config = "default (no file)"
	}

	if p, err := in.Ping(ctx); err != nil {
		r.Daemon.Error = err.Error()
	} else {
		r.Daemon = Daemon{Reachable: true, Version: p.Version, Protocol: p.Protocol, Uptime: p.Uptime}
		if st, err := in.Status(ctx); err == nil {
			r.Status = &Status{Pressure: st.Pressure, Reason: st.Reason, Degraded: st.Degraded, Agents: len(st.Agents), Heavy: len(st.Heavy)}
		}
	}

	// Two host samples, so delta-based metrics (CPU) can report.
	if _, err := in.Host.SampleHost(ctx); err != nil {
		r.Host.Error = err.Error()
	} else if s, err := in.Host.SampleHost(ctx); err != nil {
		r.Host.Error = err.Error()
	} else {
		r.Host = Check{OK: true, Missing: s.Missing}
	}
	if procs, err := in.Processes.SampleProcesses(ctx); err != nil {
		r.Processes.Error = err.Error()
	} else {
		r.Processes = Check{OK: true, Count: len(procs)}
	}
	return redact(r, newRedactor())
}

// redactor replaces identifying strings.
type redactor struct{ pairs []string }

func newRedactor() redactor {
	var pairs []string
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		pairs = append(pairs, home, "~")
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		pairs = append(pairs, u.Username, "<user>")
	}
	return redactor{pairs: pairs}
}

func (rd redactor) apply(s string) string {
	if len(rd.pairs) == 0 {
		return s
	}
	return strings.NewReplacer(rd.pairs...).Replace(s)
}

func redact(r Report, rd redactor) Report {
	for _, p := range []*string{&r.ConfigPath, &r.SocketPath, &r.Config, &r.Daemon.Error, &r.Host.Error, &r.Processes.Error} {
		*p = rd.apply(*p)
	}
	if r.Status != nil {
		r.Status.Degraded = rd.apply(r.Status.Degraded)
	}
	return r
}
