package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cfcromn/Throt/internal/app"
)

var fuzzToolNames = []string{
	"throt_status", "throt_capacity", "throt_events", "throt_stats", "throt_explain", "throt_wait_for_capacity",
}

// FuzzToolArguments calls each tool with arbitrary arguments against a
// daemon that never has capacity, on a clock that advances by each sleep.
// A call must return, its result must encode, and a wait must end within
// maxWait of clock time and at most one check per second of it.
func FuzzToolArguments(f *testing.F) {
	for i := range fuzzToolNames {
		for _, a := range []string{
			``, `null`, `{}`, `[]`, `"x"`, `{"x":1}`,
			`{"agent":"claude-1"}`, `{"agent":""}`, `{"agent":1}`,
			`{"limit":1}`, `{"limit":0}`, `{"limit":200}`, `{"limit":201}`, `{"limit":-1}`, `{"limit":1e2}`, `{"limit":9223372036854775807}`,
			`{"pid":101}`, `{"pid":0}`, `{"pid":-1}`, `{"pid":2147483648}`, `{"pid":"101"}`, `{"pid":null}`,
			`{"max_wait_seconds":1}`, `{"max_wait_seconds":600}`, `{"max_wait_seconds":601}`, `{"max_wait_seconds":0}`,
			`{"max_wait_seconds":-9223372036854775808}`, `{"max_wait_seconds":9223372036854775807}`, `{"max_wait_seconds":1.5}`,
			`{"agent":"x","max_wait_seconds":2}`, `{"agent":"x","agent":"y"}`, `{"Agent":"x"}`,
		} {
			f.Add(uint8(i), a)
		}
	}
	f.Add(uint8(255), `{}`)
	f.Fuzz(func(t *testing.T, which uint8, args string) {
		name := "throt_unknown"
		if int(which) < len(fuzzToolNames) {
			name = fuzzToolNames[which]
		}
		params := map[string]any{"name": name}
		if json.Valid([]byte(args)) {
			params["arguments"] = json.RawMessage(args)
		}
		raw, err := json.Marshal(params)
		if err != nil {
			t.Skip()
		}
		clk := &fakeClock{now: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), auto: true}
		c := &seqClient{statuses: []app.StatusResult{busy()}}
		s := New(c, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.clock = clk
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := clk.Now()
		result, rerr := s.callTool(ctx, raw)
		if call, ok := result.(deferred); ok {
			result = call(ctx)
			waited := clk.Now().Sub(start)
			if waited > maxWait {
				t.Fatalf("%s %s: waited %v, over %v", name, args, waited, maxWait)
			}
			if n := c.count(); time.Duration(n-1)*time.Second > waited {
				t.Fatalf("%s %s: %d checks in %v", name, args, n, waited)
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("%s %s: did not return within the test deadline", name, args)
		}
		if (result == nil) == (rerr == nil) {
			t.Fatalf("%s %s: want exactly one of a result (%v) and an error (%v)", name, args, result, rerr)
		}
		if rerr == nil {
			b, err := json.Marshal(result)
			if err != nil || !utf8.Valid(b) {
				t.Fatalf("%s %s: result does not encode: %v", name, args, err)
			}
		}
	})
}

// FuzzReadLine checks the framing: whatever the bytes and however small
// the reader's buffer, readLine returns the input's lines, without their
// line endings, and flags exactly the ones over maxLine.
func FuzzReadLine(f *testing.F) {
	for _, s := range []string{
		"", "\n", "\r\n", "a", "a\nb", "a\r\nb\r\n", "\r\r\n", "{\"jsonrpc\":\"2.0\"}\n\n", "a\rb\n", "\xff\n",
	} {
		f.Add([]byte(s), uint8(0), uint16(0))
	}
	f.Add([]byte("x"), uint8(3), uint16(1))
	f.Add([]byte("\r\n"), uint8(16), uint16(3))
	f.Fuzz(func(t *testing.T, in []byte, bufSize uint8, longAt uint16) {
		// Splice in a line around maxLine to exercise the limit.
		if longAt > 0 {
			at := int(longAt) % (len(in) + 1)
			long := bytes.Repeat([]byte("y"), maxLine-1+int(longAt%4))
			in = append(append(append(bytes.Clone(in[:at]), '\n'), long...), append([]byte("\n"), in[at:]...)...)
		}
		br := bufio.NewReaderSize(bytes.NewReader(in), 16+int(bufSize))
		want := strings.Split(string(in), "\n")
		for i, w := range want {
			line, tooLong, err := readLine(br)
			w = strings.TrimRight(w, "\r")
			last := i == len(want)-1
			if (err != nil) != last {
				t.Fatalf("line %d: err = %v", i, err)
			}
			if wantLong := len(w) > maxLine; tooLong != wantLong {
				t.Fatalf("line %d (%d bytes): tooLong = %v", i, len(w), tooLong)
			}
			if !tooLong && string(line) != w {
				t.Fatalf("line %d: got %q, want %q", i, line, w)
			}
			if tooLong && line != nil {
				t.Fatalf("line %d: a long line returned %d bytes", i, len(line))
			}
		}
	})
}
