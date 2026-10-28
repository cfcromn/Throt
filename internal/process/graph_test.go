package process

import (
	"reflect"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/domain"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// ps describes one scripted process. start is seconds after epoch; cpu is
// cumulative CPU seconds; rss is MiB.
type ps struct {
	pid, ppid int
	name      string
	start     int
	cpu       float64
	rss       uint64
}

func (p ps) sample() domain.ProcessSample {
	return domain.ProcessSample{
		PID:     p.pid,
		PPID:    p.ppid,
		Start:   epoch.Add(time.Duration(p.start) * time.Second),
		Name:    p.name,
		CPUTime: time.Duration(p.cpu * float64(time.Second)),
		RSS:     p.rss << 20,
	}
}

func key(pid, start int) domain.ProcessKey {
	return domain.ProcessKey{PID: pid, Start: epoch.Add(time.Duration(start) * time.Second)}
}

// step is one scripted Update.
type step struct {
	sec   float64
	procs []ps
}

func samples(procs []ps) []domain.ProcessSample {
	out := make([]domain.ProcessSample, len(procs))
	for i, p := range procs {
		out[i] = p.sample()
	}
	return out
}

// run feeds steps into g and returns the changes of each step.
func run(g *Graph, steps []step) [][]Change {
	var out [][]Change
	for _, s := range steps {
		out = append(out, g.Update(epoch.Add(time.Duration(s.sec*float64(time.Second))), samples(s.procs)))
	}
	return out
}

func newGraph(opts ...Option) *Graph { return NewGraph(agent.DefaultAdapters(), opts...) }

func ch(kind domain.EventKind, pid, start int, name, label string) Change {
	return Change{Kind: kind, Process: key(pid, start), Name: name, Agent: label}
}

const (
	agentFound = domain.EventAgentDiscovered
	agentLost  = domain.EventAgentLost
	procFound  = domain.EventProcessDiscovered
	procLost   = domain.EventProcessLost
)

func TestChanges(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
		want  [][]Change
	}{
		{
			name: "discovery and loss",
			steps: []step{
				{0, []ps{{pid: 10, ppid: 1, name: "claude"}, {pid: 11, ppid: 10, name: "zsh"}, {pid: 12, ppid: 11, name: "go"}}},
				{1, []ps{{pid: 10, ppid: 1, name: "claude"}, {pid: 11, ppid: 10, name: "zsh"}}},
				{2, nil},
			},
			want: [][]Change{
				{ch(agentFound, 10, 0, "claude", "claude-1"), ch(procFound, 12, 0, "go", "")},
				{ch(procLost, 12, 0, "go", "")},
				{ch(agentLost, 10, 0, "claude", "claude-1")},
			},
		},
		{
			name: "unknown processes are silent",
			steps: []step{
				{0, []ps{{pid: 5, ppid: 1, name: "node"}, {pid: 6, ppid: 5, name: "sh"}}},
				{1, nil},
			},
			want: [][]Change{nil, nil},
		},
		{
			name: "pid reuse is lost then discovered",
			steps: []step{
				{0, []ps{{pid: 20, ppid: 1, name: "cargo", start: 0}}},
				{1, []ps{{pid: 20, ppid: 1, name: "cargo", start: 1}}},
			},
			want: [][]Change{
				{ch(procFound, 20, 0, "cargo", "")},
				{ch(procLost, 20, 0, "cargo", ""), ch(procFound, 20, 1, "cargo", "")},
			},
		},
		{
			name: "exec changing class is lost then discovered",
			steps: []step{
				{0, []ps{{pid: 30, ppid: 1, name: "sh"}}},
				{1, []ps{{pid: 30, ppid: 1, name: "go"}}},
				{2, []ps{{pid: 30, ppid: 1, name: "claude"}}},
				{3, []ps{{pid: 30, ppid: 1, name: "codex"}}},
			},
			want: [][]Change{
				nil,
				{ch(procFound, 30, 0, "go", "")},
				{ch(procLost, 30, 0, "go", ""), ch(agentFound, 30, 0, "claude", "claude-1")},
				{ch(agentLost, 30, 0, "claude", "claude-1"), ch(agentFound, 30, 0, "codex", "codex-1")},
			},
		},
		{
			name: "labels reuse the lowest free number only after loss",
			steps: []step{
				{0, []ps{{pid: 1, ppid: 0, name: "launchd"}, {pid: 40, ppid: 1, name: "claude"}, {pid: 41, ppid: 1, name: "claude"}, {pid: 42, ppid: 1, name: "codex"}}},
				{1, []ps{{pid: 41, ppid: 1, name: "claude"}, {pid: 42, ppid: 1, name: "codex"}}},
				{2, []ps{{pid: 41, ppid: 1, name: "claude"}, {pid: 42, ppid: 1, name: "codex"}, {pid: 50, ppid: 1, name: "claude", start: 2}, {pid: 51, ppid: 1, name: "claude", start: 2}}},
				// Loss and discovery in the same update: the freed number is reused.
				{3, []ps{{pid: 50, ppid: 1, name: "claude", start: 2}, {pid: 51, ppid: 1, name: "claude", start: 2}, {pid: 60, ppid: 1, name: "claude", start: 3}}},
			},
			want: [][]Change{
				{ch(agentFound, 40, 0, "claude", "claude-1"), ch(agentFound, 41, 0, "claude", "claude-2"), ch(agentFound, 42, 0, "codex", "codex-1")},
				{ch(agentLost, 40, 0, "claude", "claude-1")},
				{ch(agentFound, 50, 2, "claude", "claude-1"), ch(agentFound, 51, 2, "claude", "claude-3")},
				{ch(agentLost, 41, 0, "claude", "claude-2"), ch(agentLost, 42, 0, "codex", "codex-1"), ch(agentFound, 60, 3, "claude", "claude-2")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(newGraph(), tt.steps)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("changes:\n got  %+v\n want %+v", got, tt.want)
			}
		})
	}
}

