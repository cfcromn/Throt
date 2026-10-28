package process

import (
	"reflect"
	"testing"

	"github.com/cfcromn/Throt/internal/agent"
)

func TestSetAdapters(t *testing.T) {
	builtin := agent.DefaultAdapters()
	with := func(custom ...agent.Adapter) []agent.Adapter { return append(custom, builtin...) }
	table := []ps{
		{pid: 10, ppid: 1, name: "claude"},
		{pid: 11, ppid: 1, name: "claude"},
		{pid: 20, ppid: 1, name: "my-agent"},
		{pid: 21, ppid: 20, name: "go"},
	}
	tests := []struct {
		name   string
		before []agent.Adapter
		after  []agent.Adapter
		want   []Change // changes of the first Update after SetAdapters
		owner  string   // owner of the go workload after the change
	}{
		{
			name:   "unchanged adapters keep labels and report nothing",
			before: with(agent.NewNameAdapter("mine", "my-agent")),
			after:  with(agent.NewNameAdapter("mine", "my-agent")),
			owner:  "mine-1",
		},
		{
			name:   "newly matched process is discovered",
			before: builtin,
			after:  with(agent.NewNameAdapter("mine", "my-agent")),
			want:   []Change{ch(agentFound, 20, 0, "my-agent", "mine-1")},
			owner:  "mine-1",
		},
		{
			name:   "no longer matched process is lost",
			before: with(agent.NewNameAdapter("mine", "my-agent")),
			after:  builtin,
			want:   []Change{ch(agentLost, 20, 0, "my-agent", "mine-1")},
			owner:  "",
		},
		{
			name:   "renamed adapter relabels its agent only",
			before: with(agent.NewNameAdapter("mine", "my-agent")),
			after:  with(agent.NewNameAdapter("ours", "my-agent")),
			want:   []Change{ch(agentLost, 20, 0, "my-agent", "mine-1"), ch(agentFound, 20, 0, "my-agent", "ours-1")},
			owner:  "ours-1",
		},
		{
			name:   "custom adapter overriding a built-in name keeps labels",
			before: builtin,
			after:  with(agent.NewNameAdapter("claude", "claude", "my-agent")),
			want:   []Change{ch(agentFound, 20, 0, "my-agent", "claude-3")},
			owner:  "claude-3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGraph(tt.before)
			g.Update(epoch, samples(table))
			before := g.Snapshot()
			g.SetAdapters(tt.after)
			if got := g.Snapshot(); !reflect.DeepEqual(got, before) {
				t.Fatalf("SetAdapters changed the snapshot before Update")
			}
			got := run(g, []step{{1, table}})[0]
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("changes:\n got  %+v\n want %+v", got, tt.want)
			}
			labels := map[int]string{}
			for _, a := range g.Snapshot().Agents {
				labels[a.Key.PID] = a.Agent
			}
			if labels[10] != "claude-1" || labels[11] != "claude-2" {
				t.Errorf("claude labels = %q, %q; want claude-1, claude-2", labels[10], labels[11])
			}
			heavy := g.Snapshot().Heavy
			if len(heavy) != 1 || heavy[0].Owner != tt.owner {
				t.Errorf("heavy = %+v, want one owned by %q", heavy, tt.owner)
			}
		})
	}
}

func TestSetAdaptersDoesNotAlias(t *testing.T) {
	adapters := []agent.Adapter{agent.NewNameAdapter("mine", "my-agent")}
	g := NewGraph(nil)
	g.SetAdapters(adapters)
	adapters[0] = agent.NewNameAdapter("other", "my-agent")
	got := g.Update(epoch, samples([]ps{{pid: 20, ppid: 1, name: "my-agent"}}))
	if want := []Change{ch(agentFound, 20, 0, "my-agent", "mine-1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
}
