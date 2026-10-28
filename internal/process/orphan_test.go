package process

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestFindOrphans(t *testing.T) {
	const uid = 501
	since := epoch.Add(time.Minute)
	stopped := func(pid int, name string, class domain.WorkloadClass, owner, parent string, parentOwned bool) StoppedView {
		return StoppedView{Key: key(pid, 0), UID: uid, Name: name, Class: class, Owner: owner,
			Parent: parent, ParentOwned: parentOwned, Since: since}
	}
	tests := []struct {
		name     string
		in       StoppedView
		recorded bool
		want     OrphanKind // "" means not reported
		hint     string
		resume   bool // AttributedHeavy
	}{
		{name: "recorded by the controller", in: stopped(20, "make", domain.ClassBuild, "claude-1", "claude", true), recorded: true},
		{name: "attributed heavy, unrecorded", in: stopped(21, "make", domain.ClassBuild, "claude-1", "claude", true),
			want: OrphanLeftPaused, hint: "likely left paused by an earlier throtd (kill -CONT 21 or throt resume 21)", resume: true},
		{name: "agent's command shell is not job control", in: stopped(22, "go", domain.ClassBuild, "codex-1", "bash", true),
			want: OrphanLeftPaused, resume: true},
		{name: "shell job", in: stopped(23, "vim", domain.ClassUnknown, "", "zsh", false),
			want: OrphanJobControl, hint: "likely job control (Ctrl-Z)"},
		{name: "login shell job", in: stopped(24, "make", domain.ClassBuild, "", "-zsh", false), want: OrphanJobControl},
		{name: "agent suspended in a terminal", in: stopped(25, "claude", domain.ClassAgent, "claude-1", "fish", false), want: OrphanJobControl},
		{name: "attributed, not heavy", in: stopped(26, "node", domain.ClassUnknown, "claude-1", "claude", true),
			want: OrphanLeftPaused, hint: "throt resume --force 26"},
		{name: "unattributed heavy, reparented", in: stopped(27, "cargo", domain.ClassBuild, "", "", false),
			want: OrphanLeftPaused, hint: "throt resume --force 27"},
		{name: "unknown", in: stopped(28, "less", domain.ClassUnknown, "", "tmux", false),
			want: OrphanUnknown, hint: "kill -CONT 28"},
		{name: "foreign uid ignored", in: func() StoppedView {
			v := stopped(29, "make", domain.ClassBuild, "claude-1", "claude", true)
			v.UID = uid + 1
			return v
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, total := FindOrphans([]StoppedView{tt.in}, uid, func(domain.ProcessKey) bool { return tt.recorded })
			if tt.want == "" {
				if total != 0 || len(got) != 0 {
					t.Fatalf("got %+v (total %d), want nothing", got, total)
				}
				return
			}
			if total != 1 || len(got) != 1 {
				t.Fatalf("got %+v (total %d), want one", got, total)
			}
			o := got[0]
			if o.Kind != tt.want || !strings.Contains(o.Hint, tt.hint) || o.AttributedHeavy() != tt.resume {
				t.Errorf("got kind %s hint %q resume %v; want %s, %q, %v", o.Kind, o.Hint, o.AttributedHeavy(), tt.want, tt.hint, tt.resume)
			}
			if o.Key != tt.in.Key || o.Name != tt.in.Name || o.Owner != tt.in.Owner || !o.Since.Equal(since) {
				t.Errorf("got %+v, want fields copied from %+v", o, tt.in)
			}
		})
	}
}

func TestFindOrphansRecordedMembers(t *testing.T) {
	in := []StoppedView{
		{Key: key(30, 0), UID: 1, Name: "make", Class: domain.ClassBuild, Owner: "claude-1"},
		{Key: key(31, 0), UID: 1, Name: "cc1", Class: domain.ClassBuild, Owner: "claude-1"},
		{Key: key(31, 5), UID: 1, Name: "cc1", Class: domain.ClassBuild, Owner: "claude-1"}, // PID reused
	}
	recorded := map[domain.ProcessKey]bool{key(30, 0): true, key(31, 0): true}
	got, total := FindOrphans(in, 1, func(k domain.ProcessKey) bool { return recorded[k] })
	if total != 1 || len(got) != 1 || got[0].Key != key(31, 5) {
		t.Fatalf("got %+v (total %d), want only the reused pid 31", got, total)
	}
}

func TestFindOrphansBound(t *testing.T) {
	var in []StoppedView
	for i := range MaxOrphans + 7 {
		in = append(in, StoppedView{Key: key(100+i, 0), UID: 1, Name: "p" + strconv.Itoa(i)})
	}
	got, total := FindOrphans(in, 1, func(domain.ProcessKey) bool { return false })
	if total != MaxOrphans+7 || len(got) != MaxOrphans || got[0].Key.PID != 100 || got[MaxOrphans-1].Key.PID != 100+MaxOrphans-1 {
		t.Fatalf("got %d orphans (total %d), want the first %d of %d", len(got), total, MaxOrphans, MaxOrphans+7)
	}
}

// TestGraphStopped checks attribution of stopped processes and how long
// they are observed stopped: Since survives samples and resets on resume.
func TestGraphStopped(t *testing.T) {
	g := newGraph()
	at := func(sec int) time.Time { return epoch.Add(time.Duration(sec) * time.Second) }
	procs := func(makeState domain.ProcessState) []domain.ProcessSample {
		out := samples([]ps{
			{pid: 10, ppid: 1, name: "zsh"},
			{pid: 11, ppid: 10, name: "claude"},
			{pid: 12, ppid: 11, name: "bash"},
			{pid: 13, ppid: 12, name: "make"},
			{pid: 14, ppid: 10, name: "vim"},
		})
		out[3].State = makeState
		out[4].State = domain.StateStopped
		return out
	}
	g.Update(at(0), procs(domain.StateRunning))
	g.Update(at(1), procs(domain.StateStopped))
	g.Update(at(5), procs(domain.StateStopped))
	snap := g.Snapshot()
	if len(snap.Stopped) != 2 {
		t.Fatalf("stopped = %+v, want make and vim", snap.Stopped)
	}
	mk, vim := snap.Stopped[0], snap.Stopped[1]
	if mk.Key.PID != 13 || mk.Owner != "claude-1" || mk.Class != domain.ClassBuild || mk.Parent != "bash" || !mk.ParentOwned || !mk.Since.Equal(at(1)) {
		t.Errorf("make = %+v", mk)
	}
	if vim.Key.PID != 14 || vim.Owner != "" || vim.Parent != "zsh" || vim.ParentOwned || !vim.Since.Equal(at(0)) {
		t.Errorf("vim = %+v", vim)
	}
	orphans, _ := FindOrphans(snap.Stopped, 0, func(domain.ProcessKey) bool { return false })
	if len(orphans) != 2 || orphans[0].Kind != OrphanLeftPaused || !orphans[0].AttributedHeavy() || orphans[1].Kind != OrphanJobControl {
		t.Errorf("orphans = %+v", orphans)
	}

	// Resumed, then stopped again: observed afresh.
	g.Update(at(6), procs(domain.StateRunning))
	g.Update(at(7), procs(domain.StateStopped))
	if s := g.Snapshot().Stopped; len(s) != 2 || !s[0].Since.Equal(at(7)) {
		t.Errorf("after resume and stop = %+v, want make since %v", s, at(7))
	}
}