func TestSnapshots(t *testing.T) {
	tests := []struct {
		name   string
		ncpu   int
		steps  []step
		agents []AgentView
		heavy  []WorkloadView
		total  float64
		hasCPU bool
	}{
		{
			name: "first update has no CPU",
			steps: []step{
				{0, []ps{{pid: 10, ppid: 1, name: "claude", cpu: 5, rss: 100}, {pid: 11, ppid: 10, name: "go", cpu: 9, rss: 50}}},
			},
			agents: []AgentView{{Key: key(10, 0), Agent: "claude-1", Name: "claude", Children: 1, RSS: 150 << 20}},
			heavy:  []WorkloadView{{Key: key(11, 0), Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10, RSS: 50 << 20}},
		},
		{
			name: "cpu deltas in cores and total fraction",
			ncpu: 4,
			steps: []step{
				{0, []ps{{pid: 10, ppid: 1, name: "claude", cpu: 1}, {pid: 11, ppid: 10, name: "go", cpu: 2}, {pid: 99, ppid: 1, name: "zsh", cpu: 0}}},
				// Over 2s: claude +1s (0.5 cores), go +4s (2 cores), zsh +1s.
				// A new process has no rate yet; its CPU time is not counted.
				{2, []ps{{pid: 10, ppid: 1, name: "claude", cpu: 2}, {pid: 11, ppid: 10, name: "go", cpu: 6}, {pid: 99, ppid: 1, name: "zsh", cpu: 1}, {pid: 12, ppid: 10, name: "rustc", cpu: 30}}},
			},
			agents: []AgentView{{Key: key(10, 0), Agent: "claude-1", Name: "claude", Children: 2, CPU: 2.5}},
			heavy: []WorkloadView{
				{Key: key(11, 0), Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10, CPU: 2},
				{Key: key(12, 0), Name: "rustc", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10},
			},
			total:  6.0 / (2 * 4),
			hasCPU: true,
		},
		{
			name: "negative deltas are ignored and total clamps to 1",
			steps: []step{
				{0, []ps{{pid: 11, ppid: 1, name: "go", cpu: 10}, {pid: 12, ppid: 1, name: "java", cpu: 0}}},
				{1, []ps{{pid: 11, ppid: 1, name: "go", cpu: 5}, {pid: 12, ppid: 1, name: "java", cpu: 3}}},
			},
			heavy: []WorkloadView{
				{Key: key(12, 0), Name: "java", Class: domain.ClassBuild, CPU: 3},
				{Key: key(11, 0), Name: "go", Class: domain.ClassBuild},
			},
			total:  1,
			hasCPU: true,
		},
		{
			name: "pid reuse does not carry cpu history",
			steps: []step{
				{0, []ps{{pid: 11, ppid: 1, name: "go", cpu: 1}}},
				{1, []ps{{pid: 11, ppid: 1, name: "go", start: 1, cpu: 50}}},
			},
			heavy:  []WorkloadView{{Key: key(11, 1), Name: "go", Class: domain.ClassBuild}},
			hasCPU: true,
		},
		{
			name: "non-advancing clock has no cpu",
			steps: []step{
				{5, []ps{{pid: 11, ppid: 1, name: "go", cpu: 1}}},
				{5, []ps{{pid: 11, ppid: 1, name: "go", cpu: 2}}},
			},
			heavy: []WorkloadView{{Key: key(11, 0), Name: "go", Class: domain.ClassBuild}},
		},
		{
			name: "nearest agent owns descendants",
			steps: []step{
				{0, []ps{
					{pid: 10, ppid: 1, name: "orca", cpu: 0, rss: 1},
					{pid: 11, ppid: 10, name: "zsh", rss: 1},
					{pid: 20, ppid: 11, name: "claude", rss: 2},
					{pid: 21, ppid: 20, name: "node", rss: 3},
					{pid: 22, ppid: 21, name: "go", rss: 4},
					{pid: 30, ppid: 10, name: "make", rss: 5},
				}},
			},
			agents: []AgentView{
				{Key: key(10, 0), Agent: "orca-1", Name: "orca", Children: 2, RSS: 7 << 20},
				{Key: key(20, 0), Agent: "claude-1", Name: "claude", Children: 2, RSS: 9 << 20},
			},
			heavy: []WorkloadView{
				{Key: key(22, 0), Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 20, RSS: 4 << 20},
				{Key: key(30, 0), Name: "make", Class: domain.ClassBuild, Owner: "orca-1", OwnerPID: 10, RSS: 5 << 20},
			},
		},
		{
			name: "node alone is not an agent",
			steps: []step{
				{0, []ps{{pid: 10, ppid: 1, name: "node"}, {pid: 11, ppid: 10, name: "esbuild", rss: 8}}},
			},
			heavy: []WorkloadView{{Key: key(11, 0), Name: "esbuild", Class: domain.ClassBuild, RSS: 8 << 20}},
		},
		{
			name: "unattributed heavy workload",
			steps: []step{
				{0, []ps{{pid: 1, ppid: 0, name: "launchd"}, {pid: 5, ppid: 1, name: "zsh"}, {pid: 6, ppid: 5, name: "buildkitd", rss: 2}}},
			},
			heavy: []WorkloadView{{Key: key(6, 0), Name: "buildkitd", Class: domain.ClassContainerBuild, RSS: 2 << 20}},
		},
		{
			name: "same-name heavy chains collapse into the topmost",
			steps: []step{
				{0, []ps{
					{pid: 10, ppid: 1, name: "claude"},
					{pid: 11, ppid: 10, name: "go", cpu: 0, rss: 1},
					{pid: 12, ppid: 11, name: "go", cpu: 0, rss: 2},
					{pid: 13, ppid: 12, name: "compile", cpu: 0, rss: 4},
					{pid: 14, ppid: 11, name: "sh"},
					{pid: 15, ppid: 14, name: "go", cpu: 0, rss: 8},
					{pid: 16, ppid: 11, name: "ld", rss: 16},
					// A nested agent's go is its own workload.
					{pid: 17, ppid: 11, name: "codex"},
					{pid: 18, ppid: 17, name: "go", rss: 32},
				}},
				{1, []ps{
					{pid: 10, ppid: 1, name: "claude"},
					{pid: 11, ppid: 10, name: "go", cpu: 1, rss: 1},
					{pid: 12, ppid: 11, name: "go", cpu: 2, rss: 2},
					{pid: 13, ppid: 12, name: "compile", cpu: 4, rss: 4},
					{pid: 14, ppid: 11, name: "sh"},
					{pid: 15, ppid: 14, name: "go", cpu: 8, rss: 8},
					{pid: 16, ppid: 11, name: "ld", rss: 16},
					{pid: 17, ppid: 11, name: "codex"},
					{pid: 18, ppid: 17, name: "go", rss: 32},
				}},
			},
			agents: []AgentView{
				{Key: key(10, 0), Agent: "claude-1", Name: "claude", Children: 6, CPU: 15, RSS: 31 << 20},
				{Key: key(17, 0), Agent: "codex-1", Name: "codex", Children: 1, RSS: 32 << 20},
			},
			heavy: []WorkloadView{
				{Key: key(11, 0), Name: "go", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10, CPU: 11, RSS: 11 << 20},
				{Key: key(16, 0), Name: "ld", Class: domain.ClassBuild, Owner: "claude-1", OwnerPID: 10, RSS: 16 << 20},
				{Key: key(18, 0), Name: "go", Class: domain.ClassBuild, Owner: "codex-1", OwnerPID: 17, RSS: 32 << 20},
			},
			total:  1,
			hasCPU: true,
		},
		{
			name: "cycles and missing parents are cut",
			steps: []step{
				{0, []ps{
					// 30 <-> 31 cycle: cut at the lowest PID, so 30 becomes a
					// root and stays unattributed even though 31 is an agent.
					{pid: 30, ppid: 31, name: "go"},
					{pid: 31, ppid: 30, name: "claude"},
					// 40 -> 41 -> 42 -> 40 cycle without an agent.
					{pid: 40, ppid: 42, name: "make"},
					{pid: 41, ppid: 40, name: "make"},
					{pid: 42, ppid: 41, name: "make"},
					// Self-parent, unknown parent, and PPID 0.
					{pid: 50, ppid: 50, name: "cargo"},
					{pid: 51, ppid: 999, name: "cargo"},
					{pid: 52, ppid: 0, name: "cargo"},
					// Parent PID reused: the "parent" started after the child.
					{pid: 60, ppid: 1, name: "claude", start: 5},
					{pid: 61, ppid: 60, name: "gcc", start: 1},
				}},
			},
			agents: []AgentView{
				{Key: key(31, 0), Agent: "claude-1", Name: "claude"},
				{Key: key(60, 5), Agent: "claude-2", Name: "claude"},
			},
			heavy: []WorkloadView{
				{Key: key(30, 0), Name: "go", Class: domain.ClassBuild},
				{Key: key(40, 0), Name: "make", Class: domain.ClassBuild},
				{Key: key(50, 0), Name: "cargo", Class: domain.ClassBuild},
				{Key: key(51, 0), Name: "cargo", Class: domain.ClassBuild},
				{Key: key(52, 0), Name: "cargo", Class: domain.ClassBuild},
				{Key: key(61, 1), Name: "gcc", Class: domain.ClassBuild},
			},
		},
		{
			name: "duplicate pid in one snapshot keeps the first",
			steps: []step{
				{0, []ps{{pid: 11, ppid: 1, name: "go", rss: 1}, {pid: 11, ppid: 1, name: "java", rss: 2}}},
			},
			heavy: []WorkloadView{{Key: key(11, 0), Name: "go", Class: domain.ClassBuild, RSS: 1 << 20}},
		},
		{
			name: "agents sort by cpu then pid",
			steps: []step{
				{0, []ps{{pid: 3, ppid: 1, name: "aider"}, {pid: 2, ppid: 1, name: "goose"}, {pid: 4, ppid: 1, name: "gemini"}}},
				{1, []ps{{pid: 3, ppid: 1, name: "aider"}, {pid: 2, ppid: 1, name: "goose"}, {pid: 4, ppid: 1, name: "gemini", cpu: 0.5}}},
			},
			agents: []AgentView{
				{Key: key(4, 0), Agent: "gemini-1", Name: "gemini", CPU: 0.5},
				{Key: key(2, 0), Agent: "goose-1", Name: "goose"},
				{Key: key(3, 0), Agent: "aider-1", Name: "aider"},
			},
			total:  0.5,
			hasCPU: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGraph(WithCPUCount(tt.ncpu))
			run(g, tt.steps)
			s := g.Snapshot()
			if !reflect.DeepEqual(s.Agents, tt.agents) {
				t.Errorf("agents:\n got  %+v\n want %+v", s.Agents, tt.agents)
			}
			if !reflect.DeepEqual(s.Heavy, tt.heavy) {
				t.Errorf("heavy:\n got  %+v\n want %+v", s.Heavy, tt.heavy)
			}
			if s.TotalCPU != tt.total || s.HasCPU != tt.hasCPU {
				t.Errorf("total = %v/%v, want %v/%v", s.TotalCPU, s.HasCPU, tt.total, tt.hasCPU)
			}
		})
	}
}

