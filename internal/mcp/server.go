// Package mcp implements `throt mcp`, a read-only Model Context Protocol
// server over stdio. Coding agents use it to check machine pressure and
// heavy-workload capacity before starting expensive work. It is a client of
// throtd like the TUI: it never pauses, resumes, or reconfigures anything.
//
// The transport is newline-delimited JSON-RPC 2.0 on stdin/stdout. stdout
// carries protocol messages only; logs go to the logger, which should write
// to stderr.
//
// The server is dual-era: it answers the stateless 2026-07-28 revision,
// where each request names its version in _meta, and the handshake-based
// revisions that open with initialize, on the same stream.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// Client is the daemon API the MCP server needs.
type Client interface {
	Status(ctx context.Context) (app.StatusResult, error)
	Events(ctx context.Context, limit int) ([]domain.Event, error)
	Stats(ctx context.Context) (app.StatsResult, error)
}

const (
	// maxLine bounds one input message, excluding its newline.
	maxLine = 1 << 20
	// callTimeout bounds one daemon call made by a tool.
	callTimeout = 5 * time.Second
	// maxWaits bounds the throt_wait_for_capacity calls in flight at once.
	maxWaits = 4
)

// Protocol revisions. modernVersion is stateless: every request names its
// version in _meta and there is no initialize handshake. The legacy
// revisions open with initialize.
const (
	modernVersion = "2026-07-28"
	latestLegacy  = "2025-11-25"
)

// Reserved _meta keys of the modern revision.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// toolsTTL is the freshness hint for tools/list and server/discover: the
// tool list changes only with the binary, which restarts the server.
const toolsTTL = time.Hour

// legacyVersion reports whether v is a handshake-based MCP revision this
// server speaks.
func legacyVersion(v string) bool {
	switch v {
	// 2025-03-26 is omitted: it requires JSON-RPC batching, which is not
	// supported.
	case latestLegacy, "2025-06-18", "2024-11-05":
		return true
	}
	return false
}

// supportedVersions lists every revision this server speaks, newest first.
func supportedVersions() []string {
	return []string{modernVersion, latestLegacy, "2025-06-18", "2024-11-05"}
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	// codeUnsupportedVersion is the modern UnsupportedProtocolVersionError.
	codeUnsupportedVersion = -32022
)

// clock is the time source of throt_wait_for_capacity.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Server answers MCP requests using a daemon client.
type Server struct {
	client  Client
	version string
	log     *slog.Logger
	// parent is the PID of the process that launched the server, normally
	// the agent itself; it is the default agent for throt_capacity.
	parent int
	clock  clock
	// callDone, when set, is called after a background call ends, with its
	// request id and whether it stopped early (cancelled, or the stream
	// ended). Tests use it.
	callDone func(id string, stopped bool)
}

// New returns a server that reports version as its serverInfo version and
// logs to log.
func New(client Client, version string, log *slog.Logger) *Server {
	return &Server{client: client, version: version, log: log, parent: os.Getppid(), clock: realClock{}}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in and writes responses to out, one per line,
// until in reaches EOF (a clean exit, returning nil), ctx is cancelled, or
// a read or write fails. Requests are handled one at a time, in order,
// except throt_wait_for_capacity: it runs in the background so that the
// client can cancel it, and its response may follow later ones. When Serve
// returns, background calls are stopped and get no response.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	ss := &stream{out: out, inflight: map[string]*inflight{}}
	defer func() {
		cancel()
		ss.wg.Wait()
	}()
	br := bufio.NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ss.err(); err != nil {
			return err
		}
		line, tooLong, rerr := readLine(br)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("read: %w", rerr)
		}
		var resp *response
		switch {
		case tooLong:
			s.log.Warn("dropped oversize message", "limit_bytes", maxLine)
			resp = errorResponse(nil, codeParseError, fmt.Sprintf("message exceeds %d bytes", maxLine))
		case len(line) > 0:
			resp = s.handle(ctx, ss, line)
		}
		if resp != nil {
			if err := ss.write(resp); err != nil {
				return err
			}
		}
		if rerr != nil {
			return nil // EOF
		}
	}
}

// stream is the state of one Serve call: serialized output and the
// background calls in flight.
type stream struct {
	wg sync.WaitGroup
	mu sync.Mutex
	// out, werr (the first write error), and inflight are guarded by mu.
	out      io.Writer
	werr     error
	inflight map[string]*inflight
}

type inflight struct {
	cancel    context.CancelFunc
	cancelled bool
}

