package domain

import "time"

// Metric names a host signal. Used to report which signals a sample lacks.
type Metric string

const (
	MetricCPU    Metric = "cpu"
	MetricLoad   Metric = "load"
	MetricMemory Metric = "memory"
	MetricSwap   Metric = "swap"
	// MetricLatency is foreground responsiveness: how late a short timer
	// fires, which grows when the CPU scheduler is saturated.
	MetricLatency Metric = "latency"
	// MetricMemoryPressure is the kernel's own memory-pressure verdict
	// (macOS kern.memorystatus_vm_pressure_level). Other platforms report
	// it missing.
	MetricMemoryPressure Metric = "memory_pressure"
	// MetricMemoryStall, MetricIOStall and MetricCPUStall are the Linux
	// kernel's pressure stall information (PSI, /proc/pressure/*): the share
	// of recent wall time in which tasks waited on memory, I/O or CPU. Other
	// platforms, and Linux kernels without PSI, report them missing.
	MetricMemoryStall Metric = "memory_stall"
	MetricIOStall     Metric = "io_stall"
	MetricCPUStall    Metric = "cpu_stall"
)

// Stall is one PSI resource reading: the share of the last 10 seconds
// (avg10 / 100) in which some task (Some), or every non-idle task at once
// (Full), was stalled on the resource. Both are fractions, 0..1.
type Stall struct {
	Some float64 `json:"some"`
	Full float64 `json:"full"`
}

// HostSample is one observation of host-wide resources. Fields for metrics
// listed in Missing are zero and must be ignored.
type HostSample struct {
	Time time.Time `json:"time"`
	// CPUUsage is the busy fraction of all CPUs since the previous sample, 0..1.
	CPUUsage float64 `json:"cpu_usage"`
	// Load1 is the 1-minute load average divided by the logical CPU count.
	Load1 float64 `json:"load1"`
	// MemTotal and MemAvailable are bytes; available means reclaimable
	// without swapping (free + inactive/cached as the platform defines it).
	MemTotal     uint64 `json:"mem_total"`
	MemAvailable uint64 `json:"mem_available"`
	SwapTotal    uint64 `json:"swap_total"`
	SwapUsed     uint64 `json:"swap_used"`
	// Latency is the timer-lateness probe result (see MetricLatency).
	Latency time.Duration `json:"latency"`
	// MemoryPressure is the kernel memory-pressure verdict (see
	// MetricMemoryPressure). The zero value is unknown and counts as missing
	// even when MetricMemoryPressure is not listed in Missing.
	MemoryPressure MemoryPressure `json:"memory_pressure,omitempty"`
	// MemoryStall, IOStall and CPUStall are PSI readings (see
	// MetricMemoryStall). nil is unknown and counts as missing even when the
	// metric is not listed in Missing, so a sample from a producer that
	// predates PSI never reads as zero stall.
	MemoryStall *Stall `json:"memory_stall,omitempty"`
	IOStall     *Stall `json:"io_stall,omitempty"`
	CPUStall    *Stall `json:"cpu_stall,omitempty"`
	// Missing lists metrics the platform could not provide for this sample.
	Missing []Metric `json:"missing,omitempty"`
}

// Has reports whether m is present in the sample.
func (s HostSample) Has(m Metric) bool {
	for _, x := range s.Missing {
		if x == m {
			return false
		}
	}
	return true
}

// HasMemoryPressure reports whether the sample carries a known kernel
// memory-pressure verdict.
func (s HostSample) HasMemoryPressure() bool {
	return s.Has(MetricMemoryPressure) && s.MemoryPressure.Known()
}

// StallFor returns the PSI reading for m (MetricMemoryStall, MetricIOStall
// or MetricCPUStall) and whether the sample carries it.
func (s HostSample) StallFor(m Metric) (Stall, bool) {
	var p *Stall
	switch m {
	case MetricMemoryStall:
		p = s.MemoryStall
	case MetricIOStall:
		p = s.IOStall
	case MetricCPUStall:
		p = s.CPUStall
	}
	if p == nil || !s.Has(m) {
		return Stall{}, false
	}
	return *p, true
}

// MemoryPressure is a kernel memory-pressure verdict, ordered by severity.
// The zero value is unknown.
type MemoryPressure int

const (
	MemoryPressureUnknown MemoryPressure = iota
	MemoryPressureNormal
	MemoryPressureWarning
	MemoryPressureCritical
)

var memoryPressureNames = [...]string{"unknown", "normal", "warning", "critical"}

// Known reports whether p is a defined verdict other than unknown.
func (p MemoryPressure) Known() bool {
	return p > MemoryPressureUnknown && int(p) < len(memoryPressureNames)
}

func (p MemoryPressure) String() string {
	if p < 0 || int(p) >= len(memoryPressureNames) {
		return "invalid"
	}
	return memoryPressureNames[p]
}

func (p MemoryPressure) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

func (p *MemoryPressure) UnmarshalText(b []byte) error {
	for i, n := range memoryPressureNames {
		if n == string(b) {
			*p = MemoryPressure(i)
			return nil
		}
	}
	*p = MemoryPressureUnknown
	return nil
}

// PressureLevel is the stabilized host pressure state.
type PressureLevel int

const (
	PressureUnknown PressureLevel = iota
	PressureNormal
	PressureElevated
	PressureHigh
	PressureCritical
)

var pressureNames = [...]string{"unknown", "normal", "elevated", "high", "critical"}

func (p PressureLevel) String() string {
	if p < 0 || int(p) >= len(pressureNames) {
		return "invalid"
	}
	return pressureNames[p]
}

func (p PressureLevel) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

func (p *PressureLevel) UnmarshalText(b []byte) error {
	for i, n := range pressureNames {
		if n == string(b) {
			*p = PressureLevel(i)
			return nil
		}
	}
	*p = PressureUnknown
	return nil
}

// EventPressureChanged is emitted when the stabilized level transitions.
const EventPressureChanged EventKind = "pressure_changed"
