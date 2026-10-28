package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// fakeClock advances by each After duration at once when auto is set;
// otherwise After never fires and reports each call on blocked.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	auto    bool
	blocked chan time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	ch := make(chan time.Time, 1)
	if c.auto {
		c.now = c.now.Add(d)
		ch <- c.now
	} else {
		c.blocked <- d
	}
	return ch
}

// seqClient returns statuses in order, repeating the last one, and fails
// from call failAt on (1-based; 0 never fails).
type seqClient struct {
	fakeClient
	mu       sync.Mutex
	statuses []app.StatusResult
	calls    int
	failAt   int
}

func (c *seqClient) Status(context.Context) (app.StatusResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.failAt > 0 && c.calls >= c.failAt {
		return app.StatusResult{}, errors.New("connect to throtd (is it running?): refused")
	}
	return c.statuses[min(c.calls, len(c.statuses))-1], nil
}

func (c *seqClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func busy() app.StatusResult { return sampleStatus(domain.PressureHigh, scheduler.ModeEnforce, 3) }
func spare() app.StatusResult {
	st := sampleStatus(domain.PressureNormal, scheduler.ModeOff, 3)
	st.Scheduler.Queued = nil
	return st
}

// session drives Serve over pipes, so a background call can finish before
// the input ends.
type session struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan message
	done  chan error
}

func startSession(t *testing.T, s *Server) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ss := &session{t: t, in: inW, lines: make(chan message, 64), done: make(chan error, 1)}
	go func() {
		err := s.Serve(context.Background(), inR, outW)
		outW.Close()
		ss.done <- err
	}()
	go func() {
		defer close(ss.lines)
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			var m message
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				m.JSONRPC = "invalid: " + sc.Text()
			}
			ss.lines <- m
		}
	}()
	t.Cleanup(func() { inW.Close() })
	return ss
}

func (ss *session) send(line string) {
	ss.t.Helper()
	if _, err := io.WriteString(ss.in, line+"\n"); err != nil {
		ss.t.Fatalf("send: %v", err)
	}
}

func (ss *session) recv() message {
	ss.t.Helper()
	select {
	case m, ok := <-ss.lines:
		if !ok {
			ss.t.Fatal("output closed")
		}
		if m.JSONRPC != "2.0" {
			ss.t.Fatalf("bad output line: %s", m.JSONRPC)
		}
		return m
	case <-time.After(5 * time.Second):
		ss.t.Fatal("no response within 5s")
	}
	return message{}
}

// close ends the input and returns the responses written after the last
// recv.
func (ss *session) close() []message {
	ss.t.Helper()
	ss.in.Close()
	var rest []message
	for m := range ss.lines {
		rest = append(rest, m)
	}
	if err := <-ss.done; err != nil {
		ss.t.Fatalf("Serve: %v", err)
	}
	return rest
}

func newWaitServer(c Client, clk *fakeClock) *Server {
	s := New(c, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.clock = clk
	return s
}

func waitCall(id, args string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"throt_wait_for_capacity","arguments":` + args + `}}`
}

func decodeWait(t *testing.T, m message) (toolResult, waitView) {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("rpc error %+v", m.Error)
	}
	var r struct {
		Content           []content       `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 {
		t.Fatalf("content = %+v", r.Content)
	}
	if !r.IsError {
		checkOutput(t, "throt_wait_for_capacity", r.StructuredContent)
	}
	var v waitView
	if len(r.StructuredContent) > 0 {
		if err := json.Unmarshal(r.StructuredContent, &v); err != nil {
			t.Fatal(err)
		}
	}
	return toolResult{Content: r.Content, IsError: r.IsError}, v
}

