package platform

// Parser for the macOS kern.procargs2 sysctl. It is a pure function and
// carries no build constraint so it is tested and fuzzed on every OS.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
)

// procargsAlign is the alignment of argv[0] after the executable path, the
// pointer size. Measured on macOS 26 (arm64), all 985 readable processes
// had exactly this padding.
const procargsAlign = 8

// procargsComm returns argv[0] from a kern.procargs2 value, reduced the way
// a ps names line is: cut at the first newline and trimmed. That is the
// string macOS ps prints for comm (both read the same kernel buffer), so
// psName gives the same name either way. The layout is a native-endian
// int32 argc, the executable path, NUL padding so that argv[0] starts at a
// multiple of procargsAlign bytes after argc, then argc NUL-terminated
// arguments and the environment.
//
// An empty argv[0] is the one case that differs from ps. ps skips every
// NUL after the path and prints the next string, argv[1] or the first
// environment variable, and an environment value must never become a
// process name, so such a process is named by its executable path instead.
// Padding shorter than the alignment, or an empty result, is an error, so
// the caller asks ps instead.
func procargsComm(b []byte) (string, error) {
	if len(b) < 4 {
		return "", errors.New("procargs: short buffer")
	}
	if int32(binary.NativeEndian.Uint32(b)) < 1 {
		return "", errors.New("procargs: no arguments")
	}
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", errors.New("procargs: unterminated executable path")
	}
	name := rest[:end]
	argv := (end + procargsAlign) &^ (procargsAlign - 1)
	switch at := len(rest) - len(bytes.TrimLeft(rest[end:], "\x00")); {
	case at < argv:
		return "", errors.New("procargs: unexpected padding")
	case at == argv && at < len(rest):
		rest = rest[argv:]
		end = bytes.IndexByte(rest, 0)
		if end < 0 {
			return "", errors.New("procargs: unterminated argv[0]")
		}
		name = rest[:end]
	}
	// Otherwise argv[0] is empty and name is the executable path. A
	// newline in the name would end ps's line there; what follows it is
	// the text a forged ps line would carry, so it is dropped here too.
	comm, _, _ := strings.Cut(string(name), "\n")
	comm = strings.TrimSpace(comm)
	if comm == "" {
		return "", errors.New("procargs: empty argv[0]")
	}
	return comm, nil
}
