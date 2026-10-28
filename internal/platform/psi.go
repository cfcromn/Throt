package platform

// Parsers and rules for Linux pressure stall information (PSI). Like
// cgroup.go they carry no build constraint so they are tested on every OS.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cfcromn/Throt/internal/domain"
)

// parsePSI parses a PSI file (/proc/pressure/{cpu,memory,io} or a cgroup v2
// *.pressure file):
//
//	some avg10=1.23 avg60=0.50 avg300=0.10 total=123456
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// It returns avg10 / 100 of each line as a fraction, clamped to 0..1. The
// "some" line is required; "full" is optional (host-wide cpu before Linux
// 5.13 has none) and reads as zero when absent. Unknown lines and fields
// are ignored.
func parsePSI(r io.Reader) (domain.Stall, error) {
	var (
		st      domain.Stall
		hasSome bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		kind, rest, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		var dst *float64
		switch kind {
		case "some":
			dst = &st.Some
		case "full":
			dst = &st.Full
		default:
			continue
		}
		v, err := psiAvg10(rest)
		if err != nil {
			return domain.Stall{}, fmt.Errorf("psi %s line: %w", kind, err)
		}
		*dst = v
		hasSome = hasSome || kind == "some"
	}
	if err := sc.Err(); err != nil {
		return domain.Stall{}, fmt.Errorf("read psi: %w", err)
	}
	if !hasSome {
		return domain.Stall{}, errors.New("psi: no some line")
	}
	return st, nil
}

// psiAvg10 returns the avg10 field of a PSI line's fields as a fraction.
func psiAvg10(fields string) (float64, error) {
	for _, f := range strings.Fields(fields) {
		v, ok := strings.CutPrefix(f, "avg10=")
		if !ok {
			continue
		}
		pct, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(pct) {
			return 0, fmt.Errorf("bad avg10 %q", v)
		}
		return max(0, min(pct/100, 1)), nil
	}
	return 0, errors.New("no avg10")
}

// readPSI reads and parses the PSI file at path.
func readPSI(path string) (domain.Stall, error) {
	f, err := os.Open(path)
	if err != nil {
		return domain.Stall{}, err
	}
	defer f.Close()
	return parsePSI(f)
}

// applyStalls sets the PSI readings of out from hostDir (/proc/pressure on
// Linux), marking each one that cannot be read missing in fails. Kernels
// before 4.20, or booted with psi=0, lack the files or fail the read.
//
// The cgroup choice follows the rule the other cgroup-aware metrics use: a
// resource is measured in the cgroup that limits it. memCgroup is the
// cgroup with the tightest memory.max (memory.pressure is read there) and
// cpuCgroup the one whose cpu.max quota binds (cpu.pressure); either is ""
// when no limit applies. I/O has no limit rule, so it is always host-wide.
// A failed cgroup read falls back to the host file.
func applyStalls(out *domain.HostSample, fails *sampleErrors, hostDir, memCgroup, cpuCgroup string) {
	read := func(m domain.Metric, cgroup, cgFile, hostFile string) *domain.Stall {
		if cgroup != "" {
			if st, err := readPSI(filepath.Join(cgroup, cgFile)); err == nil {
				return &st
			}
		}
		st, err := readPSI(filepath.Join(hostDir, hostFile))
		if err != nil {
			fails.fail(m, err)
			return nil
		}
		return &st
	}
	out.MemoryStall = read(domain.MetricMemoryStall, memCgroup, "memory.pressure", "memory")
	out.IOStall = read(domain.MetricIOStall, "", "", "io")
	out.CPUStall = read(domain.MetricCPUStall, cpuCgroup, "cpu.pressure", "cpu")
}
