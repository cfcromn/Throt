package update

import (
	"strconv"
	"strings"
)

// semver is a parsed semantic version. Build metadata is dropped: it does
// not take part in ordering.
type semver struct {
	major, minor, patch int
	pre                 []string
}

// parseVersion parses "1.2.3", "v1.2.3-rc.1" or "v1.2.3+meta".
func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, ok := numeric(p)
		if !ok {
			return v, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	if hasPre {
		v.pre = strings.Split(pre, ".")
		for _, id := range v.pre {
			if id == "" {
				return v, false
			}
			for _, r := range id {
				if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
					return v, false
				}
			}
		}
	}
	return v, true
}

// numeric parses a semver numeric identifier: digits, no leading zero.
func numeric(s string) (int, bool) {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// compare orders a and b by semver precedence: -1, 0 or 1.
func compare(a, b semver) int {
	for _, d := range [3][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if d[0] != d[1] {
			return cmpInt(d[0], d[1])
		}
	}
	// A release ranks above any of its prereleases.
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, xok := numeric(x)
		yn, yok := numeric(y)
		switch {
		case xok && yok:
			if xn != yn {
				return cmpInt(xn, yn)
			}
		case xok:
			return -1
		case yok:
			return 1
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(a.pre), len(b.pre))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// isPrerelease reports whether s parses as a semver prerelease.
func isPrerelease(s string) bool {
	v, ok := parseVersion(s)
	return ok && len(v.pre) > 0
}

// sameVersion reports whether a and b name the same version, ignoring a
// leading "v" (release tags have it; ldflags-stamped versions do not).
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
