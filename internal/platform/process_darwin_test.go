//go:build darwin

package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLookupPSNames(t *testing.T) {
	ctx := context.Background()
	self := os.Getpid()
	want := filepath.Base(os.Args[0])
	// One live PID and one that cannot exist, so the one-PID path runs and
	// a missing PID is just unnamed.
	got, err := lookupPSNames(ctx, []int{self, 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if got[self] != want {
		t.Errorf("name of self = %q, want %q", got[self], want)
	}
	if name, ok := got[1<<30]; ok {
		t.Errorf("nonexistent pid named %q", name)
	}
	// Over maxPSNameQueries PIDs that kern.procargs2 cannot read, one full
	// ps pass names them, and every other process too.
	pids := []int{self}
	for i := range maxPSNameQueries + 1 {
		pids = append(pids, 1<<30+i)
	}
	got, err = lookupPSNames(ctx, pids)
	if err != nil {
		t.Fatal(err)
	}
	if got[self] != want || len(got) < 2 {
		t.Errorf("full pass: self = %q, %d names", got[self], len(got))
	}
}

// TestProcCommMatchesPS checks that kern.procargs2 names every readable
// process exactly as a ps names pass does.
func TestProcCommMatchesPS(t *testing.T) {
	ctx := context.Background()
	if comm, err := procComm(os.Getpid()); err != nil || psName(comm) != filepath.Base(os.Args[0]) {
		t.Fatalf("procComm(self) = %q, %v", comm, err)
	}
	if _, err := procComm(1 << 30); err == nil {
		t.Error("procComm of a nonexistent pid succeeded")
	}
	psNames := func() map[int]string {
		out, err := runPS(ctx, psNameColumns)
		if err != nil {
			t.Fatal(err)
		}
		names, err := parsePSNames(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	var read int
	for pid, want := range psNames() {
		comm, err := procComm(pid)
		if err != nil {
			continue // another user's process, or gone since the ps pass
		}
		read++
		if psName(comm) == want {
			continue
		}
		// The process may have exec'd or exited between the two reads;
		// only a mismatch that a second ps pass confirms counts.
		now := psNames()[pid]
		if comm, err := procComm(pid); err == nil && now != "" && psName(comm) != now {
			t.Errorf("pid %d: procargs name %q, ps name %q", pid, psName(comm), now)
		}
	}
	if read == 0 {
		t.Fatal("kern.procargs2 read no process")
	}
}

func TestSampleProcessesNamesSelf(t *testing.T) {
	s := NewProcessSampler()
	want := filepath.Base(os.Args[0])
	for i := range 2 { // a full pass, then the cache
		procs, err := s.SampleProcesses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range procs {
			if p.PID == os.Getpid() {
				found = p.Name == want
			}
		}
		if !found {
			t.Fatalf("sample %d: self not named %q", i, want)
		}
	}
}