func TestEmptyGraph(t *testing.T) {
	s := newGraph().Snapshot()
	if s.Agents != nil || s.Heavy != nil || s.HasCPU || s.TotalCPU != 0 {
		t.Fatalf("empty snapshot = %+v", s)
	}
}

func TestSnapshotImmutable(t *testing.T) {
	g := newGraph()
	g.Update(epoch, samples([]ps{{pid: 10, ppid: 1, name: "claude"}, {pid: 11, ppid: 10, name: "go"}}))
	s := g.Snapshot()
	want := g.Snapshot()
	s.Agents[0].Agent = "mutated"
	s.Agents[0].CPU = 99
	s.Heavy[0].Owner = "mutated"
	s.Agents = append(s.Agents, AgentView{})
	if got := g.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot changed after mutation:\n got  %+v\n want %+v", got, want)
	}
}

func TestInputNotRetained(t *testing.T) {
	g := newGraph()
	in := samples([]ps{{pid: 10, ppid: 1, name: "claude"}})
	g.Update(epoch, in)
	in[0].Name = "codex"
	if got := g.Snapshot().Agents[0].Name; got != "claude" {
		t.Fatalf("graph observed caller mutation: %q", got)
	}
}

func TestLostProcessesAreRemoved(t *testing.T) {
	g := newGraph()
	for i := range 100 {
		g.Update(epoch.Add(time.Duration(i)*time.Second), samples([]ps{
			{pid: 10, ppid: 1, name: "claude"},
			{pid: 1000 + i, ppid: 10, name: "go", start: i},
		}))
	}
	if len(g.procs) != 2 {
		t.Fatalf("procs = %d, want 2", len(g.procs))
	}
	if n := len(g.used["claude"]); n != 1 {
		t.Fatalf("used labels = %d, want 1", n)
	}
}

func TestCustomAdaptersAndOrder(t *testing.T) {
	// Earlier adapters win; nil adapters are skipped; WithCPUCount ignores < 1.
	g := NewGraph([]agent.Adapter{nil, agent.NewNameAdapter("wrap", "node"), agent.NewNameAdapter("other", "node")}, WithCPUCount(0))
	if g.ncpu != 1 {
		t.Fatalf("ncpu = %d, want 1", g.ncpu)
	}
	got := g.Update(epoch, samples([]ps{{pid: 7, ppid: 1, name: "node"}}))
	want := []Change{ch(agentFound, 7, 0, "node", "wrap-1")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
}
