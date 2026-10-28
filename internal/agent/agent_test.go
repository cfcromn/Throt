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
		{"amp", "amp"},
		{"copilot", "copilot"},
		// Generic runtimes are ambiguous and stay unknown, whatever script
		// they run: argv is never read, so "node gemini.js" and
		// "python3 -m aider" are not guessed.
		{"node", ""},
		{"nodejs", ""},
		{"python3", ""},
		{"python3.12", ""},
		{"Python", ""},
		{"bun", ""},
		{"deno", ""},
		// An editor with gemini.md open is the editor.
		{"vim", ""},
		{"code", ""},
		{"gemini.md", ""},
		// Look-alikes of built-in names.
		{"aider_something_else", ""},
		{"aider-chat", ""},
		{"github-copilot", ""},
		{"copilot-language-server", ""},
		{"ampd", ""},
		{"gemini-cli", ""},
		{"codex.js", ""},
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
		{"ninja", domain.ClassBuild},
		{"cmake", domain.ClassBuild},
		{"swiftc", domain.ClassBuild},
		{"kotlinc", domain.ClassBuild},
		{"dotnet", domain.ClassBuild},
		{"msbuild", domain.ClassBuild},
		{"zig", domain.ClassBuild},
		{"ghc", domain.ClassBuild},
		{"scalac", domain.ClassBuild},
		{"sbt", domain.ClassBuild},
		{"bazelisk", domain.ClassBuild},
		{"buck2", domain.ClassBuild},
		{"turbo", domain.ClassBuild},
		{"nx", domain.ClassBuild},
		{"rollup", domain.ClassBuild},
		{"webpack-cli", domain.ClassBuild},
		{"next", domain.ClassBuild},
		{"nuxt", domain.ClassBuild},
		{"rspack", domain.ClassBuild},
		{"pytest", domain.ClassTest},
		{"jest", domain.ClassTest},
		{"vitest", domain.ClassTest},
		{"rspec", domain.ClassTest},
		{"mocha", domain.ClassTest},
		{"playwright", domain.ClassTest},
		{"cypress", domain.ClassTest},
		{"phpunit", domain.ClassTest},
		{"ctest", domain.ClassTest},
		{"docker", domain.ClassUnknown}, // CLI client; the work runs in buildkitd/the VM
		{"buildkitd", domain.ClassContainerBuild},
		{"podman", domain.ClassContainerBuild},
		{"buildctl", domain.ClassContainerBuild},
		{"buildah", domain.ClassContainerBuild},
		{"kaniko", domain.ClassContainerBuild},
		{"node", domain.ClassUnknown},
		{"python", domain.ClassUnknown},
		{"python3", domain.ClassUnknown},
		{"ruby", domain.ClassUnknown},
		{"php", domain.ClassUnknown},
		{"deno", domain.ClassUnknown},
		// Exact names only: no prefixes, suffixes or case folding.
		{"next-server", domain.ClassUnknown},
		{"ninja-build", domain.ClassUnknown},
		{"CMake", domain.ClassUnknown},
		{"turbo.exe", domain.ClassUnknown},
		{"buildah2", domain.ClassUnknown},
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
