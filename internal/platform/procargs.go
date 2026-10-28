package platform

// Parser for the macOS kern.procargs2 sysctl. It is a pure function and
// carries no build constraint so it is tested and fuzzed on every OS.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
)

// procargsComm returns argv[0] from a kern.procargs2 value, reduced the way
// a ps names line is: cut at the first newline and trimmed. That is the
// string macOS ps prints for comm (both read the same kernel buffer), so
// psName gives the same name either way. The layout is a native-endian
// int32 argc, the executable path, NUL padding, then argc NUL-terminated
// arguments. The executable path is not used: argv[0] is what ps shows.
// An empty result is an error, so the caller asks ps instead.
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
	rest = bytes.TrimLeft(rest[end:], "\x00")
	end = bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", errors.New("procargs: unterminated argv[0]")
	}
	// A newline in argv[0] would end ps's line there; what follows it is
	// the text a forged ps line would carry, so it is dropped here too.
	comm, _, _ := strings.Cut(string(rest[:end]), "\n")
	comm = strings.TrimSpace(comm)
	if comm == "" {
		return "", errors.New("procargs: empty argv[0]")
	}
	return comm, nil
}
