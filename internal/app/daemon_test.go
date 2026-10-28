package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

type fakeSampler struct{}

func (fakeSampler) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	start := time.Unix(1, 0)
	return []domain.ProcessSample{
		{PID: 10, PPID: 1, UID: uint32(os.Getuid()), Start: start, Name: "claude"},
		{PID: 11, PPID: 10, UID: uint32(os.Getuid()), Start: start, Name: "go", RSS: 1 << 20},
		{PID: 12, PPID: 1, UID: uint32(os.Getuid()) + 1, Start: start, Name: "claude"}, // another user's
	}, nil
}

func (fakeSampler) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{Time: time.Now(), MemTotal: 100, MemAvailable: 50}, nil
}

// fakeSignaler reports identities for fakeSampler's processes and records
// signals instead of sending them.
type fakeSignaler struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSignaler) Identity(_ context.Context, pid int) (time.Time, uint32, error) {
	procs, _ := fakeSampler{}.SampleProcesses(context.Background())
	for _, p := range procs {
		if p.PID == pid {
			return p.Start, p.UID, nil
		}
	}
	return time.Time{}, 0, domain.ErrProcessGone
}

func (f *fakeSignaler) record(sig string, pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fmt.Sprintf("%s:%d", sig, pid))
	return nil
}

func (f *fakeSignaler) Stop(pid int) error     { return f.record("stop", pid) }
func (f *fakeSignaler) Continue(pid int) error { return f.record("cont", pid) }

func (f *fakeSignaler) signals() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.sent, " ")
}

// shortTempDir keeps socket paths under the 104-byte sun_path limit on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "throt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startDaemon(t *testing.T, sock string) (cancel func() error) {
	t.Helper()
	return startDaemonWith(t, sock, &fakeSignaler{})
}

func startDaemonWith(t *testing.T, sock string, sig *fakeSignaler) (cancel func() error) {
	t.Helper()
	cfg := config.Default()
	cfg.SocketPath = sock
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, fakeSampler{}, fakeSampler{}, sig, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for transport.Call(context.Background(), sock, "ping", nil, nil) != nil {
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("daemon did not start: %v", <-done)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() error { stop(); return <-done }
}

func TestDaemonLifecycle(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "d", "throtd.sock")
	stop := startDaemon(t, sock)

	var ping PingResult
	if err := transport.Call(context.Background(), sock, "ping", nil, &ping); err != nil {
		t.Fatal(err)
	}
	if ping.Protocol != transport.ProtocolVersion || ping.PID != os.Getpid() {
		t.Errorf("ping = %+v", ping)
	}
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	var events []domain.Event
	if err := transport.Call(context.Background(), sock, "events", EventsParams{Limit: 10}, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Kind != domain.EventDaemonStarted {
		t.Errorf("events = %+v", events)
	}

	var st StatusResult
	if err := transport.Call(context.Background(), sock, "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Host == nil || st.Host.MemTotal != 100 {
		t.Errorf("status = %+v", st)
	}
	if len(st.Agents) != 1 || st.Agents[0].Agent != "claude-1" || len(st.Heavy) != 1 || st.Heavy[0].Owner != "claude-1" {
		t.Errorf("agents = %+v, heavy = %+v", st.Agents, st.Heavy)
	}

	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed on shutdown: %v", err)
	}
}

func TestDaemonRefusesSecondInstance(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "throtd.sock")
	stop := startDaemon(t, sock)
	defer stop()

	cfg := config.Default()
	cfg.SocketPath = sock
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
}

func TestDaemonReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "throtd.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // leaves a stale socket file, as a crashed daemon would
	stop := startDaemon(t, sock)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonKeepsNonSocketFile(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "throtd.sock")
	if err := os.WriteFile(sock, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SocketPath = sock
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(sock); string(b) != "important" {
		t.Fatal("non-socket file was modified")
	}
}

func TestDaemonRejectsSharedSocketDir(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "shared")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o777) // bypass umask
	cfg := config.Default()
	cfg.SocketPath = filepath.Join(dir, "throtd.sock")
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("err = %v", err)
	}
}

func TestDaemonPauseResume(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "throtd.sock")
	sig := &fakeSignaler{}
	stop := startDaemonWith(t, sock, sig)
	ctx := context.Background()

	// Wait for the first sample so pid 11 shows up as a heavy workload.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st StatusResult
		if err := transport.Call(ctx, sock, "status", nil, &st); err == nil && len(st.Heavy) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heavy workload sampled")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := transport.Call(ctx, sock, "pause", ControlParams{PID: 10}, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("pausing the agent itself: err = %v", err)
	}
	if err := transport.Call(ctx, sock, "pause", ControlParams{PID: 11}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "paused.json")); err != nil {
		t.Errorf("paused state not persisted: %v", err)
	}
	if err := transport.Call(ctx, sock, "pause", ControlParams{PID: 11}, nil); err == nil {
		t.Fatal("double pause succeeded")
	}
	var st StatusResult
	if err := transport.Call(ctx, sock, "status", nil, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Paused) != 1 || st.Paused[0].Key.PID != 11 {
		t.Errorf("paused = %+v", st.Paused)
	}
	if err := transport.Call(ctx, sock, "resume", ControlParams{PID: 11}, nil); err != nil {
		t.Fatal(err)
	}
	if got := sig.signals(); got != "stop:11 cont:11" {
		t.Errorf("signals = %q", got)
	}

	// Shutdown resumes anything still paused.
	if err := transport.Call(ctx, sock, "pause", ControlParams{PID: 11}, nil); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := sig.signals(); got != "stop:11 cont:11 stop:11 cont:11" {
		t.Errorf("signals after shutdown = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "paused.json")); !os.IsNotExist(err) {
		t.Errorf("paused state left behind: %v", err)
	}
}

func TestDaemonRecoversPausedState(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "throtd.sock")
	// A previous daemon crashed with pid 11 paused.
	sig := &fakeSignaler{}
	first := startDaemonWith(t, sock, sig)
	deadline := time.Now().Add(2 * time.Second)
	for transport.Call(context.Background(), sock, "pause", ControlParams{PID: 11}, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("pause never succeeded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, err := os.ReadFile(filepath.Join(dir, "paused.json"))
	if err != nil {
		t.Fatal(err)
	}
	first()
	if err := os.WriteFile(filepath.Join(dir, "paused.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	sig2 := &fakeSignaler{}
	stop := startDaemonWith(t, sock, sig2)
	if got := sig2.signals(); got != "cont:11" {
		t.Errorf("recovery signals = %q, want cont:11", got)
	}
	stop()
}
