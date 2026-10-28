package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
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
		{PID: 10, PPID: 1, Start: start, Name: "claude"},
		{PID: 11, PPID: 10, Start: start, Name: "go", RSS: 1 << 20},
	}, nil
}

func (fakeSampler) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{Time: time.Now(), MemTotal: 100, MemAvailable: 50}, nil
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
	cfg := config.Default()
	cfg.SocketPath = sock
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewDaemon(cfg, fakeSampler{}, fakeSampler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
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
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
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
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
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
	err := NewDaemon(cfg, fakeSampler{}, fakeSampler{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("err = %v", err)
	}
}
