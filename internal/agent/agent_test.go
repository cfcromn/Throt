package agent

import (
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestDefaultAdapters(t *testing.T) {
	adapters := DefaultAdapters()
	match := func(name string) string {
		for _, a := range adapters {
			if a.Match(domain.ProcessSample{Name: name}) {
				return a.Name()
			}
		}
		return ""
	}
	tests := []struct {
		exe, want string
	}{
		{"claude", "claude"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		{"gemini", "gemini"},
		{"aider", "aider"},
		{"cursor-agent", "cursor-agent"},
		{"goose", "goose"},
		{"orca", "orca"},
		// Generic runtimes are ambiguous and stay unknown.
		{"node", ""},
		{"python3", ""},
		{"bun", ""},
		// Exact names only.
		{"Claude", ""},
		{"claude-helper", ""},
		{"cursor", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := match(tt.exe); got != tt.want {
			t.Errorf("match(%q) = %q, want %q", tt.exe, got, tt.want)
		}
	}
}

func TestDefaultAdaptersFreshSlice(t *testing.T) {
	a := DefaultAdapters()
	a[0] = nil
	if DefaultAdapters()[0] == nil {
		t.Fatal("DefaultAdapters shares its backing array")
	}
}

func TestNewNameAdapter(t *testing.T) {
	a := NewNameAdapter("custom", "foo", "foo-cli")
	if a.Name() != "custom" {
		t.Errorf("Name() = %q", a.Name())
	}
	for exe, want := range map[string]bool{"foo": true, "foo-cli": true, "custom": false, "node": false} {
		if got := a.Match(domain.ProcessSample{Name: exe}); got != want {
			t.Errorf("Match(%q) = %v, want %v", exe, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		want domain.WorkloadClass
	}{
		{"go", domain.ClassBuild},
		{"gradle", domain.ClassBuild},
		{"java", domain.ClassBuild},
		{"mvn", domain.ClassBuild},
		{"cargo", domain.ClassBuild},
		{"rustc", domain.ClassBuild},
		{"clang", domain.ClassBuild},
		{"gcc", domain.ClassBuild},
		{"cc1", domain.ClassBuild},
		{"cc1plus", domain.ClassBuild},
		{"ld", domain.ClassBuild},
		{"swift-frontend", domain.ClassBuild},
		{"xcodebuild", domain.ClassBuild},
		{"make", domain.ClassBuild},
		{"bazel", domain.ClassBuild},
		{"tsc", domain.ClassBuild},
		{"esbuild", domain.ClassBuild},
		{"webpack", domain.ClassBuild},
		{"vite", domain.ClassBuild},
		{"npm", domain.ClassBuild},
		{"pnpm", domain.ClassBuild},
		{"yarn", domain.ClassBuild},
		{"bun", domain.ClassBuild},
		{"pytest", domain.ClassTest},
		{"jest", domain.ClassTest},
		{"vitest", domain.ClassTest},
		{"docker", domain.ClassContainerBuild},
		{"buildkitd", domain.ClassContainerBuild},
		{"podman", domain.ClassContainerBuild},
		{"node", domain.ClassUnknown},
		{"python3", domain.ClassUnknown},
		{"claude", domain.ClassUnknown},
		{"zsh", domain.ClassUnknown},
		{"Go", domain.ClassUnknown},
		{"", domain.ClassUnknown},
	}
	for _, tt := range tests {
		if got := Classify(domain.ProcessSample{Name: tt.name}); got != tt.want {
			t.Errorf("Classify(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
