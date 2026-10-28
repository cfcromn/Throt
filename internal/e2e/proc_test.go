package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// procState returns pid's state letter, or 0 if the process is gone: from
// /proc/PID/stat on Linux, from ps elsewhere (macOS). The letters follow
// Linux: R running, S sleeping, D uninterruptible, T stopped, Z zombie.
func procState(pid int) byte {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return 0
		}
		i := bytes.LastIndexByte(b, ')')
		if i < 0 || i+2 >= len(b) {
			return 0
		}
		return b[i+2]
	}
	// ps exits 1, printing nothing, for a PID that is gone.
	out, _ := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	return psState(out)
}

// psState parses the output of ps -o state= -p PID: one token of a state
// letter and flag characters (e.g. "Ss+", "T", "U<"). macOS letters map to
// the Linux ones: I (idle, asleep for over 20s) is S, and U (uninterruptible
// wait) is D. Anything but exactly one token yields 0.
func psState(out []byte) byte {
	f := strings.Fields(string(out))
	if len(f) != 1 || f[0][0] < 'A' || f[0][0] > 'Z' {
		return 0
	}
	switch c := f[0][0]; c {
	case 'I':
		return 'S'
	case 'U':
		return 'D'
	default:
		return c
	}
}

// comm returns the command name of pid, or "" if it is gone: from
// /proc/PID/comm on Linux, from ps elsewhere (macOS).
func comm(pid int) string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	out, _ := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	return psComm(out)
}

// psComm parses the output of ps -o comm= -p PID, which on macOS is the
// executable's path, to its base name.
func psComm(out []byte) string {
	c := strings.TrimSpace(string(out))
	if c == "" {
		return ""
	}
	return filepath.Base(c)
}

func TestPSState(t *testing.T) {
	for out, want := range map[string]byte{
		"":              0,
		"\n":            0,
		"Ss+\n":         'S',
		"S\n":           'S',
		"R+\n":          'R',
		"T\n":           'T',
		"T+\n":          'T',
		"I\n":           'S',
		"Is\n":          'S',
		"U<\n":          'D',
		"Z\n":           'Z',
		"  Ss  \n":      'S',
		"s\n":           0,
		"+\n":           0,
		"Ss\nR\n":       0,
		"STAT\nSs\n":    0,
		"ps: bad pid\n": 0,
	} {
		if got := psState([]byte(out)); got != want {
			t.Errorf("psState(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestPSComm(t *testing.T) {
	for out, want := range map[string]string{
		"":                                      "",
		"\n":                                    "",
		"/private/tmp/throte2e123/bin/make\n":   "make",
		"/private/tmp/throte2e123/bin/throt\n":  "throt",
		"make\n":                                "make",
		"/Applications/My App.app/Contents/x\n": "x",
		"-zsh\n":                                "-zsh",
	} {
		if got := psComm([]byte(out)); got != want {
			t.Errorf("psComm(%q) = %q, want %q", out, got, want)
		}
	}
}
