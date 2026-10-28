package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSampleProcessesSelf(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	procs, err := NewProcessSampler().SampleProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) < 2 {
		t.Fatalf("got %d processes, want at least 2", len(procs))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantName := filepath.Base(exe)
	if runtime.GOOS == "linux" && len(wantName) > 15 {
		wantName = wantName[:15] // TASK_COMM_LEN - 1
	}
	pid := os.Getpid()
	for _, p := range procs {
		if p.PID != pid {
			continue
		}
		if p.PPID != os.Getppid() {
			t.Errorf("PPID = %d, want %d", p.PPID, os.Getppid())
		}
		if p.Start.IsZero() || p.Start.After(time.Now().Add(time.Second)) {
			t.Errorf("Start = %v", p.Start)
		}
		if p.Name != wantName {
			t.Errorf("Name = %q, want %q", p.Name, wantName)
		}
		if p.UID != uint32(os.Getuid()) {
			t.Errorf("UID = %d, want %d", p.UID, os.Getuid())
		}
		if p.RSS == 0 {
			t.Error("RSS = 0")
		}
		return
	}
	t.Fatalf("own pid %d not in %d processes", pid, len(procs))
}

func BenchmarkSampleProcesses(b *testing.B) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		b.Skip("unsupported OS")
	}
	s := NewProcessSampler()
	for b.Loop() {
		if _, err := s.SampleProcesses(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSampleProcessesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewProcessSampler().SampleProcesses(ctx)
	want := context.Canceled
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		want = errors.ErrUnsupported
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
