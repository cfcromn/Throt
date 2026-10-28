// Package process maintains the process graph: identity across snapshots,
// per-process CPU rates, attribution of processes to coding agents, and the
// heavy workloads they run. It is deterministic: it makes no OS calls,
// starts no goroutines, and takes time only from the caller.
package process

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/domain"
)

// Change is one lifecycle transition reported by Update. Only agents and
// heavy workloads produce changes; other processes come and go silently so
// short-lived helpers cannot flood the event log.
type Change struct {
	// Kind is EventAgentDiscovered/EventAgentLost for agents and
	// EventProcessDiscovered/EventProcessLost for heavy workloads.
	Kind    domain.EventKind
	Process domain.ProcessKey
	Name    string
	// Agent is the agent instance label (e.g. "claude-1") for agent events
	// and empty otherwise.
	Agent string
}

// AgentView summarizes one agent instance and the processes it owns.
type AgentView struct {
	Key domain.ProcessKey `json:"process"`
	// Agent is the instance label, e.g. "claude-1".
	Agent string `json:"agent"`
	// Name is the agent process's executable name.
	Name string `json:"name"`
	// Children counts owned descendants, excluding the agent itself and any
	// subtree owned by a nested agent.
	Children int `json:"children"`
	// CPU is cores used by the agent and its owned descendants. Processes
	// whose rate is not yet known contribute 0.
	CPU float64 `json:"cpu"`
	// RSS is bytes resident in the agent and its owned descendants.
	RSS uint64 `json:"rss"`
}

// WorkloadView summarizes one heavy workload, after collapsing heavy
// descendants (e.g. go -> compile, make -> cc1) into their topmost heavy
// ancestor with the same owner.
type WorkloadView struct {
	Key   domain.ProcessKey    `json:"process"`
	Name  string               `json:"name"`
	Class domain.WorkloadClass `json:"class"`
	// Owner is the owning agent instance label, or "" when unattributed.
	Owner string `json:"owner"`
	// OwnerPID is the owning agent's PID, or 0 when unattributed.
	OwnerPID int `json:"owner_pid"`
	// CPU (cores) and RSS (bytes) include collapsed descendants.
	CPU float64 `json:"cpu"`
	RSS uint64  `json:"rss"`
}

// MaxUnattributed bounds Snapshot.Unattributed.
const MaxUnattributed = 5

// NameUsage is the memory of the live processes with one name that no agent
// owns. It carries the executable name only: no arguments, no path.
type NameUsage struct {
	Name      string `json:"name"`
	Processes int    `json:"processes"`
	RSS       uint64 `json:"rss"`
}

// Snapshot is an immutable view of the graph after the latest Update. It
// shares no memory with the graph and is safe to hand to other goroutines.
type Snapshot struct {
	// Agents is sorted by CPU descending, then PID ascending.
	Agents []AgentView `json:"agents"`
	// Heavy is sorted by CPU descending, then PID ascending.
	Heavy []WorkloadView `json:"heavy"`
	// Stopped lists every stopped process (domain.StateStopped) by PID, for
	// FindOrphans. It is bounded by the process table.
	Stopped []StoppedView `json:"stopped,omitempty"`
	// Unattributed lists the names with the most resident memory among
	// processes no agent owns, at most MaxUnattributed, by RSS descending
	// then name; UnattributedRSS totals all such processes. Together with
	// the agents they show where the host's memory goes.
	Unattributed    []NameUsage `json:"unattributed,omitempty"`
	UnattributedRSS uint64      `json:"unattributed_rss,omitempty"`
	// TotalCPU is the busy fraction of all CPUs across observed processes
	// since the previous Update, 0..1. Valid only when HasCPU is true.
	TotalCPU float64 `json:"total_cpu"`
	HasCPU   bool    `json:"has_cpu"`
}

// Option configures a Graph.
type Option func(*Graph)

// WithCPUCount sets the logical CPU count used to normalize TotalCPU.
// Values < 1 are ignored. The default is 1; callers pass the host's count.
func WithCPUCount(n int) Option {
	return func(g *Graph) {
		if n >= 1 {
			g.ncpu = n
		}
	}
}

// WithAgentUID only lets processes owned by uid be recognized as agents.
// Without it, any user's process may match.
func WithAgentUID(uid uint32) Option {
	return func(g *Graph) { g.agentUID, g.hasAgentUID = uid, true }
}

// track is what kind of lifecycle a process reports.
type track int

