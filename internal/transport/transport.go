// Package transport implements Throt's local protocol: one JSON request and
// one JSON response per connection over a Unix domain socket.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ProtocolVersion is bumped on any incompatible change to requests or results.
const ProtocolVersion = 1

const (
	maxConns     = 16
	ioTimeout    = 5 * time.Second
	maxBodyBytes = 1 << 20
)

// Request is sent by a client.
type Request struct {
	Protocol int             `json:"protocol"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Response is returned by the daemon. Exactly one of Result or Error is set.
type Response struct {
	Protocol int             `json:"protocol"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Handler serves one method. The returned value is JSON-encoded as Result.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Server dispatches requests to registered handlers.
type Server struct {
	handlers map[string]Handler
}

// NewServer returns a server for the given method table.
func NewServer(handlers map[string]Handler) *Server {
	return &Server{handlers: handlers}
}

// Serve accepts connections until ctx is cancelled, then closes l and waits
// for in-flight requests. At most maxConns requests are handled at once;
// further connections wait in the kernel backlog.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()

	sem := make(chan struct{}, maxConns)
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		conn, err := l.Accept()
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("accept: %w", err)
			}
			// ponytail: fixed backoff on transient errors (e.g. EMFILE); fail open rather than exit.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ioTimeout))

	var req Request
	resp := Response{Protocol: ProtocolVersion}
	if err := json.NewDecoder(limitReader(conn)).Decode(&req); err != nil {
		resp.Error = "malformed request: " + err.Error()
	} else if req.Protocol != ProtocolVersion {
		resp.Error = fmt.Sprintf("protocol mismatch: client speaks v%d, daemon speaks v%d; upgrade throt and throtd to the same release", req.Protocol, ProtocolVersion)
	} else if h, ok := s.handlers[req.Method]; !ok {
		resp.Error = "unknown method: " + req.Method
	} else if v, err := h(ctx, req.Params); err != nil {
		resp.Error = err.Error()
	} else if resp.Result, err = json.Marshal(v); err != nil {
		resp.Error = "encode result: " + err.Error()
	}
	json.NewEncoder(conn).Encode(resp)
}

// Call sends one request to the daemon at socket and decodes the result into out
// (which may be nil).
func Call(ctx context.Context, socket, method string, params, out any) error {
	if err := CheckPrivateDir(filepath.Dir(socket)); err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("connect to throtd at %s (is it running?): %w", socket, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(ioTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)

	req := Request{Protocol: ProtocolVersion, Method: method}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return fmt.Errorf("encode params: %w", err)
		}
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(limitReader(conn)).Decode(&resp); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

func limitReader(r io.Reader) io.Reader { return io.LimitReader(r, maxBodyBytes) }

// CheckPrivateDir rejects a socket directory that is a symlink, is owned by
// another user, or is writable by group/other: any of those would let another
// local user replace the socket. Both daemon and client call it.
func CheckPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory (symlinks are not allowed)", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket dir %s is owned by uid %d, not the current user", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("socket dir %s is writable by other users (mode %v); use a private directory", dir, fi.Mode().Perm())
	}
	return nil
}
