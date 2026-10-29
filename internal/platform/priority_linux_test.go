//go:build linux

package platform

import (
	"testing"
)

const validToken = tokenValid

// inBackground reports whether pid's main thread has nice >= 10 and the
// lowest best-effort I/O priority.
func inBackground(t *testing.T, pid int) bool {
	t.Helper()
	nice, err := getNice(pid)
	if err != nil {
		t.Fatal(err)
	}
	io, err := ioprioGet(pid)
	if err != nil {
		t.Fatal(err)
	}
	return nice >= backgroundNice && io == ioprioBackground
}

// restored reports whether pid's I/O priority is back. Its nice value may
// stay at 10: without RLIMIT_NICE an unprivileged user cannot lower it.
func restored(t *testing.T, pid int) bool {
	t.Helper()
	io, err := ioprioGet(pid)
	if err != nil {
		t.Fatal(err)
	}
	return io != ioprioBackground
}

func TestLowerableIO(t *testing.T) {
	for _, tc := range []struct {
		io   int
		want bool
	}{
		{0, true},
		{ioprioClassBE<<ioprioClassShift | 4, true},
		{ioprioBackground, false},
		{1<<ioprioClassShift | 0, false}, // real-time
		{3 << ioprioClassShift, false},   // idle
	} {
		if got := lowerableIO(tc.io); got != tc.want {
			t.Errorf("lowerableIO(%#x) = %v, want %v", tc.io, got, tc.want)
		}
	}
}

func TestForegroundRejectsInvalidToken(t *testing.T) {
	_, key, p := startSleep(t)
	if err := p.Foreground(t.Context(), key, 0, 0); err == nil {
		t.Fatal("Foreground accepted a zero token")
	}
}
