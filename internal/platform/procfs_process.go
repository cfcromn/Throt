package platform

// Parsers for Linux per-process procfs files. They are pure functions and
// carry no build constraint so they are tested on every OS. They run for
// every process on every sample, so they split fields in place instead of
// allocating with strings.Fields.

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
	"unicode/utf8"

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
	s, comm, err := parsePIDStatRaw(b)
	if err != nil {
		return pidStat{}, err
	}
	s.Comm = sanitizeComm(comm)
	return s, nil
}

// statFields is the number of /proc/<pid>/stat fields after comm that
// parsePIDStatRaw reads: state (field 3) through rss (field 24).
const statFields = 22

// parsePIDStatRaw is parsePIDStat leaving Comm empty: it returns comm's raw
// bytes instead, a subslice of b, so a caller can compare them with a
// cached name before allocating a new one.
func parsePIDStatRaw(b []byte) (pidStat, []byte, error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return pidStat{}, nil, errors.New("parse stat: no comm")
	}
	var s pidStat
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return pidStat{}, nil, fmt.Errorf("parse stat pid: %w", err)
	}
	if pid <= 0 {
		return pidStat{}, nil, fmt.Errorf("parse stat: invalid pid %d", pid)
	}
	s.PID = pid

	// rest starts at field 3 (state); field n is rest[n-3].
	var rest [statFields][]byte
	if n := splitFields(b[end+1:], rest[:]); n < statFields {
		return pidStat{}, nil, fmt.Errorf("parse stat: %d fields after comm, want >= %d", n, statFields)
	}
	if s.State, err = statState(rest[0]); err != nil {
		return pidStat{}, nil, err
	}
	field := func(n int) (uint64, error) {
		v, err := strconv.ParseUint(string(rest[n-3]), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse stat field %d: %w", n, err)
		}
		return v, nil
	}
	ppid, err := field(4)
	if err != nil {
		return pidStat{}, nil, err
	}
	if ppid > math.MaxInt32 { // pid_t is 32 bits
		return pidStat{}, nil, fmt.Errorf("parse stat: invalid ppid %d", ppid)
	}
	utime, err := field(14)
	if err != nil {
		return pidStat{}, nil, err
	}
	stime, err := field(15)
	if err != nil {
		return pidStat{}, nil, err
	}
	if s.StartTime, err = field(22); err != nil {
		return pidStat{}, nil, err
	}
	if utime > maxTicks || stime > maxTicks-utime || s.StartTime > maxTicks {
		return pidStat{}, nil, errors.New("parse stat: times out of range")
	}
	// rss (field 24) is signed in the kernel; a negative value is malformed.
	if s.RSSPages, err = field(24); err != nil {
		return pidStat{}, nil, err
	}
	s.PPID, s.CPUTicks = int(ppid), utime+stime
	return s, b[open+1 : end], nil
}

// splitFields splits b around runs of white space, as bytes.Fields does,
// and stores the first len(dst) fields in dst. It returns the number of
// fields, counting no further than len(dst) when b is ASCII, which is all
// the kernel writes there; it allocates only when b is not.
func splitFields(b []byte, dst [][]byte) int {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			fields := bytes.Fields(b) // Unicode spaces split too
			copy(dst, fields)
			return len(fields)
		}
	}
	n := 0
	for i := 0; i < len(b) && n < len(dst); {
		for i < len(b) && asciiSpace(b[i]) {
			i++
		}
		j := i
		for j < len(b) && !asciiSpace(b[j]) {
			j++
		}
		if j > i {
			dst[n] = b[i:j]
			n++
		}
		i = j
	}
	return n
}

// asciiSpace reports whether c is an ASCII byte bytes.Fields splits on.
func asciiSpace(c byte) bool {
	switch c {
	case '\t', '\n', '\v', '\f', '\r', ' ':
		return true
	}
	return false
}

// statState maps field 3 of /proc/<pid>/stat, one state letter, to a
// ProcessState. T is stopped by a signal; t is stopped by a tracer
// (ptrace), which a debugger owns, so it is StateOther.
func statState(f []byte) (domain.ProcessState, error) {
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
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("read status: %w", err)
	}
	return statusUID(b)
}

// statusUID returns the real uid from the first line of b, the contents of
// /proc/<pid>/status, that starts with "Uid:". Lines end at '\n', less a
// trailing '\r', as bufio.ScanLines splits them.
func statusUID(b []byte) (uint32, error) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		rest, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte{'\r'}), []byte("Uid:"))
		if !ok {
			continue
		}
		var first [1][]byte
		if splitFields(rest, first[:]) == 0 {
			return 0, errors.New("parse status: empty Uid line")
		}
		v, err := strconv.ParseUint(string(first[0]), 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse status Uid: %w", err)
		}
		return uint32(v), nil
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

// sanitizeComm returns comm as a string, replacing control characters, which
// a process may put in its own comm (e.g. a newline), with '?' as procps
// does, and keeping other bytes.
func sanitizeComm(b []byte) string {
	if !hasControl(b) {
		return string(b)
	}
	out := bytes.Clone(b)
	for i, c := range out {
		if isControl(c) {
			out[i] = '?'
		}
	}
	return string(out)
}

// commEqual reports whether sanitizeComm(b) would return s, without
// allocating.
func commEqual(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i, c := range b {
		if isControl(c) {
			c = '?'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

func hasControl(b []byte) bool {
	for _, c := range b {
		if isControl(c) {
			return true
		}
	}
	return false
}

func isControl(c byte) bool { return c < 0x20 || c == 0x7f }
