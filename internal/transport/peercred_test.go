package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func skipUnlessPeerCred(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS")
	}
}

func TestPeerUID(t *testing.T) {
	skipUnlessPeerCred(t)
	sock := filepath.Join(t.TempDir(), "p.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for name, c := range map[string]net.Conn{"server side": server, "client side": client} {
		uid, err := PeerUID(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if int64(uid) != int64(os.Getuid()) {
			t.Errorf("%s: uid = %d, want %d", name, uid, os.Getuid())
		}
	}
}

func TestPeerUIDNotUnix(t *testing.T) {
	skipUnlessPeerCred(t)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := PeerUID(a); err == nil {
		t.Fatal("PeerUID(net.Pipe) succeeded")
	}
}

func TestPrivilegedMethod(t *testing.T) {
	skipUnlessPeerCred(t)
	var calls atomic.Int32
	handlers := map[string]Handler{
		"pause": func(context.Context, json.RawMessage) (any, error) {
			calls.Add(1)
			return "paused", nil
		},
		"status": func(context.Context, json.RawMessage) (any, error) { return "ok", nil },
	}
	ctx := context.Background()

	t.Run("own", func(t *testing.T) {
		calls.Store(0)
		sock := serve(t, NewServer(handlers, WithPrivileged("pause")))
		var got string
		if err := Call(ctx, sock, "pause", nil, &got); err != nil || got != "paused" {
			t.Fatalf("pause = %q, %v", got, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("handler calls = %d, want 1", calls.Load())
		}
	})

	foreign := uint32(os.Getuid()) + 1
	for name, peer := range map[string]func(net.Conn) (uint32, error){
		"foreign": func(net.Conn) (uint32, error) { return foreign, nil },
		"unknown": func(net.Conn) (uint32, error) { return 0, errors.New("no creds") },
	} {
		t.Run(name, func(t *testing.T) {
			calls.Store(0)
			s := NewServer(handlers, WithPrivileged("pause"))
			s.peerUID = peer
			sock := serve(t, s)
			err := Call(ctx, sock, "pause", nil, nil)
			if err == nil || !strings.HasPrefix(err.Error(), "permission denied") {
				t.Fatalf("pause err = %v, want permission denied", err)
			}
			if name == "foreign" && !strings.Contains(err.Error(), "caller uid "+strconv.Itoa(int(foreign))) {
				t.Errorf("err = %v, want caller uid %d", err, foreign)
			}
			if calls.Load() != 0 {
				t.Fatalf("handler ran %d times for a denied caller", calls.Load())
			}
			// Non-privileged methods are unaffected.
			var got string
			if err := Call(ctx, sock, "status", nil, &got); err != nil || got != "ok" {
				t.Fatalf("status = %q, %v", got, err)
			}
		})
	}
}