func (ss *stream) write(resp *response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.werr != nil {
		return ss.werr
	}
	if _, err := ss.out.Write(append(b, '\n')); err != nil {
		ss.werr = fmt.Errorf("write: %w", err)
	}
	return ss.werr
}

func (ss *stream) err() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.werr
}

// cancel handles notifications/cancelled and reports the id it cancelled.
// Unknown, finished, and malformed references are ignored, as the spec
// requires.
func (ss *stream) cancel(params json.RawMessage) (string, bool) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return "", false
	}
	id := idKey(p.RequestID)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	c, ok := ss.inflight[id]
	if !ok || c.cancelled {
		return id, false
	}
	c.cancelled = true
	c.cancel()
	return id, true
}

// idKey normalizes a JSON-RPC id for lookup.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, id) != nil {
		return string(id)
	}
	return b.String()
}

// deferred is a tool call that runs in the background; it returns the
// tool result.
type deferred func(ctx context.Context) toolResult

// background runs call on its own goroutine and writes its response, unless
// it was cancelled or the stream ended first. It refuses, with a tool error,
// when maxWaits calls are already in flight.
func (s *Server) background(ctx context.Context, ss *stream, req request, call deferred, finish func(any) *response) *response {
	id := idKey(req.ID)
	ss.mu.Lock()
	if _, dup := ss.inflight[id]; dup {
		ss.mu.Unlock()
		return errorResponse(req.ID, codeInvalidRequest, "invalid request: id "+id+" is already used by a request in progress")
	}
	if len(ss.inflight) >= maxWaits {
		ss.mu.Unlock()
		return finish(errorResult(fmt.Sprintf("too many waits in progress (at most %d); "+
			"wait for one to return, or call throt_capacity instead", maxWaits)))
	}
	cctx, cancel := context.WithCancel(ctx)
	ss.inflight[id] = &inflight{cancel: cancel}
	ss.wg.Add(1)
	ss.mu.Unlock()
	go func() {
		defer ss.wg.Done()
		defer cancel()
		result := call(cctx)
		ss.mu.Lock()
		delete(ss.inflight, id)
		ss.mu.Unlock()
		// A cancelled call gets no response; one stopped because the
		// stream ended has nowhere to send it.
		stopped := cctx.Err() != nil
		if !stopped {
			if err := ss.write(finish(result)); err != nil {
				s.log.Debug("dropped background response", "err", err)
			}
		}
		if s.callDone != nil {
			s.callDone(id, stopped)
		}
	}()
	return nil
}

// readLine returns the next line without its line ending. A line longer
// than maxLine is consumed and reported as tooLong, without buffering it.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxLine+2 { // allow for "\r\n"
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if !tooLong && len(line) > maxLine {
			tooLong, line = true, nil
		}
		return line, tooLong, err
	}
}

