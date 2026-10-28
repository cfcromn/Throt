package app

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	vcs := func(rev, modified string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified},
		}}
	}
	for _, tc := range []struct {
		name string
		v    string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"ldflags win", "v0.8.0", &debug.BuildInfo{Main: debug.Module{Version: "v0.7.0"}}, true, "v0.8.0"},
		{"go install @version", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.8.0"}}, true, "v0.8.0"},
		{"clean checkout", "dev", vcs("abc1234def5678", "false"), true, "dev-abc1234"},
		{"dirty checkout", "dev", vcs("abc1234def5678", "true"), true, "dev-abc1234-dirty"},
		{"short revision", "dev", vcs("abc", "true"), true, "dev-abc-dirty"},
		{"no vcs info", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"no module version", "dev", &debug.BuildInfo{}, true, "dev"},
		{"no build info", "dev", nil, false, "dev"},
	} {
		if got := buildVersion(tc.v, tc.bi, tc.ok); got != tc.want {
			t.Errorf("%s: buildVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}