const (
	trackNone track = iota
	trackAgent
	trackHeavy
)

// proc is the graph's record of one live process.
type proc struct {
	s       domain.ProcessSample
	cpu     float64 // cores; 0 when !hasCPU
	hasCPU  bool
	adapter string // matching adapter's name, "" if not an agent
	class   domain.WorkloadClass
	track   track
	label   string // agent instance label, agents only
	parent  *proc  // nil when the parent is missing or ambiguous
	owner   *proc  // nearest agent at or above this process, or nil
	// stoppedSince is when an Update first saw this process stopped in the
	// current run of stopped samples; zero while it is not stopped.
	stoppedSince time.Time
	// activeAt is when a launcher's subtree was last active (see
	// launcherHeavy); zero if never, and for other processes.
	activeAt time.Time
}

// A launcher (agent.Launcher: npm, pnpm, yarn, bun) is heavy only while its
// subtree, itself included, uses at least launcherCPU cores or
// launcherRSS bytes, and for launcherHold after that, so a build pausing
// for I/O is not dropped and rediscovered. A stopped launcher that was heavy
// stays heavy: a queued build uses no CPU. An idle one, such as an MCP
// server under npm exec, never takes a heavy slot.
const (
	launcherCPU  = 0.5
	launcherRSS  = 512 << 20
	launcherHold = time.Minute
)

// Graph tracks processes across full snapshots. It is not safe for
// concurrent use; the owner serializes calls and shares Snapshots instead.
type Graph struct {
	adapters []agent.Adapter
	ncpu     int
	// agentUID, when set, restricts agent matching to processes of that
	// user, so another account's "claude" is never treated as ours.
	agentUID    uint32
	hasAgentUID bool

	procs   map[domain.ProcessKey]*proc
	last    time.Time
	hasLast bool
	// used holds the instance numbers taken per adapter name. Entries are
	// freed when their agent is lost, so it is bounded by live agents.
	used map[string]map[int]struct{}
	snap Snapshot
}