// handle processes one message and returns its response, or nil for
// notifications, for responses sent by the client, and for calls that
// answer in the background.
func (s *Server) handle(ctx context.Context, ss *stream, line []byte) *response {
	if !json.Valid(line) {
		return errorResponse(nil, codeParseError, "parse error: invalid JSON")
	}
	req, ok := decodeRequest(line)
	if !ok {
		// Valid JSON that is not a request object, e.g. a batch array.
		return errorResponse(nil, codeInvalidRequest, "invalid request: expected a JSON-RPC request object")
	}
	if len(req.ID) == 0 {
		// Notifications never get a response.
		switch req.Method {
		case "notifications/initialized":
		case "notifications/cancelled":
			if id, ok := ss.cancel(req.Params); ok {
				s.log.Debug("cancelled request", "id", id)
			}
		default:
			s.log.Debug("ignored notification", "method", req.Method)
		}
		return nil
	}
	if req.Method == "" {
		// A response to a request we never send; nothing to answer.
		s.log.Debug("ignored message without method")
		return nil
	}
	if !validID(req.ID) {
		// It cannot be echoed: it may not even be valid UTF-8.
		return errorResponse(nil, codeInvalidRequest, "invalid request: id must be a string, number, or null")
	}
	if req.JSONRPC != "2.0" {
		return errorResponse(req.ID, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	modern, rerr := era(req)
	if rerr != nil {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: rerr}
	}
	finish := func(result any) *response {
		if modern {
			result = s.modernResult(result)
		}
		return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	result, rerr := s.dispatch(ctx, req, modern)
	if rerr != nil {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: rerr}
	}
	if call, ok := result.(deferred); ok {
		return s.background(ctx, ss, req, call, finish)
	}
	return finish(result)
}

// era reports whether req uses the modern revision, which it does when its
// params carry _meta with a protocol version. initialize always selects the
// legacy handshake. A modern request must name a supported version and
// declare client capabilities.
func era(req request) (modern bool, rerr *rpcError) {
	if req.Method == "initialize" {
		return false, nil
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	// Params that are not an object, or a _meta that is not one, carry no
	// modern metadata; dispatch validates the params themselves.
	_ = json.Unmarshal(req.Params, &p)
	rawVersion, ok := p.Meta[metaProtocolVersion]
	if !ok {
		if req.Method == "server/discover" {
			return false, &rpcError{Code: codeInvalidParams, Message: "invalid params: server/discover requires _meta keys " +
				metaProtocolVersion + " and " + metaClientCapabilities}
		}
		return false, nil
	}
	var version string
	if json.Unmarshal(rawVersion, &version) != nil {
		return true, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta " + metaProtocolVersion + " must be a string"}
	}
	if version != modernVersion {
		msg := "Unsupported protocol version"
		if legacyVersion(version) {
			msg += " in _meta: " + version + " starts with initialize"
		}
		return true, &rpcError{Code: codeUnsupportedVersion, Message: msg,
			Data: map[string]any{"supported": supportedVersions(), "requested": version}}
	}
	if caps := p.Meta[metaClientCapabilities]; len(caps) == 0 || caps[0] != '{' {
		return true, &rpcError{Code: codeInvalidParams, Message: "invalid params: _meta " + metaClientCapabilities + " must be an object"}
	}
	return true, nil
}

// modernResult adds the members every modern result carries: resultType,
// and the server's identity in _meta.
func (s *Server) modernResult(result any) any {
	var m map[string]any
	if b, err := json.Marshal(result); err != nil || json.Unmarshal(b, &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["resultType"] = "complete"
	meta, _ := m["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaServerInfo] = s.serverInfo()
	m["_meta"] = meta
	return m
}

func (s *Server) serverInfo() map[string]string {
	return map[string]string{"name": "throt", "version": s.version}
}

// decodeRequest decodes a request object. JSON-RPC member names are
// case-sensitive, unlike encoding/json's struct field matching: {"ID":1} has
// no id and is a notification.
func decodeRequest(line []byte) (request, bool) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil || members == nil {
		return request{}, false
	}
	req := request{ID: members["id"], Params: members["params"]}
	for name, dst := range map[string]*string{"jsonrpc": &req.JSONRPC, "method": &req.Method} {
		if v, ok := members[name]; ok && json.Unmarshal(v, dst) != nil {
			return request{}, false
		}
	}
	return req, true
}

// validID reports whether id, which is valid JSON, is a JSON-RPC id: a
// string, number, or null. A string must be valid UTF-8 to be echoed.
func validID(id json.RawMessage) bool {
	switch id[0] {
	case '"':
		return utf8.Valid(id)
	case 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

// errorResponse builds an error reply; a nil id is sent as null.
func errorResponse(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) dispatch(ctx context.Context, req request, modern bool) (any, *rpcError) {
	switch {
	case req.Method == "initialize":
		return s.initialize(req.Params)
	case req.Method == "ping" && !modern:
		// The modern revision removed ping.
		return struct{}{}, nil
	case req.Method == "server/discover":
		return discover(), nil
	case req.Method == "tools/list":
		r := map[string]any{"tools": toolList()}
		if modern {
			r["ttlMs"], r["cacheScope"] = toolsTTL.Milliseconds(), "public"
		}
		return r, nil
	case req.Method == "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// instructions is the server's guidance for models, sent by initialize and
// server/discover.
const instructions = "Throt reports local machine pressure and heavy-workload capacity. " +
	"Call throt_capacity before starting a heavy build, test suite, or container build; " +
	"if ok_to_start is false, call throt_wait_for_capacity to wait for a slot, or run a lighter task. " +
	"Call throt_explain with a PID from throt_status to learn why a heavy job is paused or queued. " +
	"All tools are read-only and advisory."

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// initialize echoes the client's protocol version when it is a legacy
// revision this server speaks and otherwise offers the newest legacy one.
// initialize is the legacy handshake, so it never selects the modern
// revision.
func (s *Server) initialize(raw json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	version := latestLegacy
	if legacyVersion(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      s.serverInfo(),
		"instructions":    instructions,
	}, nil
}

// discover answers the modern server/discover. It lists the legacy
// revisions too, which a client reaches with initialize.
func discover() any {
	return map[string]any{
		"supportedVersions": supportedVersions(),
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions":      instructions,
		"ttlMs":             toolsTTL.Milliseconds(),
		"cacheScope":        "public",
	}
}
