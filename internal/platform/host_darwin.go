//go:build darwin

package platform

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// NewHostSampler returns a sampler for the current OS. On macOS it reads
// sysctl values (no cgo, no Mach calls), including the kernel's
// memory-pressure verdict, and host CPU from a long-lived iostat child.
//
// The per-CPU tick counters are only available through Mach
// host_statistics/host_processor_info, which require cgo or libSystem
// trampolines, and no sysctl exposes them; iostat reads them for us. While
// it has no fresh reading CPU is reported missing, and the daemon falls
// back to the sum of per-process CPU deltas (app.hostReading), which reads
// low when many processes live shorter than one sample interval.
//
// The sampler implements io.Closer; Close stops iostat (CloseHostSampler).
func NewHostSampler() HostSampler { return &darwinSampler{cpu: newIostatCPU()} }

// darwinSampler is not safe for concurrent use, like every HostSampler.
type darwinSampler struct {
	cpu *iostatCPU
}

// Close stops the iostat child and its reader goroutine. SampleHost still
// works afterwards, without CPU.
func (s *darwinSampler) Close() error {
	s.cpu.close()
	return nil
}

func (s *darwinSampler) SampleHost(ctx context.Context) (domain.HostSample, error) {
	if err := ctx.Err(); err != nil {
		return domain.HostSample{}, fmt.Errorf("sample host: %w", err)
	}
	out := domain.HostSample{Time: time.Now()}
	var fails sampleErrors

	busy, ok, err := s.cpu.busy(out.Time)
	if ok {
		out.CPUUsage = busy
	} else {
		fails.fail(domain.MetricCPU, err)
	}
	noteHostCPU(out.Time, ok)

	if load, err := darwinLoad1(); err != nil {
		fails.fail(domain.MetricLoad, err)
	} else {
		out.Load1 = load
	}

	if total, avail, err := darwinMemory(); err != nil {
		fails.fail(domain.MetricMemory, err)
	} else {
		out.MemTotal, out.MemAvailable = total, avail
	}

	if total, used, err := darwinSwap(); err != nil {
		fails.fail(domain.MetricSwap, err)
	} else {
		out.SwapTotal, out.SwapUsed = total, used
	}

	if p, err := darwinMemoryPressure(); err != nil {
		fails.fail(domain.MetricMemoryPressure, err)
	} else {
		out.MemoryPressure = p
	}

	// PSI is Linux-only.
	fails.fail(domain.MetricMemoryStall, nil)
	fails.fail(domain.MetricIOStall, nil)
	fails.fail(domain.MetricCPUStall, nil)

	return out, fails.finish(&out)
}

// sysctlBytes reads a binary sysctl value of exactly size bytes.
// syscall.Sysctl drops one trailing NUL byte (it assumes a string), so a
// value one byte short is padded back with that zero.
func sysctlBytes(name string, size int) ([]byte, error) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return nil, fmt.Errorf("sysctl %s: %w", name, err)
	}
	b := []byte(s)
	if len(b) == size-1 {
		b = append(b, 0)
	}
	if len(b) != size {
		return nil, fmt.Errorf("sysctl %s: got %d bytes, want %d", name, len(b), size)
	}
	return b, nil
}

// sysctlUint reads an unsigned integer sysctl that the kernel may export as
// either 32 or 64 bits (vm page counters differ across macOS releases).
// After syscall.Sysctl drops a trailing NUL, the lengths 3..4 and 7..8 still
// identify the width unambiguously.
func sysctlUint(name string) (uint64, error) {
	s, err := syscall.Sysctl(name)
	if err != nil {
		return 0, fmt.Errorf("sysctl %s: %w", name, err)
	}
	var b [8]byte
	switch len(s) {
	case 3, 4:
		copy(b[:], s)
		return uint64(binary.NativeEndian.Uint32(b[:4])), nil
	case 7, 8:
		copy(b[:], s)
		return binary.NativeEndian.Uint64(b[:]), nil
	default:
		return 0, fmt.Errorf("sysctl %s: unexpected %d-byte value", name, len(s))
	}
}

