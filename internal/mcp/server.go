// Package mcp implements `throt mcp`, a read-only Model Context Protocol
// server over stdio. Coding agents use it to check machine pressure and
// heavy-workload capacity before starting expensive work. It is a client of
// throtd like the TUI: it never pauses, resumes, or reconfigures anything.
//
// The transport is newline-delimited JSON-RPC 2.0 on stdin/stdout. stdout
// carries protocol messages only; logs go to the logger, which should write
// to stderr.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// Client is the daemon API the MCP server needs.
type Client interface {
	Status(ctx context.Context) (app.StatusResult, error)
	Events(ctx context.Context, limit int) ([]domain.Event, error)
}

const (
	// maxLine bounds one input message, excluding its newline.
	maxLine = 1 << 20
	// callTimeout bounds one daemon call made by a tool.
	callTimeout = 5 * time.Second
)

// latestVersion is the newest MCP revision this server speaks. Revisions
// from 2026-07-28 on replace initialize with per-request negotiation and
// are not supported yet.
const latestVersion = "2025-11-25"

// supportedVersion reports whether v is a handshake-based MCP revision
// this server speaks.
func supportedVersion(v string) bool {
	switch v {
	// 2025-03-26 is omitted: it requires JSON-RPC batching, which is not
	// supported.
	case latestVersion, "2025-06-18", "2024-11-05":
		return true
	}
	return false
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Server answers MCP requests using a daemon client.
type Server struct {
	client  Client
	version string
	log     *slog.Logger
	// parent is the PID of the process that launched the server, normally
	// the agent itself; it is the default agent for throt_capacity.
	parent int
}

// New returns a server that reports version as its serverInfo version and
// logs to log.
func New(client Client, version string, log *slog.Logger) *Server {
	return &Server{client: client, version: version, log: log, parent: os.Getppid()}
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
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in and writes responses to out, one per line,
// until in reaches EOF (a clean exit, returning nil), ctx is cancelled, or
// a read or write fails. Requests are handled one at a time.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	br := bufio.NewReader(in)
	for {
		if err := ctx.Err(); err != nil {
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
			resp = s.handle(ctx, line)
		}
		if resp != nil {
			b, err := json.Marshal(resp)
			if err != nil {
				return fmt.Errorf("encode response: %w", err)
			}
			if _, err := out.Write(append(b, '\n')); err != nil {
				return fmt.Errorf("write: %w", err)
			}
		}
		if rerr != nil {
			return nil // EOF
		}
	}
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
// notifications and for responses sent by the client.
func (s *Server) handle(ctx context.Context, line []byte) *response {
	if !json.Valid(line) {
		return errorResponse(nil, codeParseError, "parse error: invalid JSON")
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		// Valid JSON that is not a request object, e.g. a batch array.
		return errorResponse(nil, codeInvalidRequest, "invalid request: expected a JSON-RPC request object")
	}
	if len(req.ID) == 0 {
		// Notifications (notifications/initialized, cancelled, ...) never
		// get a response.
		if req.Method != "notifications/initialized" {
			s.log.Debug("ignored notification", "method", req.Method)
		}
		return nil
	}
	if req.Method == "" {
		// A response to a request we never send; nothing to answer.
		s.log.Debug("ignored message without method")
		return nil
	}
	if req.JSONRPC != "2.0" {
		return errorResponse(req.ID, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	result, rerr := s.dispatch(ctx, req)
	if rerr != nil {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: rerr}
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

// errorResponse builds an error reply; a nil id is sent as null.
func errorResponse(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// initialize echoes the client's protocol version when supported and
// otherwise offers the newest version this server speaks.
func (s *Server) initialize(raw json.RawMessage) (any, *rpcError) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	version := latestVersion
	if supportedVersion(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": "throt", "version": s.version},
		"instructions": "Throt reports local machine pressure and heavy-workload capacity. " +
			"Call throt_capacity before starting a heavy build, test suite, or container build; " +
			"if ok_to_start is false, wait or run a lighter task. All tools are read-only and advisory.",
	}, nil
}
