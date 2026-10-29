package platform

// Parser for the lines macOS `iostat -n 0 -w N` prints. It is a pure
// function and carries no build constraint so it is tested and fuzzed on
// every OS.

import (
	"fmt"
	"strconv"
	"strings"
)

// iostatSlack is how far us+sy+id may stray from 100: each is rounded to
// a whole percent on its own.
const iostatSlack = 3

// parseIostatLine parses one line of `iostat -n 0` output:
//
//	     cpu    load average
//	us sy id   1m   5m   15m
//	13  9 77  23.58 14.72 9.95
//
// It returns header=true for the two header lines, and for a data line the
// busy fraction (100-id)/100. Anything else is an error: the line comes
// from another program and is not trusted.
func parseIostatLine(line string) (busy float64, header bool, err error) {
	f := strings.Fields(line)
	switch strings.Join(f, " ") {
	case "cpu load average", "us sy id 1m 5m 15m":
		return 0, true, nil
	}
	if len(f) != 6 {
		return 0, false, fmt.Errorf("iostat: %d fields, want 6", len(f))
	}
	var pct [3]int
	for i := range pct {
		v, ok := iostatPercent(f[i])
		if !ok {
			return 0, false, fmt.Errorf("iostat: bad percentage %q", f[i])
		}
		pct[i] = v
	}
	if sum := pct[0] + pct[1] + pct[2]; sum < 100-iostatSlack || sum > 100+iostatSlack {
		return 0, false, fmt.Errorf("iostat: us+sy+id = %d", sum)
	}
	for _, s := range f[3:] {
		if !iostatLoad(s) {
			return 0, false, fmt.Errorf("iostat: bad load average %q", s)
		}
	}
	return float64(100-pct[2]) / 100, false, nil
}

// iostatPercent accepts one to three decimal digits worth at most 100.
func iostatPercent(s string) (int, bool) {
	if len(s) == 0 || len(s) > 3 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil || v > 100 {
		return 0, false
	}
	return v, true
}

// iostatLoad accepts a plain non-negative decimal such as 23.58: digits
// with at most one dot, never inf, exponents or signs.
func iostatLoad(s string) bool {
	if len(s) == 0 || len(s) > 12 || strings.Trim(s, "0123456789.") != "" || strings.Count(s, ".") > 1 || s == "." {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}
