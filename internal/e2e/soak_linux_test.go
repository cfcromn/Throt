package e2e

// The soak test's process inspection on Linux, from /proc.

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procStat returns the state letter and start time (clock ticks since
// boot) of pid, or ok false if it is gone.
func procStat(pid int) (state byte, start uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, 0, false
	}
	start, err = strconv.ParseUint(f[19], 10, 64) // field 22
	return f[0][0], start, err == nil
}

// soakComm returns the command name of pid, or "" if it is gone.
func soakComm(pid int) string { return comm(pid) }

// daemonUsage reads pid's RSS (KiB), thread and file descriptor counts
// from /proc/PID/status and /proc/PID/fd.
func daemonUsage(pid int) (rss int64, threads, fds int, ok bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, 0, false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "VmRSS":
			rss, _ = strconv.ParseInt(f[0], 10, 64)
		case "Threads":
			threads, _ = strconv.Atoi(f[0])
		}
	}
	ents, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	return rss, threads, len(ents), true
}

// stoppedProcesses lists every process in state T, which in the test's
// container or CI VM are all processes the test started.
func stoppedProcesses() []string {
	ents, _ := os.ReadDir("/proc")
	var out []string
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, _, ok := procStat(pid); ok && st == 'T' {
			out = append(out, fmt.Sprintf("%d (%s)", pid, comm(pid)))
		}
	}
	return out
}
