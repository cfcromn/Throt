package e2e

// The soak test's process inspection on macOS, which has no /proc. The
// checker polls every job ten times a second, so state, start time and
// name come in-process from sysctl kern.proc.pid rather than from a ps per
// job. throtd's own usage, sampled every 30s, comes from the tools: RSS
// from ps -o rss=, threads from ps -M, and file descriptors from lsof.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// p_stat values from XNU's sys/proc.h.
const (
	darwinSIDL   = 1
	darwinSRUN   = 2
	darwinSSLEEP = 3
	darwinSSTOP  = 4
	darwinSZOMB  = 5
)

// procStat returns the state letter (following Linux: R, S, T, Z) and start
// time (microseconds since the epoch) of pid, or ok false if it is gone.
func procStat(pid int) (state byte, start uint64, ok bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return 0, 0, false
	}
	tv := k.Proc.P_starttime
	return darwinState(k.Proc.P_stat), uint64(tv.Sec)*1e6 + uint64(tv.Usec), true
}

func darwinState(stat int8) byte {
	switch stat {
	case darwinSIDL, darwinSRUN:
		return 'R'
	case darwinSSLEEP:
		return 'S'
	case darwinSSTOP:
		return 'T'
	case darwinSZOMB:
		return 'Z'
	}
	return '?'
}

// soakComm returns the command name of pid (the kernel's p_comm, at most
// 16 bytes), or "" if it is gone.
func soakComm(pid int) string {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return ""
	}
	c := k.Proc.P_comm[:]
	if i := bytes.IndexByte(c, 0); i >= 0 {
		c = c[:i]
	}
	return string(c)
}

// daemonUsage reads pid's RSS (KiB) with ps -o rss=, its threads with
// ps -M, and its file descriptors with lsof; fds is -1 when lsof fails.
func daemonUsage(pid int) (rss int64, threads, fds int, ok bool) {
	p := strconv.Itoa(pid)
	out, err := exec.Command("/bin/ps", "-o", "rss=", "-p", p).Output()
	if err != nil {
		return 0, 0, 0, false
	}
	if rss, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err != nil {
		return 0, 0, 0, false
	}
	out, err = exec.Command("/bin/ps", "-M", "-p", p).Output()
	if err != nil {
		return 0, 0, 0, false
	}
	threads = psThreads(out)
	fds = -1
	if out, err := exec.Command("/usr/sbin/lsof", "-n", "-P", "-p", p, "-F", "f").Output(); err == nil {
		fds = lsofFDs(out)
	}
	return rss, threads, fds, true
}

// psThreads counts the threads in ps -M output: a header line, then one
// line per thread.
func psThreads(out []byte) int {
	n := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return max(0, n-1)
}

// lsofFDs counts the numbered file descriptors in lsof -F f output ("p" and
// "f" lines), leaving out cwd, txt and other non-descriptor entries, as
// /proc/PID/fd does.
func lsofFDs(out []byte) int {
	n := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if len(line) > 1 && line[0] == 'f' && line[1] >= '0' && line[1] <= '9' {
			n++
		}
	}
	return n
}

// stoppedProcesses lists the test user's processes in state T. A CI VM
// runs other users' services, none of them the test's, so they are left
// out.
func stoppedProcesses() []string {
	procs, _ := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	var out []string
	for _, k := range procs {
		if darwinState(k.Proc.P_stat) == 'T' {
			pid := int(k.Proc.P_pid)
			out = append(out, fmt.Sprintf("%d (%s)", pid, soakComm(pid)))
		}
	}
	return out
}

func TestPSThreads(t *testing.T) {
	for out, want := range map[string]int{
		"": 0,
		"USER PID TT %CPU STAT PRI STIME UTIME COMMAND\n": 0,
		"USER PID TT %CPU STAT PRI STIME UTIME COMMAND\n" +
			"me 4242 s000 0.0 S 31T 0:00.01 0:00.02 /tmp/bin/throtd\n" +
			"   4242      0.0 S 31T 0:00.00 0:00.00\n" +
			"   4242      0.0 S 31T 0:00.00 0:00.00\n\n": 3,
	} {
		if got := psThreads([]byte(out)); got != want {
			t.Errorf("psThreads(%q) = %d, want %d", out, got, want)
		}
	}
}

func TestLsofFDs(t *testing.T) {
	for out, want := range map[string]int{
		"":                          0,
		"p4242\n":                   0,
		"p4242\nfcwd\nftxt\nftxt\n": 0,
		"p4242\nfcwd\nftxt\nf0\nf1\nf2\nf10\nf123\n": 5,
	} {
		if got := lsofFDs([]byte(out)); got != want {
			t.Errorf("lsofFDs(%q) = %d, want %d", out, got, want)
		}
	}
}
