package platform

// Parser for macOS ps(1) output. It is a pure function over io.Reader and
// carries no build constraint so it is tested on every OS.

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// psStatColumns has no free-text column, so every record is unambiguous and
// the set of PIDs it lists is authoritative. state is one token of a state
// letter and flag characters (e.g. "Ss+"), never containing a space.
const psStatColumns = "pid=,ppid=,uid=,rss=,state=,time=,lstart="

// psNameColumns lists executable names. comm may contain spaces or even
// newlines, so this pass is parsed defensively. args/command are never
// requested: they can contain secrets.
const psNameColumns = "pid=,comm="

// psLstartLayout is lstart in the C locale, e.g. "Sun Oct 18 11:06:02 2026".
// Fields are re-joined with single spaces, so a padded day still matches.
const psLstartLayout = "Mon Jan 2 15:04:05 2006"

// parsePS combines a stats pass and a full names pass. lstart is
// interpreted in loc. Malformed lines are skipped; an error is returned only
// when a reader fails.
func parsePS(stats, names io.Reader, loc *time.Location) ([]domain.ProcessSample, error) {
	out, err := parsePSStats(stats, loc)
	if err != nil {
		return nil, err
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

// parsePSStats parses a psStatColumns pass, keeping the first line of each
// PID. lstart is interpreted in loc.
func parsePSStats(r io.Reader, loc *time.Location) ([]domain.ProcessSample, error) {
	var out []domain.ProcessSample
	seen := make(map[int]bool)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p, ok := parsePSStatLine(sc.Text(), loc); ok && !seen[p.PID] {
			seen[p.PID] = true
			out = append(out, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ps output: %w", err)
	}
	return out, nil
}

// parsePSNames maps PID to executable base name. An executable path that
// contains a newline can forge a "pid name" line, but only for a PID that
// also has its own genuine line, so any PID seen twice maps to "": an
// unnamed process is merely unattributed (observe-only), and a forged line
// can neither hide a process nor invent one. A PID without any line is
// absent from the map.
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
		names[pid] = psName(comm)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ps names: %w", err)
	}
	for pid := range dup {
		names[pid] = ""
	}
	return names, nil
}

// psNamesRefresh is how many samples a cached name is trusted before every
// name is read again. exec(2) changes a process's name but not its PID or
// start time, so only a fresh read notices it.
// ponytail: a periodic full refresh instead of exec detection; a process
// that execs after its first sample (a wrapper script that ends in exec
// make) keeps its old name for up to this many samples.
const psNamesRefresh = 15

// psNameCache remembers executable names by process identity, so a sample
// only asks ps for processes it has not named yet. A reused PID has a new
// start time and therefore a new key. It is not safe for concurrent use.
type psNameCache struct {
	names map[domain.ProcessKey]string
	// sinceFull counts samples since every name was last read.
	sinceFull int
}

// psNameLookup returns names for pids, as parsePSNames maps them. It may
// return names for other PIDs too; those are ignored.
type psNameLookup func(pids []int) (map[int]string, error)

// name sets Name on every process in procs, from the cache or from lookup
// for processes without a cached name, and prunes the cache to procs. A
// name from lookup counts only for a PID it was asked about, so a forged
// line for another PID is ignored. A process that lookup does not name
// (it exited between the passes) stays unnamed and is asked about again
// next sample.
func (c *psNameCache) name(procs []domain.ProcessSample, lookup psNameLookup) error {
	full := c.names == nil || c.sinceFull+1 >= psNamesRefresh
	asked := func(p domain.ProcessSample) bool {
		_, cached := c.names[p.Key()]
		return full || !cached
	}
	var ask []int
	for _, p := range procs {
		if asked(p) {
			ask = append(ask, p.PID)
		}
	}
	var got map[int]string
	if len(ask) > 0 {
		var err error
		if got, err = lookup(ask); err != nil {
			return err
		}
	}
	next := make(map[domain.ProcessKey]string, len(procs))
	for i := range procs {
		p := &procs[i]
		name, ok := c.names[p.Key()]
		if asked(*p) {
			name, ok = got[p.PID]
		}
		p.Name = name
		if ok {
			next[p.Key()] = name
		}
	}
	c.names = next
	if full {
		c.sinceFull = 0
	} else {
		c.sinceFull++
	}
	return nil
}

// psName reduces macOS ps comm, which is argv[0] and may hold a process
// title with arguments ("npm run build", "x --token=..."), to an executable
// name. A bare first word is the name; for a path, everything from the first
// " -" (flags) is dropped and the base name's first word is kept.
// ponytail: heuristic; paths with spaces in the final element keep only
// their first word, which is harmless for unknown processes.
func psName(comm string) string {
	first, _, _ := strings.Cut(comm, " ")
	if !strings.Contains(first, "/") {
		return first
	}
	exe, _, _ := strings.Cut(comm, " -")
	base, _, _ := strings.Cut(path.Base(exe), " ")
	return base
}

// parsePSStatLine parses one psStatColumns line: pid ppid uid rss state
// time lstart (11 fields, lstart being 5). Lines with any other number of
// fields are rejected.
func parsePSStatLine(line string, loc *time.Location) (domain.ProcessSample, bool) {
	fields := strings.Fields(line)
	if len(fields) != 11 {
		return domain.ProcessSample{}, false
	}
	pid, err1 := strconv.Atoi(fields[0])
	ppid, err2 := strconv.Atoi(fields[1])
	uid, err3 := strconv.ParseUint(fields[2], 10, 32)
	rssKiB, err4 := strconv.ParseUint(fields[3], 10, 64)
	state, ok := psState(fields[4])
	cpu, err5 := parsePSTime(fields[5])
	start, err6 := time.ParseInLocation(psLstartLayout, strings.Join(fields[6:11], " "), loc)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil || !ok || pid <= 0 || ppid < 0 {
		return domain.ProcessSample{}, false
	}
	return domain.ProcessSample{
		PID:     pid,
		PPID:    ppid,
		UID:     uint32(uid),
		Start:   start,
		CPUTime: cpu,
		RSS:     rssKiB * 1024, // macOS ps reports RSS in KiB
		State:   state,
	}, true
}

// psState maps a macOS ps state token, a state letter followed by flag
// characters (e.g. "Ss+", "T", "U<"), to a ProcessState. A token that does
// not start with an upper-case letter is malformed. macOS ps has no letter
// for traced processes; T is a stopped process.
func psState(tok string) (domain.ProcessState, bool) {
	if tok == "" || tok[0] < 'A' || tok[0] > 'Z' {
		return "", false
	}
	switch tok[0] {
	case 'R':
		return domain.StateRunning, true
	case 'S', 'I', 'U':
		return domain.StateSleeping, true
	case 'T':
		return domain.StateStopped, true
	case 'Z':
		return domain.StateZombie, true
	}
	return domain.StateOther, true
}

// psIdentityColumns reports one process's identity. Like psStatColumns it has
// no free-text column, and lstart is parsed exactly as the sampler parses it.
const psIdentityColumns = "uid=,lstart="

// parsePSIdentity parses the output of ps -o psIdentityColumns -p <pid>: one
// line of uid and lstart (6 fields). Anything else is an error.
func parsePSIdentity(out []byte, loc *time.Location) (time.Time, uint32, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		return time.Time{}, 0, fmt.Errorf("ps identity: %d lines, want 1", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 6 {
		return time.Time{}, 0, fmt.Errorf("ps identity: %d fields, want 6", len(fields))
	}
	uid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("ps identity uid: %w", err)
	}
	start, err := time.ParseInLocation(psLstartLayout, strings.Join(fields[1:6], " "), loc)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("ps identity lstart: %w", err)
	}
	return start, uint32(uid), nil
}

