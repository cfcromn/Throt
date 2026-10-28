package platform

import (
	"encoding/binary"
	"testing"
)

// procargs builds a kern.procargs2 value: argc, the executable path, NUL
// padding, then the arguments.
func procargs(argc int32, exe string, pad int, args ...string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	b = append(b, make([]byte, 1+pad)...)
	for _, a := range args {
		b = append(b, a...)
		b = append(b, 0)
	}
	return b
}

func TestProcargsComm(t *testing.T) {
	unterminated := procargs(1, "/x", 0, "abc")
	unterminated = unterminated[:len(unterminated)-1]
	for _, tc := range []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"path", procargs(2, "/usr/local/go/bin/go", 3, "/usr/local/go/bin/go", "build"), "/usr/local/go/bin/go", true},
		{"bare", procargs(1, "/opt/homebrew/bin/node", 0, "node"), "node", true},
		{"title", procargs(1, "/opt/homebrew/bin/node", 5, "npm run build"), "npm run build", true},
		{"spaces", procargs(1, "/x", 0, "  claude  "), "claude", true},
		// A newline cannot smuggle a second name in, as on a ps line.
		{"newline", procargs(1, "/x", 0, "/tmp/evil\n    1 launchd"), "/tmp/evil", true},
		{"empty argv0", procargs(1, "/x", 0, ""), "", false},
		{"newline first", procargs(1, "/x", 0, "\nclaude"), "", false},
		{"no args", procargs(0, "/x", 0), "", false},
		{"negative argc", procargs(-1, "/x", 0, "a"), "", false},
		{"unterminated argv0", unterminated, "", false},
		{"unterminated path", binary.NativeEndian.AppendUint32(nil, 1), "", false},
		{"short", []byte{1, 0}, "", false},
		{"nil", nil, "", false},
	} {
		got, err := procargsComm(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%s: procargsComm = %q, %v; want %q, ok %v", tc.name, got, err, tc.want, tc.ok)
		}
	}
}
