package app

import "runtime/debug"

// Version is the release version, set at build time with -ldflags "-X".
// A build without it (go install) reports its build info instead.
var Version = "dev"

func init() {
	bi, ok := debug.ReadBuildInfo()
	Version = buildVersion(Version, bi, ok)
}

// buildVersion returns v unless it is "dev", the unstamped default. Then it
// returns the module version from bi, e.g. v0.8.0 for go install
// github.com/cfcromn/Throt/cmd/throtd@v0.8.0, or for a build from a
// checkout, "dev-" and the short VCS revision, with "-dirty" when the tree
// had uncommitted changes.
func buildVersion(v string, bi *debug.BuildInfo, ok bool) string {
	if v != "dev" || !ok || bi == nil {
		return v
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision":
			rev = s.Value[:min(len(s.Value), 7)]
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "-dirty"
		}
	}
	if rev == "" {
		return v
	}
	return v + "-" + rev + dirty
}