// psIdentifyColumns adds the parent PID to psIdentityColumns.
const psIdentifyColumns = "uid=,ppid=,lstart="

// parsePSIdentify parses the output of ps -o psIdentifyColumns -p <pid>: one
// line of uid, ppid and lstart (7 fields). Anything else is an error. The
// result's PID and Name are unset.
func parsePSIdentify(out []byte, loc *time.Location) (domain.ProcessSample, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		return domain.ProcessSample{}, fmt.Errorf("ps identity: %d lines, want 1", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 7 {
		return domain.ProcessSample{}, fmt.Errorf("ps identity: %d fields, want 7", len(fields))
	}
	uid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return domain.ProcessSample{}, fmt.Errorf("ps identity uid: %w", err)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return domain.ProcessSample{}, fmt.Errorf("ps identity ppid %q: invalid", fields[1])
	}
	start, err := time.ParseInLocation(psLstartLayout, strings.Join(fields[2:7], " "), loc)
	if err != nil {
		return domain.ProcessSample{}, fmt.Errorf("ps identity lstart: %w", err)
	}
	return domain.ProcessSample{PPID: ppid, UID: uint32(uid), Start: start}, nil
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
	// add rejects a field whose product or running sum overflows.
	var total time.Duration
	add := func(v uint64, unit time.Duration) error {
		d := time.Duration(v) * unit
		if d/unit != time.Duration(v) || total > math.MaxInt64-d {
			return fmt.Errorf("ps time %q: out of range", s)
		}
		total += d
		return nil
	}
	if err := add(sec, time.Second); err != nil {
		return 0, err
	}
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
		if err := add(f, 1); err != nil {
			return 0, err
		}
	}
	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.ParseUint(parts[i], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: bad field", s)
		}
		if err := add(v, unit); err != nil {
			return 0, err
		}
		unit = time.Hour
	}
	if err := add(uint64(days), 24*time.Hour); err != nil {
		return 0, err
	}
	return total, nil
}
