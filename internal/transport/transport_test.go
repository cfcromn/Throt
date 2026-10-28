package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func startServer(t *testing.T, handlers map[string]Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(handlers).Serve(ctx, l) }()
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
	err := Call(context.Background(), filepath.Join(t.TempDir(), "missing.sock"), "ping", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "is it running") {
		t.Fatalf("err = %v", err)
	}
}
