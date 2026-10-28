//go:build darwin

package platform

import (
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
	// Over maxPSNameQueries PIDs, one full pass names them all.
	pids := make([]int, maxPSNameQueries+1)
	for i := range pids {
		pids[i] = self
	}
	got, err = lookupPSNames(ctx, pids)
	if err != nil {
		t.Fatal(err)
	}
	if got[self] != want || len(got) < 2 {
		t.Errorf("full pass: self = %q, %d names", got[self], len(got))
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
