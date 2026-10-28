package process

import (
	"reflect"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

func mem(pid, start int, uid uint32) Member { return Member{Key: key(pid, start), UID: uid} }

func TestWorkload(t *testing.T) {
	tests := []struct {
		name   string
		procs  []ps
		key    domain.ProcessKey
		want   WorkloadTarget
		wantOK bool
	}{
		{
			name: "attributed workload with descendants in parent-first order",
			procs: []ps{
				{pid: 10, ppid: 1, name: "claude", uid: 501},
				{pid: 20, ppid: 10, name: "make", uid: 501},
				{pid: 25, ppid: 20, name: "sh", uid: 501},
				{pid: 22, ppid: 20, name: "cc", uid: 501},
				{pid: 30, ppid: 25, name: "make", uid: 501},
				{pid: 23, ppid: 22, name: "ld", uid: 0},
			},
			key: key(20, 0),
			want: WorkloadTarget{
				Key: key(20, 0), Name: "make", Class: domain.ClassBuild, Owner: "claude-1",
				Members: []Member{mem(20, 0, 501), mem(22, 0, 501), mem(25, 0, 501), mem(23, 0, 0), mem(30, 0, 501)},
			},
			wantOK: true,
		},
		{
			name: "unattributed workload",
			procs: []ps{
				{pid: 5, ppid: 1, name: "go", uid: 7},
				{pid: 6, ppid: 5, name: "compile", uid: 7},
			},
			key: key(5, 0),
			want: WorkloadTarget{
				Key: key(5, 0), Name: "go", Class: domain.ClassBuild,
				Members: []Member{mem(5, 0, 7), mem(6, 0, 7)},
			},
			wantOK: true,
		},
		{
			name: "nested agent subtree is excluded",
			procs: []ps{
				{pid: 10, ppid: 1, name: "claude"},
				{pid: 20, ppid: 10, name: "npm"},
				{pid: 21, ppid: 20, name: "node"},
				{pid: 22, ppid: 20, name: "codex"},
				{pid: 23, ppid: 22, name: "sh"},
				{pid: 24, ppid: 23, name: "go"},
			},
			key: key(20, 0),
			want: WorkloadTarget{
				Key: key(20, 0), Name: "npm", Class: domain.ClassBuild, Owner: "claude-1",
				Members: []Member{mem(20, 0, 0), mem(21, 0, 0)},
			},
			wantOK: true,
		},
		{
			name: "unattributed root excludes an agent below it",
			procs: []ps{
				{pid: 5, ppid: 1, name: "make"},
				{pid: 6, ppid: 5, name: "claude"},
				{pid: 7, ppid: 6, name: "sh"},
			},
			key: key(5, 0),
			want: WorkloadTarget{
				Key: key(5, 0), Name: "make", Class: domain.ClassBuild,
				Members: []Member{mem(5, 0, 0)},
			},
			wantOK: true,
		},
		{
			name:  "collapsed heavy descendant is not top level",
			procs: []ps{{pid: 5, ppid: 1, name: "go"}, {pid: 6, ppid: 5, name: "go"}},
			key:   key(6, 0),
		},
		{
			name:  "non-heavy process",
			procs: []ps{{pid: 5, ppid: 1, name: "sh"}},
			key:   key(5, 0),
		},
		{
			name:  "agent is not a workload",
			procs: []ps{{pid: 5, ppid: 1, name: "claude"}},
			key:   key(5, 0),
		},
		{
			name:  "unknown key",
			procs: []ps{{pid: 5, ppid: 1, name: "go"}},
			key:   key(9, 0),
		},
		{
			name:  "reused pid has a different key",
			procs: []ps{{pid: 5, ppid: 1, name: "go", start: 3}},
			key:   key(5, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGraph()
			g.Update(epoch, samples(tt.procs))
			got, ok := g.Workload(tt.key)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Workload =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestWorkloadLatestSnapshotAndCopy(t *testing.T) {
	g := newGraph()
	g.Update(epoch, samples([]ps{{pid: 5, ppid: 1, name: "go"}, {pid: 6, ppid: 5, name: "compile"}}))
	w, ok := g.Workload(key(5, 0))
	if !ok || len(w.Members) != 2 {
		t.Fatalf("Workload = %+v, %v", w, ok)
	}
	w.Members[0].UID = 99
	if again, _ := g.Workload(key(5, 0)); again.Members[0].UID != 0 {
		t.Error("mutating the result changed the graph")
	}

	g.Update(epoch.Add(1), samples([]ps{{pid: 5, ppid: 1, name: "go"}}))
	if w, _ := g.Workload(key(5, 0)); len(w.Members) != 1 {
		t.Errorf("members after child exit = %+v, want only the top", w.Members)
	}
	g.Update(epoch.Add(2), nil)
	if _, ok := g.Workload(key(5, 0)); ok {
		t.Error("workload still reported after its process exited")
	}
}

func TestLive(t *testing.T) {
	g := newGraph()
	g.Update(epoch, samples([]ps{{pid: 5, ppid: 1, name: "go"}, {pid: 7, ppid: 1, name: "sleep"}}))
	if !g.Live(key(5, 0)) || !g.Live(key(7, 0)) {
		t.Fatal("live processes reported dead")
	}
	g.Update(epoch.Add(1), samples([]ps{{pid: 7, ppid: 1, name: "sleep"}}))
	if g.Live(key(5, 0)) {
		t.Error("exited process reported live")
	}
	if !g.Live(key(7, 0)) || g.Live(key(7, 1)) {
		t.Error("liveness must match the process start time")
	}
}
