package domain

import "testing"

func TestHostSampleSignals(t *testing.T) {
	stall := &Stall{Some: 0.25, Full: 0.1}
	full := HostSample{MemoryPressure: MemoryPressureWarning, MemoryStall: stall, IOStall: stall, CPUStall: stall}
	for _, tc := range []struct {
		name     string
		s        HostSample
		m        Metric
		has      bool
		pressure bool // HasMemoryPressure
		stall    bool // StallFor(m) reports a reading
	}{
		{"all present", full, MetricMemoryStall, true, true, true},
		{"io stall", full, MetricIOStall, true, true, true},
		{"cpu stall", full, MetricCPUStall, true, true, true},
		{"not a stall metric", full, MetricCPU, true, true, false},
		{"listed missing", HostSample{MemoryStall: stall, MemoryPressure: MemoryPressureNormal, Missing: []Metric{MetricLoad, MetricMemoryStall, MetricMemoryPressure}},
			MetricMemoryStall, false, false, false},
		// A producer that predates PSI or the kernel verdict leaves them
		// zero without listing them: that is unknown, never zero stall.
		{"zero values are unknown", HostSample{}, MetricIOStall, true, false, false},
		{"invalid verdict is unknown", HostSample{MemoryPressure: MemoryPressure(9)}, MetricCPU, true, false, false},
	} {
		if got := tc.s.Has(tc.m); got != tc.has {
			t.Errorf("%s: Has(%s) = %v, want %v", tc.name, tc.m, got, tc.has)
		}
		if got := tc.s.HasMemoryPressure(); got != tc.pressure {
			t.Errorf("%s: HasMemoryPressure = %v, want %v", tc.name, got, tc.pressure)
		}
		got, ok := tc.s.StallFor(tc.m)
		if ok != tc.stall || (ok && got != *stall) || (!ok && got != (Stall{})) {
			t.Errorf("%s: StallFor(%s) = %+v, %v; want present %v", tc.name, tc.m, got, ok, tc.stall)
		}
	}
}

func TestLevelText(t *testing.T) {
	for _, tc := range []struct {
		p    PressureLevel
		text string
	}{
		{PressureUnknown, "unknown"}, {PressureNormal, "normal"}, {PressureElevated, "elevated"},
		{PressureHigh, "high"}, {PressureCritical, "critical"},
	} {
		b, _ := tc.p.MarshalText()
		var back PressureLevel
		if err := back.UnmarshalText(b); err != nil || string(b) != tc.text || back != tc.p {
			t.Errorf("%d: %q -> %v, %v", tc.p, b, back, err)
		}
	}
	for _, tc := range []struct {
		m    MemoryPressure
		text string
	}{
		{MemoryPressureUnknown, "unknown"}, {MemoryPressureNormal, "normal"},
		{MemoryPressureWarning, "warning"}, {MemoryPressureCritical, "critical"},
	} {
		b, _ := tc.m.MarshalText()
		var back MemoryPressure
		if err := back.UnmarshalText(b); err != nil || string(b) != tc.text || back != tc.m {
			t.Errorf("%d: %q -> %v, %v", tc.m, b, back, err)
		}
		if tc.m.Known() != (tc.m != MemoryPressureUnknown) {
			t.Errorf("%s: Known = %v", tc.m, tc.m.Known())
		}
	}
	// Out-of-range values never index past the names, and unknown text from
	// a newer peer decodes as unknown rather than failing the message.
	if PressureLevel(-1).String() != "invalid" || PressureLevel(5).String() != "invalid" ||
		MemoryPressure(-1).String() != "invalid" || MemoryPressure(4).String() != "invalid" || MemoryPressure(-1).Known() {
		t.Error("out-of-range values not reported invalid")
	}
	p, m := PressureHigh, MemoryPressureCritical
	if err := p.UnmarshalText([]byte("severe")); err != nil || p != PressureUnknown {
		t.Errorf("PressureLevel from unknown text = %v, %v", p, err)
	}
	if err := m.UnmarshalText([]byte("severe")); err != nil || m != MemoryPressureUnknown {
		t.Errorf("MemoryPressure from unknown text = %v, %v", m, err)
	}
}