// NewGraph returns an empty graph that recognizes agents with adapters.
// Earlier adapters win when several match the same process.
func NewGraph(adapters []agent.Adapter, opts ...Option) *Graph {
	g := &Graph{
		adapters: slices.Clone(adapters),
		ncpu:     1,
		procs:    map[domain.ProcessKey]*proc{},
		used:     map[string]map[int]struct{}{},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// SetAdapters replaces the agent adapters; earlier adapters still win. It
// takes effect on the next Update, which re-matches every process: an agent
// still matched by an adapter of the same name keeps its instance label, and
// processes that become or stop being agents are reported as discovered or
// lost like any other transition.
func (g *Graph) SetAdapters(adapters []agent.Adapter) {
	g.adapters = slices.Clone(adapters)
}

// Update replaces the process table with a fresh full snapshot taken at now
// and returns lifecycle changes.
//
// A process is identified by PID and Start, so a reused PID is a new process
// with no CPU history. If procs lists a PID more than once, the first entry
// wins. CPU rates are computed against the previous Update; they are unknown
// on a process's first snapshot, when now does not advance, or when CPU time
// went backwards.
func (g *Graph) Update(now time.Time, procs []domain.ProcessSample) []Change {
	dt := now.Sub(g.last).Seconds()
	rates := g.hasLast && dt > 0
	g.last, g.hasLast = now, true

	next := make(map[domain.ProcessKey]*proc, len(procs))
	byPID := make(map[int]*proc, len(procs))
	order := make([]*proc, 0, len(procs))
	// One allocation for the whole table rather than one per process; the
	// previous table's slab is dropped with g.procs below.
	slab := make([]proc, len(procs))
	var busy float64 // CPU seconds across all processes since the last Update
	for _, s := range procs {
		if _, dup := byPID[s.PID]; dup {
			continue
		}
		if s.State == domain.StateZombie {
			// It has exited and only waits for its parent to reap it, so it
			// holds no slot and owns nothing: treat it as gone.
			continue
		}
		p := &slab[len(order)]
		p.s = s
		old, ok := g.procs[s.Key()]
		if ok && rates && s.CPUTime >= old.s.CPUTime {
			d := (s.CPUTime - old.s.CPUTime).Seconds()
			p.cpu, p.hasCPU = d/dt, true
			busy += d
		}
		if ok {
			p.activeAt = old.activeAt
		}
		if s.State == domain.StateStopped {
			p.stoppedSince = now
			if ok && !old.stoppedSince.IsZero() {
				p.stoppedSince = old.stoppedSince
			}
		}
		next[s.Key()], byPID[s.PID] = p, p
		order = append(order, p)
	}
	slices.SortFunc(order, func(a, b *proc) int { return cmp.Compare(a.s.PID, b.s.PID) })

	g.link(order, byPID)
	active := g.launcherActivity(order)
	for _, p := range order {
		p.adapter = g.match(p.s)
		switch {
		case p.adapter != "":
			p.class, p.track = domain.ClassAgent, trackAgent
		default:
			p.class = agent.Classify(p.s)
			if p.class.Heavy() && (!agent.Launcher(p.s.Name) || g.launcherHeavy(now, p, active[p])) {
				p.track = trackHeavy
			}
		}
	}
	for _, p := range order {
		for q := p; q != nil; q = q.parent {
			if q.adapter != "" {
				p.owner = q
				break
			}
		}
	}

	changes := g.lifecycle(next, order)
	g.procs = next
	g.snap = g.build(order, busy, dt, rates)
	return changes
}

// launcherActivity sums CPU and RSS over the subtree of every launcher and
// reports which launchers are active now. Each process adds itself to every
// launcher above it, so nested launchers both count it.
func (g *Graph) launcherActivity(order []*proc) map[*proc]bool {
	type use struct {
		cpu float64
		rss uint64
	}
	var sums map[*proc]*use
	for _, p := range order {
		for q := p; q != nil; q = q.parent {
			if !agent.Launcher(q.s.Name) {
				continue
			}
			if sums == nil {
				sums = map[*proc]*use{}
			}
			u := sums[q]
			if u == nil {
				u = &use{}
				sums[q] = u
			}
			u.cpu += p.cpu
			u.rss += p.s.RSS
		}
	}
	active := make(map[*proc]bool, len(sums))
	for q, u := range sums {
		active[q] = u.cpu >= launcherCPU || u.rss >= launcherRSS
	}
	return active
}

// launcherHeavy reports whether launcher p is heavy at now, recording its
// activity; see launcherCPU.
func (g *Graph) launcherHeavy(now time.Time, p *proc, active bool) bool {
	if active {
		p.activeAt = now
		return true
	}
	if old := g.procs[p.s.Key()]; old != nil && old.track == trackHeavy && p.s.State == domain.StateStopped {
		return true
	}
	return !p.activeAt.IsZero() && now.Sub(p.activeAt) < launcherHold
}

// link sets parent pointers from PPIDs. PPID 0 or 1 (the kernel and init or
// launchd), a self-parent, an unknown PPID, or a parent that started after
// the child (its PID was reused) leaves the parent nil. Any remaining cycle
// is cut at its lowest PID so every ancestry walk terminates.
func (g *Graph) link(order []*proc, byPID map[int]*proc) {
	for _, p := range order {
		if pp := p.s.PPID; pp > 1 && pp != p.s.PID {
			if q := byPID[pp]; q != nil && !q.s.Start.After(p.s.Start) {
				p.parent = q
			}
		}
	}
	const (
		unseen = iota
		walking
		done
	)
	state := make(map[*proc]int, len(order))
	var path []*proc
	for _, p := range order {
		path = path[:0]
		q := p
		for q != nil && state[q] == unseen {
			state[q] = walking
			path = append(path, q)
			q = q.parent
		}
		if q != nil && state[q] == walking {
			// q is on the current path, so path[i:] from q is a cycle.
			i := slices.Index(path, q)
			low := slices.MinFunc(path[i:], func(a, b *proc) int { return cmp.Compare(a.s.PID, b.s.PID) })
			low.parent = nil
		}
		for _, x := range path {
			state[x] = done
		}
	}
}

func (g *Graph) match(s domain.ProcessSample) string {
	if g.hasAgentUID && s.UID != g.agentUID {
		return ""
	}
	for _, a := range g.adapters {
		if a != nil && a.Match(s) {
			return a.Name()
		}
	}
	return ""
}

// lifecycle diffs the old table against next, frees labels of lost agents,
// assigns labels to new ones, and returns sorted changes. A process whose
// tracked kind or adapter changes (e.g. after exec) is reported as lost and
// rediscovered.
func (g *Graph) lifecycle(next map[domain.ProcessKey]*proc, order []*proc) []Change {
	var changes []Change
	for k, old := range g.procs {
		if np := next[k]; np != nil && np.track == old.track && np.adapter == old.adapter {
			np.label = old.label
			continue
		}
		switch old.track {
		case trackAgent:
			delete(g.used[old.adapter], labelNum(old))
			changes = append(changes, Change{Kind: domain.EventAgentLost, Process: k, Name: old.s.Name, Agent: old.label})
		case trackHeavy:
			changes = append(changes, Change{Kind: domain.EventProcessLost, Process: k, Name: old.s.Name})
		}
	}
	// order is sorted by PID, so labels are assigned deterministically.
	for _, p := range order {
		if old := g.procs[p.s.Key()]; old != nil && old.track == p.track && old.adapter == p.adapter {
			continue
		}
		switch p.track {
		case trackAgent:
			p.label = g.assign(p.adapter)
			changes = append(changes, Change{Kind: domain.EventAgentDiscovered, Process: p.s.Key(), Name: p.s.Name, Agent: p.label})
		case trackHeavy:
			changes = append(changes, Change{Kind: domain.EventProcessDiscovered, Process: p.s.Key(), Name: p.s.Name})
		}
	}
	slices.SortFunc(changes, compareChanges)
	return changes
}

// compareChanges orders by PID, then Start, then losses before discoveries,
// so a reused PID or an exec reads as "old gone, new here".
func compareChanges(a, b Change) int {
	rank := func(k domain.EventKind) int {
		if k == domain.EventAgentLost || k == domain.EventProcessLost {
			return 0
		}
		return 1
	}
	return cmp.Or(
		cmp.Compare(a.Process.PID, b.Process.PID),
		a.Process.Start.Compare(b.Process.Start),
		cmp.Compare(rank(a.Kind), rank(b.Kind)),
		cmp.Compare(a.Kind, b.Kind),
	)
}

// assign takes the lowest free instance number for adapter.
func (g *Graph) assign(adapter string) string {
	used := g.used[adapter]
	if used == nil {
		used = map[int]struct{}{}
		g.used[adapter] = used
	}
	n := 1
	for {
		if _, ok := used[n]; !ok {
			break
		}
		n++
	}
	used[n] = struct{}{}
	return adapter + "-" + strconv.Itoa(n)
}

// labelNum recovers the instance number from an agent's label.
func labelNum(p *proc) int {
	n, _ := strconv.Atoi(p.label[len(p.adapter)+1:])
	return n
}

// build computes the snapshot from the linked, attributed table.
func (g *Graph) build(order []*proc, busy, dt float64, rates bool) Snapshot {
	var snap Snapshot
	if rates {
		snap.HasCPU = true
		snap.TotalCPU = min(max(busy/(dt*float64(g.ncpu)), 0), 1)
	}

	agents := map[*proc]int{}
	for _, p := range order {
		if p.owner == nil {
			continue
		}
		i, ok := agents[p.owner]
		if !ok {
			o := p.owner
			i = len(snap.Agents)
			agents[o] = i
			snap.Agents = append(snap.Agents, AgentView{Key: o.s.Key(), Agent: o.label, Name: o.s.Name})
		}
		v := &snap.Agents[i]
		v.CPU += p.cpu
		v.RSS += p.s.RSS
		if p != p.owner {
			v.Children++
		}
	}

	heavy := map[*proc]int{}
	for _, p := range order {
		if p.track != trackHeavy {
			continue
		}
		r := collapseRoot(p)
		i, ok := heavy[r]
		if !ok {
			i = len(snap.Heavy)
			heavy[r] = i
			v := WorkloadView{Key: r.s.Key(), Name: r.s.Name, Class: r.class}
			if r.owner != nil {
				v.Owner, v.OwnerPID = r.owner.label, r.owner.s.PID
			}
			snap.Heavy = append(snap.Heavy, v)
		}
		v := &snap.Heavy[i]
		v.CPU += p.cpu
		v.RSS += p.s.RSS
	}

	for _, p := range order { // already sorted by PID
		if p.s.State != domain.StateStopped {
			continue
		}
		v := StoppedView{Key: p.s.Key(), UID: p.s.UID, Name: p.s.Name, Class: p.class, Since: p.stoppedSince}
		if p.owner != nil {
			v.Owner = p.owner.label
		}
		if q := p.parent; q != nil {
			v.Parent = q.s.Name
			v.ParentOwned = q.owner != nil
			v.ParentKey = q.s.Key()
		}
		snap.Stopped = append(snap.Stopped, v)
	}

	byName := map[string]int{}
	for _, p := range order {
		if p.owner != nil {
			continue
		}
		snap.UnattributedRSS += p.s.RSS
		i, ok := byName[p.s.Name]
		if !ok {
			i = len(snap.Unattributed)
			byName[p.s.Name] = i
			snap.Unattributed = append(snap.Unattributed, NameUsage{Name: p.s.Name})
		}
		snap.Unattributed[i].Processes++
		snap.Unattributed[i].RSS += p.s.RSS
	}
	slices.SortFunc(snap.Unattributed, func(a, b NameUsage) int {
		return cmp.Or(cmp.Compare(b.RSS, a.RSS), cmp.Compare(a.Name, b.Name))
	})
	if len(snap.Unattributed) > MaxUnattributed {
		snap.Unattributed = slices.Clone(snap.Unattributed[:MaxUnattributed])
	}

	slices.SortFunc(snap.Agents, func(a, b AgentView) int {
		return cmp.Or(cmp.Compare(b.CPU, a.CPU), cmp.Compare(a.Key.PID, b.Key.PID))
	})
	slices.SortFunc(snap.Heavy, func(a, b WorkloadView) int {
		return cmp.Or(cmp.Compare(b.CPU, a.CPU), cmp.Compare(a.Key.PID, b.Key.PID))
	})
	return snap
}

// collapseRoot returns the topmost ancestor of heavy process p (or p itself)
// that is heavy and has p's owner, so a build and the compilers it spawns
// are one workload. Intermediate processes may be anything (make -> sh ->
// cc1 collapses), but a chain never crosses into another agent's ownership.
func collapseRoot(p *proc) *proc {
	r := p
	for q := p.parent; q != nil; q = q.parent {
		if q.track == trackHeavy && q.owner == p.owner {
			r = q
		}
	}
	return r
}

// Snapshot returns a copy of the view computed by the latest Update. Before
// the first Update it is empty.
func (g *Graph) Snapshot() Snapshot {
	s := g.snap
	s.Agents = slices.Clone(s.Agents)
	s.Heavy = slices.Clone(s.Heavy)
	s.Stopped = slices.Clone(s.Stopped)
	s.Unattributed = slices.Clone(s.Unattributed)
	return s
}

// Member is one process of a workload, as observed in the latest Update.
type Member struct {
	Key domain.ProcessKey
	UID uint32
}

// WorkloadTarget describes one top-level heavy workload and the processes
// that make it up, for callers that may act on it (e.g. process control).
type WorkloadTarget struct {
	Key   domain.ProcessKey
	Name  string
	Class domain.WorkloadClass
	// Owner is the owning agent instance label, or "" when unattributed.
	Owner string
	// Members are the workload's top process followed by every descendant
	// with the same owner, parents before children (siblings by PID). A
	// subtree owned by a nested agent is excluded.
	Members []Member
}

// Live reports whether key was a live process as of the latest Update.
func (g *Graph) Live(key domain.ProcessKey) bool { return g.procs[key] != nil }

// Workload returns the top-level heavy workload whose top process is key,
// as of the latest Update. It reports false when key is not a live process,
// is not heavy, or was collapsed into a heavy ancestor. The result
// shares no memory with the graph.
func (g *Graph) Workload(key domain.ProcessKey) (WorkloadTarget, bool) {
	r := g.procs[key]
	if r == nil || r.track != trackHeavy || collapseRoot(r) != r {
		return WorkloadTarget{}, false
	}
	children := map[*proc][]*proc{}
	for _, p := range g.procs {
		if p.parent != nil {
			children[p.parent] = append(children[p.parent], p)
		}
	}
	t := WorkloadTarget{Key: key, Name: r.s.Name, Class: r.class}
	if r.owner != nil {
		t.Owner = r.owner.label
	}
	// Breadth-first from the root, so every parent precedes its children.
	// Links are acyclic (see link), so the walk terminates.
	queue := []*proc{r}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		t.Members = append(t.Members, Member{Key: p.s.Key(), UID: p.s.UID})
		kids := children[p]
		slices.SortFunc(kids, func(a, b *proc) int { return cmp.Compare(a.s.PID, b.s.PID) })
		for _, c := range kids {
			// A process with another owner (a nested agent, or anything it
			// owns) is not part of this workload, and neither is its subtree.
			if c.owner == r.owner {
				queue = append(queue, c)
			}
		}
	}
	return t, true
}
