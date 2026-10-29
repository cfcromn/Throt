package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// TestMain keeps paused.json writes off the disk's flush path: these tests
// fake every process, and a full fsync on a loaded macOS runner can stall
// the sampling goroutine past their deadlines between a scheduler decision
// and its status. TestWriteFileAtomicSyncs covers the real sync.
func TestMain(m *testing.M) {
	syncFile = func(*os.File) error { return nil }
	os.Exit(m.Run())
}

func TestWriteFileAtomicSyncs(t *testing.T) {
	var synced []string
	defer func(orig func(*os.File) error) { syncFile = orig }(syncFile)
	syncFile = func(f *os.File) error {
		synced = append(synced, filepath.Base(f.Name()))
		return f.Sync()
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "paused.json")
	if err := writeFileAtomic(path, []byte("state")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "state" {
		t.Fatalf("paused.json = %q, %v", b, err)
	}
	// The temporary file before the rename, then the directory after it.
	if len(synced) != 2 || !strings.HasPrefix(synced[0], "paused.json.") || synced[1] != filepath.Base(dir) {
		t.Fatalf("synced %q, want the temporary file then %q", synced, filepath.Base(dir))
	}
}

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

// fakeSignaler verifies identities against fakeSampler's processes and
// records signals instead of sending them.
type fakeSignaler struct {
	mu       sync.Mutex
	sent     []string
	contFail bool // Continue fails transiently
}

func (f *fakeSignaler) identity(k domain.ProcessKey) (uid uint32, ok bool) {
	procs, _ := fakeSampler{}.SampleProcesses(context.Background())
	for _, p := range procs {
		if p.PID == k.PID && p.Start.Equal(k.Start) {
			return p.UID, true
		}
	}
	return 0, false
}

func (f *fakeSignaler) record(sig string, k domain.ProcessKey, uid uint32) error {
	if got, ok := f.identity(k); !ok || got != uid {
		return domain.ErrProcessGone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, fmt.Sprintf("%s:%d", sig, k.PID))
	return nil
}

func (f *fakeSignaler) Stop(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return f.record("stop", k, uid)
}

func (f *fakeSignaler) Continue(_ context.Context, k domain.ProcessKey, uid uint32) error {
	f.mu.Lock()
	fail := f.contFail
	f.mu.Unlock()
	if fail {
		return errors.New("ps: fork: resource temporarily unavailable")
	}
	return f.record("cont", k, uid)
}

func (f *fakeSignaler) setContFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contFail = fail
}

func (f *fakeSignaler) Identify(ctx context.Context, pid int) (domain.ProcessSample, error) {
	procs, _ := fakeSampler{}.SampleProcesses(ctx)
	for _, p := range procs {
		if p.PID == pid {
			return p, nil
		}
	}
	return domain.ProcessSample{}, domain.ErrProcessGone
}

func (f *fakeSignaler) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	_, ok := f.identity(k)
	return !ok, nil
}

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

	// The daemon answers before its first sample lands; wait for it.
	var st StatusResult
	waitFor(t, "first sample", func() bool {
		st = StatusResult{}
		return transport.Call(context.Background(), sock, "status", nil, &st) == nil && st.Host != nil && len(st.Agents) > 0
	})
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

