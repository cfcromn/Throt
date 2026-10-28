// Package agent recognizes coding-agent processes and classifies workloads by
// executable name. All vendor knowledge lives here so core logic can treat
// agents through the Adapter interface alone. It is deterministic: it makes
// no OS calls and keeps no mutable state.
package agent

import "github.com/cfcromn/Throt/internal/domain"

// Adapter recognizes one coding agent's process. Name-based by default;
// vendor knowledge lives only here.
type Adapter interface {
	// Name is a short stable identifier such as "claude". It prefixes agent
	// instance labels, so it should be lowercase and contain no spaces.
	Name() string
	// Match reports whether p is this agent's own process (not a child).
	Match(p domain.ProcessSample) bool
}

// nameAdapter matches processes whose executable name is in a fixed set.
type nameAdapter struct {
	name  string
	names map[string]struct{}
}

// NewNameAdapter returns an adapter called name that matches processes whose
// executable base name exactly equals one of exes. Generic runtimes such as
// "node" or "python" should not be listed: they are ambiguous, and ambiguous
// processes stay unknown.
func NewNameAdapter(name string, exes ...string) Adapter {
	a := nameAdapter{name: name, names: make(map[string]struct{}, len(exes))}
	for _, e := range exes {
		a.names[e] = struct{}{}
	}
	return a
}

func (a nameAdapter) Name() string { return a.name }

func (a nameAdapter) Match(p domain.ProcessSample) bool {
	_, ok := a.names[p.Name]
	return ok
}

// defaultAgents lists the agents recognized out of the box. Each matches only
// its own executable name; many of them run on node, but node alone never
// matches. It is never mutated.
var defaultAgents = []string{
	"claude", "codex", "opencode", "gemini", "aider", "cursor-agent", "goose", "orca",
}

// DefaultAdapters returns a fresh slice of the built-in name adapters, one
// per agent, matching the executable name equal to the adapter name.
func DefaultAdapters() []Adapter {
	out := make([]Adapter, len(defaultAgents))
	for i, n := range defaultAgents {
		out[i] = NewNameAdapter(n, n)
	}
	return out
}

// classes maps executable base names to heavy workload classes. Anything not
// listed is unknown. It is never mutated.
var classes = map[string]domain.WorkloadClass{
	"go":             domain.ClassBuild,
	"gradle":         domain.ClassBuild,
	"java":           domain.ClassBuild,
	"mvn":            domain.ClassBuild,
	"cargo":          domain.ClassBuild,
	"rustc":          domain.ClassBuild,
	"clang":          domain.ClassBuild,
	"gcc":            domain.ClassBuild,
	"cc1":            domain.ClassBuild,
	"cc1plus":        domain.ClassBuild,
	"ld":             domain.ClassBuild,
	"swift-frontend": domain.ClassBuild,
	"xcodebuild":     domain.ClassBuild,
	"make":           domain.ClassBuild,
	"bazel":          domain.ClassBuild,
	"tsc":            domain.ClassBuild,
	"esbuild":        domain.ClassBuild,
	"webpack":        domain.ClassBuild,
	"vite":           domain.ClassBuild,
	"npm":            domain.ClassBuild,
	"pnpm":           domain.ClassBuild,
	"yarn":           domain.ClassBuild,
	"bun":            domain.ClassBuild,
	"ninja":          domain.ClassBuild,
	"cmake":          domain.ClassBuild,
	"swiftc":         domain.ClassBuild,
	"kotlinc":        domain.ClassBuild,
	"dotnet":         domain.ClassBuild,
	"msbuild":        domain.ClassBuild,
	"zig":            domain.ClassBuild,
	"ghc":            domain.ClassBuild,
	"scalac":         domain.ClassBuild,
	"sbt":            domain.ClassBuild,
	"bazelisk":       domain.ClassBuild,
	"buck2":          domain.ClassBuild,
	"turbo":          domain.ClassBuild,
	"nx":             domain.ClassBuild,
	"rollup":         domain.ClassBuild,
	"webpack-cli":    domain.ClassBuild,
	"rspack":         domain.ClassBuild,
	// next and nuxt also run long-lived dev servers (next dev, nuxt dev),
	// not only one-shot builds. Both still compile heavily, so they count as
	// builds; a dev server then holds a heavy slot for as long as it runs.
	"next":       domain.ClassBuild,
	"nuxt":       domain.ClassBuild,
	"pytest":     domain.ClassTest,
	"jest":       domain.ClassTest,
	"vitest":     domain.ClassTest,
	"rspec":      domain.ClassTest,
	"mocha":      domain.ClassTest,
	"playwright": domain.ClassTest,
	"cypress":    domain.ClassTest,
	"phpunit":    domain.ClassTest,
	"ctest":      domain.ClassTest,
	"buildkitd":  domain.ClassContainerBuild,
	"podman":     domain.ClassContainerBuild,
	"buildctl":   domain.ClassContainerBuild,
	"buildah":    domain.ClassContainerBuild,
	"kaniko":     domain.ClassContainerBuild,
}

// Classify labels p by its exact executable name. It never returns
// ClassAgent: agent recognition is the adapters' job. Unlisted names,
// including generic runtimes such as node and python, are ClassUnknown.
func Classify(p domain.ProcessSample) domain.WorkloadClass {
	if c, ok := classes[p.Name]; ok {
		return c
	}
	return domain.ClassUnknown
}
