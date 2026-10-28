// Package transport implements Throt's local protocol: one JSON request and
// one JSON response per connection over a Unix domain socket.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	handlers   map[string]Handler
	privileged map[string]bool
	// peerUID reports the caller's uid; tests replace it to simulate a
	// foreign caller.
	peerUID func(net.Conn) (uint32, error)
	// peerPID reports the caller's PID; tests replace it.
	peerPID func(net.Conn) (int, error)
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithPrivileged marks methods that only the user running the daemon may
// call. Before such a handler runs, the server verifies the caller's uid with
// the kernel and refuses the request if it differs or cannot be read.
func WithPrivileged(methods ...string) ServerOption {
	return func(s *Server) {
		for _, m := range methods {
			s.privileged[m] = true
		}
	}
}

// NewServer returns a server for the given method table.
func NewServer(handlers map[string]Handler, opts ...ServerOption) *Server {
	s := &Server{handlers: handlers, privileged: make(map[string]bool), peerUID: PeerUID, peerPID: PeerPID}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// authorize permits a privileged method only for a caller with the daemon's
// own uid.
func (s *Server) authorize(conn net.Conn, method string) error {
	if !s.privileged[method] {
		return nil
	}
	uid, err := s.peerUID(conn)
	if err != nil {
		return fmt.Errorf("permission denied: cannot verify caller uid: %w", err)
	}
	if int64(uid) != int64(os.Getuid()) {
		return fmt.Errorf("permission denied: caller uid %d does not own throtd", uid)
	}
	return nil
}

// peerPIDKey is the context key of a privileged caller's PID.
type peerPIDKey struct{}

// WithPeerPID returns ctx carrying pid as the caller's kernel-verified PID.
// The server sets it for privileged methods; tests use it directly.
func WithPeerPID(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, peerPIDKey{}, pid)
}

// PeerPIDFrom returns the caller's PID as verified by the kernel. It is
// set only for privileged methods, and only where the OS reports it (Linux
// SO_PEERCRED, macOS LOCAL_PEERPID) for a peer in the daemon's PID
// namespace; otherwise ok is false and handlers must not rely on it.
func PeerPIDFrom(ctx context.Context) (pid int, ok bool) {
	pid, ok = ctx.Value(peerPIDKey{}).(int)
	return pid, ok
}

// withPeer adds the caller's PID to ctx for a privileged method, when the
// kernel reports one. A PID of 0 means the peer is in another PID namespace.
func (s *Server) withPeer(ctx context.Context, conn net.Conn, method string) context.Context {
	if !s.privileged[method] {
		return ctx
	}
	if pid, err := s.peerPID(conn); err == nil && pid > 0 {
		return WithPeerPID(ctx, pid)
	}
	return ctx
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
		resp.Error = "unknown method: " + clip(req.Method)
	} else if err := s.authorize(conn, req.Method); err != nil {
		resp.Error = err.Error()
	} else if v, err := h(s.withPeer(ctx, conn, req.Method), req.Params); err != nil {
		resp.Error = err.Error()
		if resp.Error == "" {
			// An empty Error reads as success on the client.
			resp.Error = req.Method + ": failed without a message"
		}
	} else if resp.Result, err = json.Marshal(v); err != nil {
		resp.Error = "encode result: " + err.Error()
	}
	json.NewEncoder(conn).Encode(resp)
}

// NotRunningError reports that no daemon listens on Socket: the socket or
// its directory does not exist, or nothing accepts connections on it (a
// daemon that exited without removing it). Err is the underlying error.
type NotRunningError struct {
	Socket string
	Err    error
}

func (e *NotRunningError) Error() string {
	what := "no socket at "
	if errors.Is(e.Err, syscall.ECONNREFUSED) {
		what = "nothing listening on "
	}
	return "throtd is not running (" + what + e.Socket + "); start it with `throtd`"
}

func (e *NotRunningError) Unwrap() error { return e.Err }

// Call sends one request to the daemon at socket and decodes the result into out
// (which may be nil).
func Call(ctx context.Context, socket, method string, params, out any) error {
	if err := CheckPrivateDir(filepath.Dir(socket)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &NotRunningError{Socket: socket, Err: err}
		}
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return &NotRunningError{Socket: socket, Err: err}
	}
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
	return readResponse(conn, out)
}

// readResponse decodes one response from r, at most maxBodyBytes, into out
// (which may be nil).
func readResponse(r io.Reader, out any) error {
	var resp Response
	if err := json.NewDecoder(limitReader(r)).Decode(&resp); err != nil {
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

// clip shortens a client-sent string echoed in an error. Echoed whole, a
// method name close to maxBodyBytes, once JSON-escaped, made a response
// the client could not read, so it saw a truncated reply instead.
func clip(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "..."
}

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
