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
)

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
