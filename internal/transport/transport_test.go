package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func startServer(t *testing.T, handlers map[string]Handler) string {
	t.Helper()
	return serve(t, NewServer(handlers))
}

func serve(t *testing.T, s *Server) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return sock
}

func TestCall(t *testing.T) {
	sock := startServer(t, map[string]Handler{
		"echo": func(_ context.Context, p json.RawMessage) (any, error) {
			var s string
			err := json.Unmarshal(p, &s)
			return s, err
		},
		"fail": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") },
	})
	ctx := context.Background()

	var got string
	if err := Call(ctx, sock, "echo", "hi", &got); err != nil || got != "hi" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if err := Call(ctx, sock, "fail", nil, nil); err == nil || err.Error() != "boom" {
		t.Fatalf("fail err = %v", err)
	}
	if err := Call(ctx, sock, "nope", nil, nil); err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestProtocolMismatch(t *testing.T) {
	sock := startServer(t, map[string]Handler{})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	json.NewEncoder(conn).Encode(Request{Protocol: ProtocolVersion + 1, Method: "ping"})
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Error, "protocol mismatch") {
		t.Fatalf("error = %q", resp.Error)
	}
}

func TestCallNoDaemon(t *testing.T) {
	for _, sock := range []string{
		filepath.Join(t.TempDir(), "missing.sock"),
		filepath.Join(t.TempDir(), "no-dir", "missing.sock"),
	} {
		err := Call(context.Background(), sock, "ping", nil, nil)
		want := "throtd is not running (no socket at " + sock + "); start it with `throtd`"
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", sock, err, want)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: err = %v does not wrap fs.ErrNotExist", sock, err)
		}
	}
}

// TestCallStaleSocket checks a socket file nobody listens on: the daemon
// exited without removing it.
func TestCallStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "stale.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	err = Call(context.Background(), sock, "ping", nil, nil)
	want := "throtd is not running (nothing listening on " + sock + "); start it with `throtd`"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("err = %v does not wrap ECONNREFUSED", err)
	}
}

// TestEmptyHandlerError checks that a handler error without a message
// still reaches the client as an error.
func TestEmptyHandlerError(t *testing.T) {
	sock := startServer(t, map[string]Handler{
		"empty": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("") },
	})
	err := Call(context.Background(), sock, "empty", nil, nil)
	if err == nil || err.Error() != "empty: failed without a message" {
		t.Fatalf("err = %v, want a non-empty error", err)
	}
}

func TestCheckPrivateDir(t *testing.T) {
	base := t.TempDir()
	private := filepath.Join(base, "private")
	shared := filepath.Join(base, "shared")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(shared, 0o777)
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		dir     string
		wantErr string
	}{
		{private, ""},
		{shared, "writable by other users"},
		{link, "not a directory"},
		{filepath.Join(base, "missing"), "no such file"},
	}
	for _, tt := range tests {
		err := CheckPrivateDir(tt.dir)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tt.dir, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want %q", tt.dir, err, tt.wantErr)
		}
	}
}

// TestUnknownMethodClipped: an unknown method of nearly maxBodyBytes of
// "<" was echoed whole, and escaped as < the response grew to 6 MiB,
// which the client read as a truncated reply instead of the error.
func TestUnknownMethodClipped(t *testing.T) {
	req := `{"protocol":1,"method":"` + strings.Repeat("<", maxBodyBytes-64) + `"}`
	conn := &bufConn{in: bytes.NewReader([]byte(req))}
	NewServer(nil).handle(context.Background(), conn)
	if n := conn.out.Len(); n > 1024 {
		t.Fatalf("response is %d bytes", n)
	}
	err := readResponse(&conn.out, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "unknown method: <<<<") || !strings.HasSuffix(err.Error(), "...") {
		t.Fatalf("err = %v, want the clipped unknown method", err)
	}
}
