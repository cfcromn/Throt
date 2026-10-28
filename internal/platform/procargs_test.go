package platform

import (
	"encoding/binary"
	"testing"
)

// procargs builds a kern.procargs2 value as the kernel lays it out: argc,
// the executable path, NUL padding so argv[0] is aligned, then the
// arguments and the environment.
func procargs(argc int32, exe string, args ...string) []byte {
	b := binary.NativeEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	b = append(b, 0)
	for (len(b)-4)%procargsAlign != 0 {
		b = append(b, 0)
	}
	for _, a := range args {
		b = append(b, a...)
		b = append(b, 0)
	}
	return b
}

func TestProcargsComm(t *testing.T) {
	unterminated := procargs(1, "/x", "abc")
	unterminated = unterminated[:len(unterminated)-1]
	short := binary.NativeEndian.AppendUint32(nil, 1)
	short = append(short, "/bin/cat\x00\x00\x00ca\x00"...)
	for _, tc := range []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"path", procargs(2, "/usr/local/go/bin/go", "/usr/local/go/bin/go", "build"), "/usr/local/go/bin/go", true},
		{"bare", procargs(1, "/opt/homebrew/bin/node", "node"), "node", true},
		{"title", procargs(1, "/opt/homebrew/bin/node", "npm run build"), "npm run build", true},
		{"spaces", procargs(1, "/x", "  claude  "), "claude", true},
		{"aligned path", procargs(1, "/bin/cat", "ca"), "ca", true},
		// A newline cannot smuggle a second name in, as on a ps line.
		{"newline", procargs(1, "/x", "/tmp/evil\n    1 launchd"), "/tmp/evil", true},
		// An empty argv[0] names the executable, never the next string:
		// ps would print argv[1] or the first environment variable.
		{"empty argv0", procargs(1, "/bin/cat", "", "SECRET_TOKEN=hunter2"), "/bin/cat", true},
		{"empty argv0 then argv1", procargs(2, "/bin/sleep", "", "claude", "HOME=/Users/a"), "/bin/sleep", true},
		{"empty argv0 at the end", procargs(1, "/bin/cat", ""), "/bin/cat", true},
		{"empty argv0 and path", procargs(1, "", "", "SECRET=1"), "", false},
		{"short padding", short, "", false},
		{"newline first", procargs(1, "/x", "\nclaude"), "", false},
		{"no args", procargs(0, "/x"), "", false},
		{"negative argc", procargs(-1, "/x", "a"), "", false},
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