// darwinLoad1 returns the 1-minute load average divided by hw.logicalcpu.
// vm.loadavg is struct loadavg { fixpt_t ldavg[3]; long fscale; }, 24 bytes
// on 64-bit macOS with fscale at offset 16.
func darwinLoad1() (float64, error) {
	b, err := sysctlBytes("vm.loadavg", 24)
	if err != nil {
		return 0, err
	}
	ld1 := binary.NativeEndian.Uint32(b[0:4])
	fscale := binary.NativeEndian.Uint64(b[16:24])
	if fscale == 0 {
		return 0, errors.New("sysctl vm.loadavg: zero fscale")
	}
	ncpu, err := sysctlUint("hw.logicalcpu")
	if err != nil {
		return 0, err
	}
	if ncpu == 0 {
		return 0, errors.New("sysctl hw.logicalcpu: zero")
	}
	return float64(ld1) / float64(fscale) / float64(ncpu), nil
}

// darwinMemory returns total and available memory in bytes. Available is
// free + speculative + purgeable + file-backed (pageable external) pages:
// macOS does not expose the inactive queue through sysctl, and file-backed
// pages are what the kernel reclaims without swapping (Activity Monitor's
// "Cached Files"). The sum is clamped to the total.
func darwinMemory() (total, avail uint64, err error) {
	total, err = sysctlUint("hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, errors.New("sysctl hw.memsize: zero")
	}
	pageSize, err := sysctlUint("vm.pagesize")
	if err != nil {
		return 0, 0, err
	}
	var pages uint64
	for _, name := range []string{
		"vm.page_free_count",
		"vm.page_speculative_count",
		"vm.page_purgeable_count",
		"vm.page_pageable_external_count",
	} {
		n, err := sysctlUint(name)
		if err != nil {
			return 0, 0, err
		}
		pages += n
	}
	return total, min(pages*pageSize, total), nil
}

// darwinMemoryPressure reads the kernel's memory-pressure verdict.
// kern.memorystatus_vm_pressure_level is a uint32 holding one of xnu's
// NOTE_MEMORYSTATUS_PRESSURE_* values (bsd/sys/event_private.h), the same
// values as DISPATCH_MEMORYPRESSURE_*: 1 normal, 2 warning, 4 critical.
func darwinMemoryPressure() (domain.MemoryPressure, error) {
	const name = "kern.memorystatus_vm_pressure_level"
	v, err := syscall.SysctlUint32(name)
	if err != nil {
		return domain.MemoryPressureUnknown, fmt.Errorf("sysctl %s: %w", name, err)
	}
	return kernelMemoryPressure(v)
}

// kernelMemoryPressure maps a NOTE_MEMORYSTATUS_PRESSURE_* value to a
// verdict. Any other value is an error, so the metric is reported missing
// rather than guessed.
func kernelMemoryPressure(v uint32) (domain.MemoryPressure, error) {
	switch v {
	case 1:
		return domain.MemoryPressureNormal, nil
	case 2:
		return domain.MemoryPressureWarning, nil
	case 4:
		return domain.MemoryPressureCritical, nil
	default:
		return domain.MemoryPressureUnknown, fmt.Errorf("kern.memorystatus_vm_pressure_level: unexpected value %d", v)
	}
}

// darwinSwap returns total and used swap in bytes. vm.swapusage is
// struct xsw_usage { u_int64_t total, avail, used; u_int32_t pagesize;
// boolean_t encrypted; }, 32 bytes.
func darwinSwap() (total, used uint64, err error) {
	b, err := sysctlBytes("vm.swapusage", 32)
	if err != nil {
		return 0, 0, err
	}
	total = binary.NativeEndian.Uint64(b[0:8])
	used = binary.NativeEndian.Uint64(b[16:24])
	return total, min(used, total), nil
}