func TestWaitForCapacity(t *testing.T) {
	tests := []struct {
		name       string
		statuses   []app.StatusResult
		args       string
		wantChecks int
		wantWaited float64
		wantOK     bool
		timedOut   bool
		text       []string
	}{
		{"ok at once", []app.StatusResult{spare()}, `{}`, 1, 0, true, false, []string{"YES (after waiting 0s, 1 checks)"}},
		{"ok after two seconds", []app.StatusResult{busy(), busy(), spare()}, `{"max_wait_seconds":10}`, 3, 2, true, false,
			[]string{"YES (after waiting 2s, 3 checks)"}},
		{"times out", []app.StatusResult{busy()}, `{"max_wait_seconds":3}`, 4, 3, false, true,
			[]string{"NO (still, after waiting the full 3s, 4 checks)", "timed out:", "pressure is high"}},
		{"minimum", []app.StatusResult{busy()}, `{"max_wait_seconds":1}`, 2, 1, false, true, nil},
		{"default is 60s", []app.StatusResult{busy()}, `null`, 61, 60, false, true, []string{"full 1m0s"}},
		{"maximum", []app.StatusResult{busy()}, `{"max_wait_seconds":600,"agent":"claude-1"}`, 601, 600, false, true,
			[]string{"running heavy for claude-1: 1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &seqClient{statuses: tt.statuses}
			for i := range c.statuses {
				c.statuses[i].Scheduler.Queued = nil
			}
			clk := &fakeClock{now: time.Unix(1000, 0), auto: true}
			ss := startSession(t, newWaitServer(c, clk))
			ss.send(waitCall("1", tt.args))
			r, v := decodeWait(t, ss.recv())
			if rest := ss.close(); len(rest) != 0 {
				t.Fatalf("extra responses: %+v", rest)
			}
			if r.IsError || v.OKToStart != tt.wantOK || v.TimedOut != tt.timedOut || v.Checks != tt.wantChecks ||
				v.WaitedSeconds != tt.wantWaited || c.count() != tt.wantChecks {
				t.Fatalf("got %+v (calls %d): %s", v, c.count(), r.Content[0].Text)
			}
			for _, d := range clk.sleeps {
				if d != time.Second {
					t.Errorf("polled after %s, want 1s", d)
				}
			}
			for _, w := range append(tt.text, "advisory") {
				if !strings.Contains(r.Content[0].Text, w) {
					t.Errorf("text missing %q:\n%s", w, r.Content[0].Text)
				}
			}
		})
	}
}

func TestWaitForCapacityDefaultsToParentAgent(t *testing.T) {
	st := spare()
	st.Scheduler.Queued = nil
	s := newWaitServer(&seqClient{statuses: []app.StatusResult{st}}, &fakeClock{auto: true})
	s.parent = st.Agents[1].Key.PID
	r := s.waitForCapacity(context.Background(), "", time.Second)
	if !strings.Contains(r.Content[0].Text, "running heavy for codex-1: 1") {
		t.Fatalf("text:\n%s", r.Content[0].Text)
	}
}

func TestWaitForCapacityDaemonDown(t *testing.T) {
	for _, failAt := range []int{1, 3} {
		t.Run(fmt.Sprint("fail at check ", failAt), func(t *testing.T) {
			c := &seqClient{statuses: []app.StatusResult{busy()}, failAt: failAt}
			ss := startSession(t, newWaitServer(c, &fakeClock{auto: true}))
			ss.send(waitCall("1", `{"max_wait_seconds":600}`))
			r, _ := decodeWait(t, ss.recv())
			ss.close()
			if !r.IsError || !strings.Contains(r.Content[0].Text, "throtd &") || c.count() != failAt {
				t.Fatalf("calls %d: %+v", c.count(), r)
			}
		})
	}
}

func TestWaitForCapacityBadArguments(t *testing.T) {
	for _, args := range []string{
		`{"max_wait_seconds":0}`, `{"max_wait_seconds":601}`, `{"max_wait_seconds":-1}`,
		`{"max_wait_seconds":1.5}`, `{"max_wait_seconds":"5"}`, `{"wait":5}`, `{"agent":7}`, `[]`,
	} {
		t.Run(args, func(t *testing.T) {
			// Bad arguments are answered at once, without polling.
			c := &seqClient{statuses: []app.StatusResult{busy()}}
			r, _ := call(t, c, "throt_wait_for_capacity", args)
			if !r.IsError || !strings.Contains(r.Content[0].Text, "invalid arguments") || c.count() != 0 {
				t.Fatalf("calls %d: %+v", c.count(), r)
			}
		})
	}
}

// blockedWait starts a wait that stays blocked after its first check.
func blockedWait(t *testing.T, ss *session, clk *fakeClock, id string) {
	t.Helper()
	ss.send(waitCall(id, `{"max_wait_seconds":600}`))
	select {
	case <-clk.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not start")
	}
}

