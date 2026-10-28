package platform

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// benchProcs is about the size of a busy developer Mac's process table.
const benchProcs = 1500

// syntheticPS returns stats and names passes listing n processes.
func syntheticPS(n int) (stats, names string) {
	var s, nm strings.Builder
	for pid := 1; pid <= n; pid++ {
		fmt.Fprintf(&s, "%5d %5d   501  20480 Ss    1:02.50 Mon Oct  5 09:00:00 2026\n", pid, max(pid/3, 1))
		fmt.Fprintf(&nm, "%5d /Applications/Some App.app/Contents/MacOS/helper-%d --type=renderer\n", pid, pid)
	}
	return s.String(), nm.String()
}

func BenchmarkParsePS(b *testing.B) {
	stats, names := syntheticPS(benchProcs)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parsePS(strings.NewReader(stats), strings.NewReader(names), time.UTC); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParsePSNames(b *testing.B) {
	_, names := syntheticPS(benchProcs)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parsePSNames(strings.NewReader(names)); err != nil {
			b.Fatal(err)
		}
	}
}
