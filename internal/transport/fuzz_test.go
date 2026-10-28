package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
	"unicode/utf8"
)

// bufConn is a net.Conn that reads a fixed request and records the reply.
// Methods handle does not use are left to the nil embedded interface.
type bufConn struct {
	net.Conn
	in  *bytes.Reader
	out bytes.Buffer
}

func (c *bufConn) Read(b []byte) (int, error)       { return c.in.Read(b) }
func (c *bufConn) Write(b []byte) (int, error)      { return c.out.Write(b) }
func (c *bufConn) Close() error                     { return nil }
func (c *bufConn) SetDeadline(time.Time) error      { return nil }
func (c *bufConn) SetReadDeadline(time.Time) error  { return nil }
func (c *bufConn) SetWriteDeadline(time.Time) error { return nil }

func FuzzHandle(f *testing.F) {
	for _, s := range []string{
		`{"protocol":1,"method":"echo","params":"hi"}`,
		`{"protocol":1,"method":"echo","params":{"a":[1,2.5,null,true]}}`,
		`{"protocol":1,"method":"fail"}`,
		`{"protocol":1,"method":"nope"}`,
		`{"protocol":1,"method":"pause","params":{"pid":10}}`,
		`{"protocol":2,"method":"ping"}`,
		`{"protocol":1,"method":"ping"}{"protocol":1}`,
		`{"protocol":"1"}`,
		`null`, ``, `garbage`, `[`,
	} {
		f.Add([]byte(s + "\n"))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		s := NewServer(map[string]Handler{
			"ping": func(context.Context, json.RawMessage) (any, error) { return "pong", nil },
			"echo": func(_ context.Context, p json.RawMessage) (any, error) {
				var v any
				err := json.Unmarshal(p, &v)
				return v, err
			},
			"fail": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") },
			"pause": func(context.Context, json.RawMessage) (any, error) {
				t.Errorf("request %q: privileged method ran without a verified caller", in)
				return nil, nil
			},
		}, WithPrivileged("pause"))
		s.peerUID = func(net.Conn) (uint32, error) { return 0, errors.New("no peer credentials") }
		conn := &bufConn{in: bytes.NewReader(in)}
		s.handle(context.Background(), conn)
		out := conn.out.Bytes()
		line, rest, ok := bytes.Cut(out, []byte("\n"))
		if !ok || len(rest) != 0 {
			t.Fatalf("request %q: want one response line, got %q", in, out)
		}
		var resp Response
		if !json.Valid(line) || !utf8.Valid(line) || json.Unmarshal(line, &resp) != nil {
			t.Fatalf("request %q: response is not valid JSON: %q", in, line)
		}
		if resp.Protocol != ProtocolVersion || (resp.Error == "") == (resp.Result == nil) {
			t.Fatalf("request %q: response %q needs the protocol and exactly one of result and error", in, line)
		}
	})
}
