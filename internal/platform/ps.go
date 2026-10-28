package platform

// Parser for macOS ps(1) output. It is a pure function over io.Reader and
// carries no build constraint so it is tested on every OS.

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// psStatColumns has no free-text column, so every record is unambiguous and
// the set of PIDs it lists is authoritative.
const psStatColumns = "pid=,ppid=,uid=,rss=,time=,lstart="

// psNameColumns lists executable names. comm may contain spaces or even
// newlines, so this pass is parsed defensively. args/command are never
// requested: they can contain secrets.
const psNameColumns = "pid=,comm="

// psLstartLayout is lstart in the C locale, e.g. "Sun Oct 18 11:06:02 2026".
// Fields are re-joined with single spaces, so a padded day still matches.
const psLstartLayout = "Mon Jan 2 15:04:05 2006"

// parsePS combines the stats and names passes. lstart is interpreted in loc.
// Malformed lines are skipped; an error is returned only when a reader fails.
func parsePS(stats, names io.Reader, loc *time.Location) ([]domain.ProcessSample, error) {
	var out []domain.ProcessSample
	seen := make(map[int]bool)
	sc := bufio.NewScanner(stats)
	for sc.Scan() {
		if p, ok := parsePSStatLine(sc.Text(), loc); ok && !seen[p.PID] {
			seen[p.PID] = true
			out = append(out, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ps output: %w", err)
	}
	nameOf, err := parsePSNames(names)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Name = nameOf[out[i].PID]
	}
	return out, nil
}

// parsePSNames maps PID to executable base name. An executable path that
// contains a newline can forge a "pid name" line, but only for a PID that
// also has its own genuine line, so any PID seen twice gets no name at all:
// an unnamed process is merely unattributed (observe-only), and a forged
// line can neither hide a process nor invent one.
func parsePSNames(r io.Reader) (map[int]string, error) {
	names := make(map[int]string)
	dup := make(map[int]bool)
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		pidStr, comm, ok := strings.Cut(strings.TrimLeft(sc.Text(), " \t"), " ")
		comm = strings.TrimSpace(comm)
		pid, err := strconv.Atoi(pidStr)
		if !ok || err != nil || comm == "" {
			continue
		}
		if _, exists := names[pid]; exists {
			dup[pid] = true
		}
		names[pid] = path.Base(comm)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ps names: %w", err)
	}
	for pid := range dup {
		delete(names, pid)
	}
	return names, nil
}

// parsePSStatLine parses one psStatColumns line: pid ppid uid rss time
// lstart (5 fields). Lines with any other number of fields are rejected.
func parsePSStatLine(line string, loc *time.Location) (domain.ProcessSample, bool) {
	fields := strings.Fields(line)
	if len(fields) != 10 {
		return domain.ProcessSample{}, false
	}
	pid, err1 := strconv.Atoi(fields[0])
	ppid, err2 := strconv.Atoi(fields[1])
	uid, err3 := strconv.ParseUint(fields[2], 10, 32)
	rssKiB, err4 := strconv.ParseUint(fields[3], 10, 64)
	cpu, err5 := parsePSTime(fields[4])
	start, err6 := time.ParseInLocation(psLstartLayout, strings.Join(fields[5:10], " "), loc)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil || pid < 0 || ppid < 0 {
		return domain.ProcessSample{}, false
	}
	return domain.ProcessSample{
		PID:     pid,
		PPID:    ppid,
		UID:     uint32(uid),
		Start:   start,
		CPUTime: cpu,
		RSS:     rssKiB * 1024, // macOS ps reports RSS in KiB
	}, true
}

// parsePSTime parses ps cumulative CPU time: [D-][[H:]M:]S[.ff]. macOS prints
// M:SS.ss with unbounded minutes (e.g. "2221:05.48"); other ps
// implementations print H:MM:SS or D-HH:MM:SS for long-running processes.
func parsePSTime(s string) (time.Duration, error) {
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		v, err := strconv.ParseUint(d, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: bad days", s)
		}
		days, s = int64(v), rest
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("ps time %q: too many fields", s)
	}
	secStr := parts[len(parts)-1]
	whole, frac, _ := strings.Cut(secStr, ".")
	sec, err := strconv.ParseUint(whole, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("ps time %q: bad seconds", s)
	}
	total := time.Duration(sec) * time.Second
	if frac != "" {
		if len(frac) > 9 {
			return 0, fmt.Errorf("ps time %q: bad fraction", s)
		}
		f, err := strconv.ParseUint(frac, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: bad fraction", s)
		}
		for range 9 - len(frac) {
			f *= 10
		}
		total += time.Duration(f)
	}
	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.ParseUint(parts[i], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: bad field", s)
		}
		total += time.Duration(v) * unit
		unit = time.Hour
	}
	return total + time.Duration(days)*24*time.Hour, nil
}
