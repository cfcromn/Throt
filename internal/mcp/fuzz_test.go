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

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// fuzzServe runs in through Serve and returns the output lines, failing on
// any line that is not a valid JSON-RPC 2.0 message.
func fuzzServe(t *testing.T, in []byte) [][]byte {
	t.Helper()
	ts := time.Date(2026, 10, 28, 14, 2, 52, 0, time.UTC)
	c := &fakeClient{
		status: sampleStatus(domain.PressureHigh, scheduler.ModeEnforce, 3),
		events: []domain.Event{{Time: ts, Kind: domain.EventPressureChanged, Message: "pressure elevated -> high"}},
	}
	var out bytes.Buffer
	s := New(c, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Serve(context.Background(), bytes.NewReader(in), &out); err != nil {
		t.Fatalf("Serve(%q): %v", in, err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var m message
		if !json.Valid(line) || !utf8.Valid(line) || json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
			t.Fatalf("input %q: output line is not a JSON-RPC message: %q", in, line)
		}
		if (m.Error == nil) == (m.Result == nil) {
			t.Fatalf("input %q: output needs exactly one of result and error: %q", in, line)
		}
		lines = append(lines, bytes.Clone(line))
	}
	return lines
}

func FuzzServe(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":[]}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"a","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":4,`,
		`[{"jsonrpc":"2.0","id":5,"method":"ping"}]`,
		`{"jsonrpc":"1.0","id":6,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"throt_pause"}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":[1]}`,
		`{"jsonrpc":"2.0","id":9,"result":{}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"throt_status"}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"throt_capacity","arguments":{"agent":"claude-1"}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"throt_events","arguments":{"limit":1}}}`,
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"throt_events","arguments":{"limit":0}}}`,
		`{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"throt_stats"}}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","ID":14,"method":"ping"}`,
		"{\"jsonrpc\":\"2.0\",\"id\":\"\xff\",\"method\":\"ping\"}",
		`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
		"\n\r\n",
	} {
		f.Add([]byte(s + "\n"))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		lines := fuzzServe(t, in)
		if n := bytes.Count(in, []byte("\n")) + 1; len(lines) > n {
			t.Fatalf("input %q: %d responses to %d lines", in, len(lines), n)
		}
		// A well-formed request object without an "id" member is a
		// notification and must never be answered. (Malformed ones are
		// Invalid Requests, which JSON-RPC answers with a null id.)
		for line := range bytes.Lines(in) {
			var obj map[string]json.RawMessage
			if json.Unmarshal(line, &obj) != nil || obj == nil {
				continue
			}
			var version, method string
			if _, ok := obj["id"]; ok || json.Unmarshal(obj["jsonrpc"], &version) != nil || version != "2.0" ||
				json.Unmarshal(obj["method"], &method) != nil {
				continue
			}
			if p, ok := obj["params"]; ok && p[0] != '{' && p[0] != '[' {
				continue
			}
			if out := fuzzServe(t, line); len(out) != 0 {
				t.Fatalf("notification %q answered: %q", line, out)
			}
		}
	})
}

func FuzzNotification(f *testing.F) {
	f.Add("notifications/initialized", "")
	f.Add("notifications/cancelled", `{"requestId":1}`)
	f.Add("tools/call", `{"name":"throt_status"}`)
	f.Add("initialize", `[]`)
	f.Fuzz(func(t *testing.T, method, params string) {
		msg := map[string]any{"jsonrpc": "2.0", "method": method}
		if json.Valid([]byte(params)) {
			msg["params"] = json.RawMessage(params)
		}
		b, err := json.Marshal(msg)
		if err != nil {
			t.Skip()
		}
		if strings.Contains(string(b), "\n") {
			t.Fatalf("encoded notification has a newline: %q", b)
		}
		if out := fuzzServe(t, append(b, '\n')); len(out) != 0 {
			t.Fatalf("notification %q answered: %q", b, out)
		}
	})
}