func TestWaitForCapacityCancelled(t *testing.T) {
	for _, id := range []string{`"w"`, `42`} {
		t.Run(id, func(t *testing.T) {
			c := &seqClient{statuses: []app.StatusResult{busy()}}
			clk := &fakeClock{blocked: make(chan time.Duration, 8)}
			s := newWaitServer(c, clk)
			ended := make(chan bool, 1)
			s.callDone = func(got string, stopped bool) {
				if got == id {
					ended <- stopped
				}
			}
			ss := startSession(t, s)
			blockedWait(t, ss, clk, id)
			// Other requests are answered while the wait is in flight.
			ss.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
			if m := ss.recv(); string(m.ID) != `"p"` {
				t.Fatalf("ping = %+v", m)
			}
			// Cancelling an unknown id changes nothing.
			ss.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"other"}}`)
			ss.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId": ` + id + ` ,"reason":"user"}}`)
			select {
			case stopped := <-ended:
				if !stopped {
					t.Fatal("wait ended without being cancelled")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not stop the wait")
			}
			// A repeated cancel is ignored, and the id can be reused.
			ss.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`)
			ss.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
			if m := ss.recv(); string(m.ID) != id || string(m.Result) != "{}" {
				t.Fatalf("reused id = %+v", m)
			}
			if rest := ss.close(); len(rest) != 0 {
				t.Fatalf("cancelled wait answered: %+v", rest)
			}
			if c.count() != 1 {
				t.Errorf("polled %d times, want 1", c.count())
			}
		})
	}
}

func TestWaitForCapacityStoppedAtEOF(t *testing.T) {
	clk := &fakeClock{blocked: make(chan time.Duration, 8)}
	s := newWaitServer(&seqClient{statuses: []app.StatusResult{busy()}}, clk)
	ended := make(chan bool, 1)
	s.callDone = func(_ string, stopped bool) { ended <- stopped }
	ss := startSession(t, s)
	blockedWait(t, ss, clk, "1")
	// Closing stdin is shutdown: Serve returns promptly, with no response
	// for the wait.
	if rest := ss.close(); len(rest) != 0 {
		t.Fatalf("responses after EOF: %+v", rest)
	}
	if stopped := <-ended; !stopped {
		t.Fatal("wait not stopped")
	}
}

func TestWaitForCapacityBounded(t *testing.T) {
	clk := &fakeClock{blocked: make(chan time.Duration, 8)}
	ss := startSession(t, newWaitServer(&seqClient{statuses: []app.StatusResult{busy()}}, clk))
	for i := range maxWaits {
		blockedWait(t, ss, clk, fmt.Sprint(i))
	}
	// An id already in flight is refused.
	ss.send(waitCall("0", `{}`))
	if m := ss.recv(); m.Error == nil || m.Error.Code != codeInvalidRequest || string(m.ID) != "0" {
		t.Fatalf("duplicate id = %+v", m)
	}
	ss.send(waitCall("99", `{}`))
	r, _ := decodeWait(t, ss.recv())
	if !r.IsError || !strings.Contains(r.Content[0].Text, "too many waits") {
		t.Fatalf("over the bound: %+v", r)
	}
	// After one is cancelled, another may start.
	ss.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":0}}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ss.send(waitCall("100", `{"max_wait_seconds":600}`))
		select {
		case <-clk.blocked:
			ss.close()
			return
		case m := <-ss.lines:
			// The cancel had not landed yet; the refusal is a tool error.
			if time.Now().After(deadline) {
				t.Fatalf("no slot freed: %+v", m)
			}
		}
	}
}

func TestWaitForCapacityModern(t *testing.T) {
	ss := startSession(t, newWaitServer(&seqClient{statuses: []app.StatusResult{spare()}}, &fakeClock{auto: true}))
	ss.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"throt_wait_for_capacity",` + modernMeta + `}}`)
	m := ss.recv()
	ss.close()
	if m.Error != nil {
		t.Fatalf("err %+v", m.Error)
	}
	r := modernResultFields(t, m.Result)
	if !strings.Contains(string(r["structuredContent"]), `"checks":1`) {
		t.Errorf("result = %s", m.Result)
	}
}
