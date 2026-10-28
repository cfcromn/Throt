package update

import "fmt"

// Choice is the release Select picked and how it relates to the running
// version.
type Choice struct {
	Release Release
	// Cmp is -1 when Release is older than the current version (a
	// downgrade), 0 when it is the same, 1 when it is newer.
	Cmp int
	// Explicit is set when the release was asked for with --version.
	Explicit bool
}

// UpToDate reports whether there is nothing to install: the chosen release
// is the running version, or, unless it was asked for by version, older
// (Throt never downgrades unasked).
func (c Choice) UpToDate() bool { return c.Cmp == 0 || (c.Cmp < 0 && !c.Explicit) }

// Select picks the release to install from rs, ignoring drafts and tags
// that are not semver. With want set it is the release tagged want (with
// or without a leading "v"), older or not, so it also rolls back. Otherwise
// it is the newest release, prereleases included when pre is set or when
// current is itself a prerelease.
func Select(rs []Release, current, want string, pre bool) (Choice, error) {
	cur, ok := parseVersion(current)
	if !ok {
		return Choice{}, fmt.Errorf("the running version %q is not a release version", current)
	}
	pre = pre || len(cur.pre) > 0
	if want != "" {
		if _, ok := parseVersion(want); !ok {
			return Choice{}, fmt.Errorf("--version %q is not a version like v1.2.3", want)
		}
		for _, r := range rs {
			if r.Draft || !sameVersion(r.TagName, want) {
				continue
			}
			v, _ := parseVersion(r.TagName)
			return Choice{Release: r, Cmp: compare(v, cur), Explicit: true}, nil
		}
		return Choice{}, fmt.Errorf("no published release %s", want)
	}
	var best *Release
	var bestV semver
	for i, r := range rs {
		v, ok := parseVersion(r.TagName)
		if r.Draft || !ok || (!pre && (r.Prerelease || len(v.pre) > 0)) {
			continue
		}
		if best == nil || compare(v, bestV) > 0 {
			best, bestV = &rs[i], v
		}
	}
	if best == nil {
		if pre {
			return Choice{}, fmt.Errorf("no published releases found")
		}
		return Choice{}, fmt.Errorf("no stable release found; use --pre to include prereleases")
	}
	return Choice{Release: *best, Cmp: compare(bestV, cur)}, nil
}
