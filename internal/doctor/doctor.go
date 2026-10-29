// Package doctor builds the sanitized diagnostic report printed by
// `throt doctor`. The report is safe to paste into a public issue: it holds
// no arguments or environment, names no process except stopped ones throtd
// did not pause and busy unattributed java (by executable base name only),
// and home paths and the user name are redacted.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
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
	Orphans    Orphans `json:"orphans"`
	// Findings are setup problems worth fixing, one sentence each.
	Findings []string `json:"findings,omitempty"`
}

// maxDaemonFindings bounds the unattributed java findings.
const maxDaemonFindings = 3

// daemonFindings flags busy heavy java processes no agent owns. They are
// almost always Gradle or Kotlin daemons, which builds started by an agent
// hand their work to, so throtd can neither attribute nor queue those
// builds. Only the PID and CPU are reported.
func daemonFindings(heavy []process.WorkloadView) []string {
	var out []string
	for _, h := range heavy {
		if h.Name != "java" || h.Owner != "" || h.CPU <= 0 || len(out) == maxDaemonFindings {
			continue
		}
		out = append(out, fmt.Sprintf("unattributed java (pid %d, %.0f%% CPU) is probably a Gradle or Kotlin daemon: "+
			"builds there cannot be queued; set org.gradle.daemon=false in ~/.gradle/gradle.properties so they run under the agent",
			h.Key.PID, h.CPU*100))
	}
	return out
}

// Orphans lists stopped processes of the user that throtd did not pause:
// left stopped by a throtd killed before it recorded them, or suspended
// with Ctrl-Z. Throt never resumes them on its own.
type Orphans struct {
	// Source is "throtd" (the daemon's view, which excludes its own
	// pauses) or "local" (throtd is down, so nothing excludes a pause
	// recorded in paused.json; throtd resumes those when it starts).
	Source string `json:"source"`
	// Count counts them all; Entries lists at most process.MaxOrphans.
	Count   int           `json:"count"`
	Entries []OrphanEntry `json:"entries,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// OrphanEntry is one stopped process, named by executable base name only.
type OrphanEntry struct {
	PID   int    `json:"pid"`
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
	Hint  string `json:"hint"`
	// StoppedFor is how long it has been observed stopped, when known.
	StoppedFor string `json:"stopped_for,omitempty"`
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
	// Derived explains host metrics the sampler lacks but throtd computes
	// another way, such as CPU from per-process CPU on macOS.
	Derived map[domain.Metric]string `json:"derived,omitempty"`
	Count   int                      `json:"count,omitempty"`
	Error   string                   `json:"error,omitempty"`
}

// derivedCPU is how throtd fills host CPU without a host CPU counter.
const derivedCPU = "derived from process CPU by throtd"

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
	// Adapters and UID attribute stopped processes when throtd is down.
	Adapters []agent.Adapter
	UID      uint32
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

	now := time.Now()
	orphansKnown := false
	if p, err := in.Ping(ctx); err != nil {
		r.Daemon.Error = err.Error()
	} else {
		r.Daemon = Daemon{Reachable: true, Version: p.Version, Protocol: p.Protocol, Uptime: p.Uptime}
		if st, err := in.Status(ctx); err == nil {
			r.Status = &Status{Pressure: st.Pressure, Reason: st.Reason, Degraded: st.Degraded, Agents: len(st.Agents), Heavy: len(st.Heavy)}
			r.Orphans = orphanReport("throtd", st.Orphans, st.OrphanCount, now)
			r.Findings = daemonFindings(st.Heavy)
			orphansKnown = true
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
		if !orphansKnown {
			r.Orphans = Orphans{Source: "local", Error: err.Error()}
		}
	} else {
		r.Processes = Check{OK: true, Count: len(procs)}
		// Like the daemon (app.hostReading): without a host CPU counter,
		// CPU is the sum of per-process CPU deltas.
		if r.Host.OK && slices.Contains(r.Host.Missing, domain.MetricCPU) {
			r.Host.Missing = slices.DeleteFunc(slices.Clone(r.Host.Missing), func(m domain.Metric) bool { return m == domain.MetricCPU })
			r.Host.Derived = map[domain.Metric]string{domain.MetricCPU: derivedCPU}
		}
		if !orphansKnown {
			g := process.NewGraph(in.Adapters, process.WithAgentUID(in.UID))
			g.Update(now, procs)
			list, total := process.FindOrphans(g.Snapshot().Stopped, in.UID, func(domain.ProcessKey) bool { return false })
			r.Orphans = orphanReport("local", list, total, now)
		}
	}
	return redact(r, newRedactor())
}

// orphanReport keeps only PIDs, base names, owner labels and hints: nothing
// that carries arguments or paths.
func orphanReport(source string, list []process.Orphan, total int, now time.Time) Orphans {
	o := Orphans{Source: source, Count: total}
	for _, x := range list {
		e := OrphanEntry{PID: x.Key.PID, Name: x.Name, Owner: x.Owner, Hint: x.Hint}
		if d := now.Sub(x.Since); !x.Since.IsZero() && d >= time.Second {
			e.StoppedFor = d.Round(time.Second).String()
		}
		o.Entries = append(o.Entries, e)
	}
	return o
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
	r.Orphans.Error = rd.apply(r.Orphans.Error)
	for i := range r.Orphans.Entries {
		e := &r.Orphans.Entries[i]
		e.Name, e.Owner = rd.apply(e.Name), rd.apply(e.Owner)
	}
	return r
}
