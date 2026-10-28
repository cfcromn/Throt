package platform

// Parsers for Linux per-process procfs files. They are pure functions and
// carry no build constraint so they are tested on every OS.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat times.
// ponytail: assumes CLK_TCK=100 (true on all mainstream Linux ABIs); reading
// it via sysconf needs cgo, and the auxv AT_CLKTCK route was not worth it yet.
const clockTicks = 100

// maxTicks is the largest tick count that converts to a time.Duration.
const maxTicks = math.MaxInt64 / uint64(time.Second/clockTicks)

// ticksToDuration converts clock ticks to a duration. ticks must not exceed
// maxTicks; parsePIDStat guarantees that.
func ticksToDuration(ticks uint64) time.Duration {
	return time.Duration(ticks) * (time.Second / clockTicks)
}

// pidStat holds the /proc/<pid>/stat fields Throt uses.
type pidStat struct {
	PID       int
	Comm      string
	State     domain.ProcessState
	PPID      int
	CPUTicks  uint64 // utime + stime
	StartTime uint64 // clock ticks after boot
	RSSPages  uint64
}

// parsePIDStat parses /proc/<pid>/stat. comm is enclosed in parentheses and
// may itself contain spaces and ')', so it ends at the last ')'.
func parsePIDStat(b []byte) (pidStat, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return pidStat{}, errors.New("parse stat: no comm")
	}
	var s pidStat
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return pidStat{}, fmt.Errorf("parse stat pid: %w", err)
	}
	if pid <= 0 {
		return pidStat{}, fmt.Errorf("parse stat: invalid pid %d", pid)
	}
	s.PID, s.Comm = pid, sanitizeComm(b[open+1:end])

	// rest starts at field 3 (state); field n is rest[n-3].
	rest := strings.Fields(string(b[end+1:]))
	if len(rest) < 22 {
		return pidStat{}, fmt.Errorf("parse stat: %d fields after comm, want >= 22", len(rest))
	}
	if s.State, err = statState(rest[0]); err != nil {
		return pidStat{}, err
	}
	field := func(n int) (uint64, error) {
		v, err := strconv.ParseUint(rest[n-3], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse stat field %d: %w", n, err)
		}
		return v, nil
	}
	ppid, err := field(4)
	if err != nil {
		return pidStat{}, err
	}
	if ppid > math.MaxInt32 { // pid_t is 32 bits
		return pidStat{}, fmt.Errorf("parse stat: invalid ppid %d", ppid)
	}
	utime, err := field(14)
	if err != nil {
		return pidStat{}, err
	}
	stime, err := field(15)
	if err != nil {
		return pidStat{}, err
	}
	if s.StartTime, err = field(22); err != nil {
		return pidStat{}, err
	}
	if utime > maxTicks || stime > maxTicks-utime || s.StartTime > maxTicks {
		return pidStat{}, errors.New("parse stat: times out of range")
	}
	// rss (field 24) is signed in the kernel; a negative value is malformed.
	if s.RSSPages, err = field(24); err != nil {
		return pidStat{}, err
	}
	s.PPID, s.CPUTicks = int(ppid), utime+stime
	return s, nil
}

// statState maps field 3 of /proc/<pid>/stat, one state letter, to a
// ProcessState. T is stopped by a signal; t is stopped by a tracer
// (ptrace), which a debugger owns, so it is StateOther.
func statState(f string) (domain.ProcessState, error) {
	if len(f) != 1 {
		return "", fmt.Errorf("parse stat: state %q is not one letter", f)
	}
	switch f[0] {
	case 'R':
		return domain.StateRunning, nil
	case 'S', 'D', 'I':
		return domain.StateSleeping, nil
	case 'T':
		return domain.StateStopped, nil
	case 'Z':
		return domain.StateZombie, nil
	}
	return domain.StateOther, nil
}

// parseStatusUID returns the real uid from the "Uid:" line of
// /proc/<pid>/status.
func parseStatusUID(r io.Reader) (uint32, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Uid:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, errors.New("parse status: empty Uid line")
		}
		v, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse status Uid: %w", err)
		}
		return uint32(v), nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read status: %w", err)
	}
	return 0, errors.New("parse status: no Uid line")
}

// parseBootTime returns btime (boot time, Unix seconds) from /proc/stat.
func parseBootTime(r io.Reader) (time.Time, error) {
	sc := bufio.NewScanner(r)
	// The intr line grows with the IRQ count and can exceed the 64 KiB default.
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "btime ")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || v <= 0 {
			return time.Time{}, fmt.Errorf("parse /proc/stat btime %q", rest)
		}
		return time.Unix(v, 0), nil
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	return time.Time{}, errors.New("parse /proc/stat: no btime line")
}

// sanitizeComm replaces control characters, which a process may put in its
// own comm (e.g. a newline), with '?' as procps does, and keeps other bytes.
func sanitizeComm(b []byte) string {
	out := bytes.Clone(b)
	for i, c := range out {
		if c < 0x20 || c == 0x7f {
			out[i] = '?'
		}
	}
	return string(out)
}