func TestListenSocketNeverOpenToOthers(t *testing.T) {
	// A traversable socket dir passes the directory check; under a lax
	// umask the socket must still never exist with group/other bits.
	dir := filepath.Join(shortTempDir(t), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	var created os.FileMode
	orig := listenUnix
	defer func() { listenUnix = orig }()
	listenUnix = func(path string) (net.Listener, error) {
		l, err := orig(path)
		if err == nil {
			fi, serr := os.Lstat(path)
			if serr != nil {
				t.Error(serr)
			} else {
				created = fi.Mode().Perm()
			}
		}
		return l, err
	}
	l, err := listen(filepath.Join(dir, "throtd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if created&0o077 != 0 {
		t.Fatalf("socket created with mode %v, open to other users until chmod", created)
	}
	if cur := syscall.Umask(0); cur != 0 {
		t.Fatalf("umask left at %#o, want it restored", cur)
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

func TestDaemonKeepsUnresumedStateOnShutdown(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "throtd.sock")
	sig := &fakeSignaler{}
	stop := startDaemonWith(t, sock, sig)
	deadline := time.Now().Add(2 * time.Second)
	for transport.Call(context.Background(), sock, "pause", ControlParams{PID: 11}, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("pause never succeeded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sig.setContFail(true)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "paused.json"))
	if err != nil {
		t.Fatalf("unresumed work not persisted: %v", err)
	}
	if !strings.Contains(string(b), `"state":"resuming"`) || !strings.Contains(string(b), `"pid":11`) {
		t.Errorf("paused.json = %s", b)
	}

	// The next daemon resumes it and removes the file.
	sig.setContFail(false)
	stop = startDaemonWith(t, sock, sig)
	if got := sig.signals(); got != "stop:11 cont:11" {
		t.Errorf("signals = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "paused.json")); !os.IsNotExist(err) {
		t.Errorf("paused.json left after recovery: %v", err)
	}
	stop()
}

func TestDaemonMovesAsideMalformedState(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "throtd.sock")
	if err := os.WriteFile(filepath.Join(dir, "paused.json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	sig := &fakeSignaler{}
	stop := startDaemonWith(t, sock, sig)
	stop()
	if b, err := os.ReadFile(filepath.Join(dir, "paused.json.invalid")); err != nil || string(b) != "garbage" {
		t.Errorf("malformed state not kept: %q, %v", b, err)
	}
	if got := sig.signals(); got != "" {
		t.Errorf("signals = %q", got)
	}
}

func TestDaemonEventsSince(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "throtd.sock")
	stop := startDaemon(t, sock)
	defer stop()
	ctx := context.Background()

	var all []domain.Event
	waitFor(t, "a few events", func() bool {
		all = nil
		return transport.Call(ctx, sock, "events", nil, &all) == nil && len(all) >= 2
	})
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("seq not increasing: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}
	var after []domain.Event
	if err := transport.Call(ctx, sock, "events", EventsParams{Since: all[0].Seq, Limit: 1}, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Seq != all[1].Seq {
		t.Fatalf("since %d limit 1 = %+v, want seq %d", all[0].Seq, after, all[1].Seq)
	}
}

func TestControlRefusesStaleStart(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "throtd.sock")
	sig := &fakeSignaler{}
	stop := startDaemonWith(t, sock, sig)
	defer stop()
	ctx := context.Background()
	var st StatusResult
	waitFor(t, "heavy workload", func() bool {
		st = StatusResult{}
		return transport.Call(ctx, sock, "status", nil, &st) == nil && len(st.Heavy) == 1
	})
	stale := ControlParams{PID: st.Heavy[0].Key.PID, Start: st.Heavy[0].Key.Start.Add(-time.Hour)}
	if err := transport.Call(ctx, sock, "pause", stale, nil); err == nil || !strings.Contains(err.Error(), "PID reused") {
		t.Fatalf("stale pause err = %v", err)
	}
	if sig.signals() != "" {
		t.Fatalf("signals sent: %q", sig.signals())
	}
	fresh := ControlParams{PID: st.Heavy[0].Key.PID, Start: st.Heavy[0].Key.Start}
	if err := transport.Call(ctx, sock, "pause", fresh, nil); err != nil {
		t.Fatal(err)
	}
	if err := transport.Call(ctx, sock, "resume", fresh, nil); err != nil {
		t.Fatal(err)
	}
}

// TestPressureWindowFollowsInterval checks that the pressure window holds
// ten samples at long sample intervals, and 30s at short ones.
func TestPressureWindowFollowsInterval(t *testing.T) {
	for interval, want := range map[time.Duration]time.Duration{
		2 * time.Second:  30 * time.Second,
		5 * time.Second:  50 * time.Second,
		10 * time.Second: 100 * time.Second,
	} {
		cfg := config.Default()
		cfg.SampleInterval = config.Duration(interval)
		d := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, &fakeSignaler{}, slog.New(slog.DiscardHandler))
		if got := d.engine.Window(); got != want {
			t.Errorf("interval %v: window %v, want %v", interval, got, want)
		}
	}
}
