package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
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

// FuzzHandleOversized sends requests around maxBodyBytes: head, then fill
// repeated to about the limit, then tail. Whatever the daemon answers must
// fit in what the client reads, so the client sees the daemon's error
// rather than a truncated reply.
func FuzzHandleOversized(f *testing.F) {
	f.Add([]byte(`{"protocol":1,"method":"`), []byte("<"), []byte(`"}`), uint16(0))
	f.Add([]byte(`{"protocol":1,"method":"`), []byte(`\u0000`), []byte(`"}`), uint16(100))
	f.Add([]byte(`{"protocol":1,"method":"ping","params":"`), []byte("a"), []byte(`"}`), uint16(200))
	f.Add([]byte(`{"protocol":1,"method":"ping","params":[`), []byte("1,"), []byte(`1]}`), uint16(64))
	f.Add([]byte(`{"protocol":1,"method":"fail"`), []byte(" "), []byte(`}`), uint16(65))
	f.Add([]byte(`{"protocol":1,"method":"ping","x":`), []byte("["), []byte(``), uint16(1))
	f.Add([]byte(``), []byte("\xff"), []byte(``), uint16(3))
	f.Fuzz(func(t *testing.T, head, fill, tail []byte, n uint16) {
		if len(fill) == 0 {
			fill = []byte(" ")
		}
		// From 128 bytes under the limit to 128 over it.
		size := maxBodyBytes - 128 + int(n%256)
		in := bytes.Clone(head)
		for len(in) < size {
			in = append(in, fill...)
		}
		in = append(in, tail...)
		s := NewServer(map[string]Handler{
			"ping": func(context.Context, json.RawMessage) (any, error) { return "pong", nil },
			"fail": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") },
		})
		conn := &bufConn{in: bytes.NewReader(in)}
		s.handle(context.Background(), conn)
		out := conn.out.Bytes()
		if len(out) > maxBodyBytes {
			t.Fatalf("request of %d bytes (%q...): %d-byte response is over the client's %d-byte limit", len(in), in[:min(len(in), 64)], len(out), maxBodyBytes)
		}
		if bytes.Count(out, []byte("\n")) != 1 || out[len(out)-1] != '\n' {
			t.Fatalf("request of %d bytes: want one response line, got %q", len(in), out[:min(len(out), 256)])
		}
		var v any
		if err := readResponse(bytes.NewReader(out), &v); err != nil && strings.HasPrefix(err.Error(), "read response") {
			t.Fatalf("request of %d bytes: client cannot read the response: %v", len(in), err)
		}
	})
}

// endless yields data, then whitespace forever: a peer that never ends
// its response.
type endless struct{ data []byte }

func (e *endless) Read(b []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(b, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	for i := range b {
		b[i] = ' '
	}
	return len(b), nil
}

// FuzzReadResponse decodes arbitrary daemon replies in the client. It must
// never panic, never read past maxBodyBytes (so a peer that never stops
// cannot hang it), and succeed only on a reply without an error.
func FuzzReadResponse(f *testing.F) {
	for _, s := range []string{
		`{"protocol":1,"result":"pong"}`,
		`{"protocol":1,"result":{"a":[1,2]}}`,
		`{"protocol":1,"error":"boom"}`,
		`{"protocol":1,"error":""}`,
		`{"protocol":1}`,
		`{"protocol":"1","result":1}`,
		`{"protocol":1,"result":1}{"protocol":1,"error":"x"}`,
		`{"protocol":1,"result":`,
		`[]`, `null`, `"x"`, ``, "\xff",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, out := range []any{nil, new(any), new(Response), new(map[string]int), new(string)} {
			err := readResponse(bytes.NewReader(data), out)
			var resp Response
			if err == nil && json.NewDecoder(bytes.NewReader(data)).Decode(&resp) == nil && resp.Error != "" {
				t.Fatalf("reply %q: an error reply decoded as success", data)
			}
			lr := &countingReader{r: &endless{data: bytes.Clone(data)}}
			readResponse(lr, out)
			if lr.n > maxBodyBytes {
				t.Fatalf("reply %q: read %d bytes, over the %d-byte limit", data, lr.n, maxBodyBytes)
			}
		}
	})
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += n
	return n, err
}
