package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortTimeout lowers ioTimeout for one test.
func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := ioTimeout
	ioTimeout = d
	t.Cleanup(func() { ioTimeout = old })
}

func readConnResponse(t *testing.T, conn net.Conn) Response {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

// TestStalledClientsReleaseSlots checks that clients which send half a
// request and stall are cut off by the server's deadline, so they cannot
// hold every connection slot and lock out the real client.
func TestStalledClientsReleaseSlots(t *testing.T) {
	shortTimeout(t, 200*time.Millisecond)
	sock := startServer(t, map[string]Handler{
		"ping": func(context.Context, json.RawMessage) (any, error) { return "pong", nil },
	})
	var stalled []net.Conn
	for range maxConns {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := io.WriteString(c, `{"protocol":1,"meth`); err != nil {
			t.Fatal(err)
		}
		stalled = append(stalled, c)
	}
	// The server hangs up on each stalled client once the deadline passes.
	for _, c := range stalled {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("stalled client was not cut off: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got string
	if err := Call(ctx, sock, "ping", nil, &got); err != nil || got != "pong" {
		t.Fatalf("ping after stalled clients = %q, %v", got, err)
	}
}

// TestCallTimesOutOnSlowDaemon checks that a daemon that accepts but never
// answers fails the call by the caller's deadline instead of hanging.
func TestCallTimesOutOnSlowDaemon(t *testing.T) {
	const io = 2 * time.Second
	shortTimeout(t, io)
	release := make(chan struct{})
	sock := startServer(t, map[string]Handler{
		"hang": func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-release
			return nil, nil
		},
	})
	defer close(release) // before startServer's cleanup waits for handlers
	tests := []struct {
		name     string
		deadline time.Duration // 0: none, so ioTimeout applies
		min, max time.Duration
	}{
		// A caller deadline shorter than ioTimeout wins.
		{"caller deadline", 100 * time.Millisecond, 0, io * 3 / 4},
		{"io timeout", 0, io * 3 / 4, 3 * io},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.deadline > 0 {
				ctx, cancel = context.WithTimeout(context.Background(), tt.deadline)
			}
			defer cancel()
			start := time.Now()
			err := Call(ctx, sock, "hang", nil, nil)
			if err == nil || !strings.HasPrefix(err.Error(), "read response:") || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("err = %v, want a read timeout", err)
			}
			if d := time.Since(start); d < tt.min || d > tt.max {
				t.Errorf("call took %v, want %v..%v", d, tt.min, tt.max)
			}
		})
	}
}

// TestOversizedRequest checks that a request beyond maxBodyBytes is refused
// without running the handler.
func TestOversizedRequest(t *testing.T) {
	var ran sync.WaitGroup
	var called atomic.Bool
	sock := startServer(t, map[string]Handler{
		"echo": func(context.Context, json.RawMessage) (any, error) { called.Store(true); return nil, nil },
	})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ran.Add(1)
	go func() {
		defer ran.Done()
		// The server stops reading at the limit, so this write may fail.
		io.WriteString(conn, `{"protocol":1,"method":"echo","params":"`+strings.Repeat("a", maxBodyBytes)+`"}`)
	}()
	resp := readConnResponse(t, conn)
	conn.Close()
	ran.Wait()
	if !strings.HasPrefix(resp.Error, "malformed request:") {
		t.Errorf("error = %q, want malformed request", resp.Error)
	}
	if called.Load() {
		t.Error("handler ran for an oversized request")
	}
}

// TestCallResultErrors covers responses the client cannot use: one over
// maxBodyBytes, a result the server cannot encode, and one that does not
// fit the caller's type.
func TestCallResultErrors(t *testing.T) {
	sock := startServer(t, map[string]Handler{
		"huge":   func(context.Context, json.RawMessage) (any, error) { return strings.Repeat("a", maxBodyBytes), nil },
		"chan":   func(context.Context, json.RawMessage) (any, error) { return make(chan int), nil },
		"number": func(context.Context, json.RawMessage) (any, error) { return 7, nil },
	})
	tests := []struct {
		method string
		out    any
		want   string
	}{
		{"huge", new(string), "read response:"},
		{"chan", nil, "encode result:"},
		{"number", new(string), "decode result:"},
	}
	for _, tt := range tests {
		err := Call(context.Background(), sock, tt.method, nil, tt.out)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want prefix %q", tt.method, err, tt.want)
		}
	}
	if err := Call(context.Background(), sock, "number", make(chan int), nil); err == nil || !strings.HasPrefix(err.Error(), "encode params:") {
		t.Errorf("unencodable params: err = %v", err)
	}
}

// TestCallRefusesUnsafeSocketDir checks that the client does not connect
// through a directory another user could have replaced the socket in, and
// does not report it as a daemon that is simply down.
func TestCallRefusesUnsafeSocketDir(t *testing.T) {
	// A short path: t.TempDir() can exceed the socket path limit on macOS.
	base, err := os.MkdirTemp("", "tr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"g": 0o770, "w": 0o757} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			sock := filepath.Join(dir, "s")
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			accepted := make(chan struct{}, 1)
			go func() {
				if c, err := l.Accept(); err == nil {
					accepted <- struct{}{}
					c.Close()
				}
			}()
			err = Call(context.Background(), sock, "ping", nil, nil)
			var nr *NotRunningError
			if err == nil || !strings.Contains(err.Error(), "writable by other users") || errors.As(err, &nr) {
				t.Fatalf("err = %v, want an unsafe directory error", err)
			}
			l.Close()
			select {
			case <-accepted:
				t.Error("client connected through an unsafe directory")
			default:
			}
		})
	}
	if err := CheckPrivateDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("regular file: err = %v", err)
	}
}

// fakeListener returns the queued errors from Accept, then blocks until
// closed.
type fakeListener struct {
	net.Listener
	mu     sync.Mutex
	errs   []error
	closed chan struct{}
	once   sync.Once
}

func (l *fakeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

// TestServeAcceptErrors checks that a transient accept error (such as
// EMFILE) does not stop the server, while a listener closed out from under
// it does, with an error.
func TestServeAcceptErrors(t *testing.T) {
	s := NewServer(nil)
	l := &fakeListener{errs: []error{errors.New("too many open files"), net.ErrClosed}, closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("Serve kept accepting on a closed listener")
	}
	cancel()
	if err == nil || !errors.Is(err, net.ErrClosed) || !strings.HasPrefix(err.Error(), "accept:") {
		t.Fatalf("Serve = %v, want accept: closed", err)
	}
	if len(l.errs) != 0 {
		t.Errorf("Serve stopped after the transient error; %d left", len(l.errs))
	}

	// Cancelling ctx is a clean stop, even though Accept then fails.
	l = &fakeListener{closed: make(chan struct{})}
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- s.Serve(ctx, l) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop on cancel")
	}
}
