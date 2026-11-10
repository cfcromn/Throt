package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture lines are synthetic: they have the fields of real session
// logs, and "SECRET" wherever a real log has conversation content.

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestClaudeParser(t *testing.T) {
	const sess = "11111111-aaaa-4bbb-8ccc-000000000001"
	assistant := func(id, usage string) string {
		return `{"type":"assistant","timestamp":"2026-10-28T10:00:05.000Z","sessionId":"` + sess + `","cwd":"/work/app",` +
			`"message":{"id":"` + id + `","model":"claude-x","role":"assistant","content":[{"type":"text","text":"SECRET"}],"usage":` + usage + `}}`
	}
	tests := []struct {
		name    string
		path    string
		line    string
		want    Entry
		ok, bad bool
	}{
		{
			name: "assistant with usage",
			line: assistant("msg_1", `{"input_tokens":3,"output_tokens":40,"cache_creation_input_tokens":500,"cache_read_input_tokens":6000,"service_tier":"standard"}`),
			want: Entry{Agent: ClaudeCode, Session: sess, Project: "app", Time: ts("2026-10-28T10:00:05Z"), Message: true, ID: "msg_1",
				Tokens: Tokens{Input: 3, Output: 40, CacheWrite: 500, CacheRead: 6000}},
			ok: true,
		},
		{
			name: "negative counts are zero",
			line: assistant("msg_2", `{"input_tokens":-3,"output_tokens":4}`),
			want: Entry{Agent: ClaudeCode, Session: sess, Project: "app", Time: ts("2026-10-28T10:00:05Z"), Message: true, ID: "msg_2", Tokens: Tokens{Output: 4}},
			ok:   true,
		},
		{
			name: "user line is a timestamp only",
			line: `{"type":"user","timestamp":"2026-10-28T10:00:00.000Z","sessionId":"` + sess + `","cwd":"/work/app","message":{"role":"user","content":"SECRET"}}`,
			want: Entry{Agent: ClaudeCode, Session: sess, Project: "app", Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{
			name: "synthetic response has no usage",
			line: `{"type":"assistant","timestamp":"2026-10-28T10:00:00Z","sessionId":"` + sess + `","message":{"id":"x","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`,
			want: Entry{Agent: ClaudeCode, Session: sess, Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{
			name: "usage on a line that is not a response",
			line: `{"type":"progress","timestamp":"2026-10-28T10:00:00Z","sessionId":"` + sess + `","message":{"id":"x","usage":{"input_tokens":9}}}`,
			want: Entry{Agent: ClaudeCode, Session: sess, Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{
			name: "message of another type",
			line: `{"type":"system","timestamp":"2026-10-28T10:00:00Z","sessionId":"` + sess + `","message":"SECRET"}`,
			want: Entry{Agent: ClaudeCode, Session: sess, Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{name: "no timestamp", line: `{"type":"last-prompt","sessionId":"` + sess + `","lastPrompt":"SECRET"}`},
		{
			name: "session from the file name",
			path: "/h/.claude/projects/-work-app/22222222-aaaa-4bbb-8ccc-000000000002.jsonl",
			line: `{"type":"system","timestamp":"2026-10-28T10:00:00Z"}`,
			want: Entry{Agent: ClaudeCode, Session: "22222222-aaaa-4bbb-8ccc-000000000002", Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{
			name: "subagent log belongs to its session",
			path: "/h/.claude/projects/-work-app/22222222-aaaa-4bbb-8ccc-000000000002/subagents/agent-a1.jsonl",
			line: `{"type":"user","timestamp":"2026-10-28T10:00:00Z"}`,
			want: Entry{Agent: ClaudeCode, Session: "22222222-aaaa-4bbb-8ccc-000000000002", Time: ts("2026-10-28T10:00:00Z")},
			ok:   true,
		},
		{name: "truncated line", line: `{"type":"assistant","timestamp":"2026-10-28T10:00:00Z","mess`, bad: true},
		{name: "not JSON", line: `SECRET`, bad: true},
		{name: "bad timestamp", line: `{"type":"user","timestamp":"yesterday"}`, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path == "" {
				path = "/h/.claude/projects/-work-app/x.jsonl"
			}
			got, ok, err := newClaudeParser(path).parse([]byte(tt.line))
			if (err != nil) != tt.bad || ok != tt.ok {
				t.Fatalf("ok %v, err %v; want ok %v, bad %v", ok, err, tt.ok, tt.bad)
			}
			if got != tt.want {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestClaudeParserKeepsState checks that a line without a session id or
// working directory gets those of the line before it.
func TestClaudeParserKeepsState(t *testing.T) {
	p := newClaudeParser("/h/.claude/projects/-work-app/file.jsonl")
	if _, _, err := p.parse([]byte(`{"type":"user","timestamp":"2026-10-28T10:00:00Z","sessionId":"s1","cwd":"/work/app"}`)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := p.parse([]byte(`{"type":"system","timestamp":"2026-10-28T10:01:00Z"}`))
	want := Entry{Agent: ClaudeCode, Session: "s1", Project: "app", Time: ts("2026-10-28T10:01:00Z")}
	if err != nil || !ok || got != want {
		t.Errorf("got %+v, %v, %v; want %+v", got, ok, err, want)
	}
}

func TestCodexParser(t *testing.T) {
	const thread = "01a00000-0000-7000-8000-000000000001"
	count := func(at, total, last string) string {
		info := "null"
		if total != "" {
			info = `{"total_token_usage":` + total + `,"last_token_usage":` + last + `,"model_context_window":100}`
		}
		return `{"timestamp":"` + at + `","type":"event_msg","payload":{"type":"token_count","info":` + info + `,"rate_limits":{}}}`
	}
	use := func(in, cached, out int) string {
		b, _ := json.Marshal(map[string]int{"input_tokens": in, "cached_input_tokens": cached, "cache_write_input_tokens": 0, "output_tokens": out, "total_tokens": in + out})
		return string(b)
	}
	stamp := func(at string) Entry {
		return Entry{Agent: Codex, Session: thread, Project: "app", Time: ts(at)}
	}
	msg := func(at string, tok Tokens) Entry {
		e := stamp(at)
		e.Message, e.Tokens = true, tok
		return e
	}
	steps := []struct {
		name string
		line string
		want Entry
		ok   bool
	}{
		{"meta", `{"timestamp":"2026-10-28T09:00:00Z","type":"session_meta","payload":{"id":"` + thread + `","session_id":"` + thread + `","cwd":"/work/app","base_instructions":{"text":"SECRET"}}}`,
			stamp("2026-10-28T09:00:00Z"), true},
		{"message is a timestamp only", `{"timestamp":"2026-10-28T09:00:01Z","type":"response_item","payload":{"type":"message","id":"m1","content":[{"text":"SECRET"}]}}`,
			stamp("2026-10-28T09:00:01Z"), true},
		{"count without info", count("2026-10-28T09:00:02Z", "", ""), stamp("2026-10-28T09:00:02Z"), true},
		{"first count", count("2026-10-28T09:00:03Z", use(1000, 0, 20), use(1000, 0, 20)),
			msg("2026-10-28T09:00:03Z", Tokens{Input: 1000, Output: 20}), true},
		{"repeated count", count("2026-10-28T09:00:04Z", use(1000, 0, 20), use(1000, 0, 20)), stamp("2026-10-28T09:00:04Z"), true},
		{"growth of the total", count("2026-10-28T09:00:05Z", use(2500, 900, 50), use(9, 9, 9)),
			msg("2026-10-28T09:00:05Z", Tokens{Input: 600, Output: 30, CacheRead: 900}), true},
		{"total went backwards", count("2026-10-28T09:00:06Z", use(400, 300, 5), use(400, 300, 5)),
			msg("2026-10-28T09:00:06Z", Tokens{Input: 100, Output: 5, CacheRead: 300}), true},
		{"turn context moves the project", `{"timestamp":"2026-10-28T09:00:07Z","type":"turn_context","payload":{"cwd":"/work/other","model":"gpt-x"}}`,
			Entry{Agent: Codex, Session: thread, Project: "other", Time: ts("2026-10-28T09:00:07Z")}, true},
		{"no timestamp", `{"type":"event_msg","payload":{"type":"task_started"}}`, Entry{}, false},
	}
	p := newCodexParser("/h/.codex/sessions/2026/10/28/rollout-2026-10-28T09-00-00-" + thread + ".jsonl")
	for _, s := range steps {
		got, ok, err := p.parse([]byte(s.line))
		if err != nil || ok != s.ok || got != s.want {
			t.Errorf("%s: got %+v, %v, %v\nwant %+v, %v", s.name, got, ok, err, s.want, s.ok)
		}
	}
	if _, _, err := p.parse([]byte(`{"timestamp":`)); err == nil {
		t.Error("truncated line: no error")
	}
}

func TestCodexParserSession(t *testing.T) {
	const file = "01a00000-0000-7000-8000-000000000002"
	path := "/h/.codex/sessions/2026/10/28/rollout-2026-10-28T09-00-00-" + file + ".jsonl"
	tests := []struct{ name, line, want string }{
		{"file name", `{"timestamp":"2026-10-28T09:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`, file},
		{"meta id", `{"timestamp":"2026-10-28T09:00:00Z","type":"session_meta","payload":{"id":"thread"}}`, "thread"},
		{"subagent thread counts for its session", `{"timestamp":"2026-10-28T09:00:00Z","type":"session_meta","payload":{"id":"thread","session_id":"root"}}`, "root"},
	}
	for _, tt := range tests {
		got, _, err := newCodexParser(path).parse([]byte(tt.line))
		if err != nil || got.Session != tt.want {
			t.Errorf("%s: session %q, %v; want %q", tt.name, got.Session, err, tt.want)
		}
	}
	if got, _, _ := newCodexParser("/x/odd.jsonl").parse([]byte(`{"timestamp":"2026-10-28T09:00:00Z"}`)); got.Session != "odd" {
		t.Errorf("odd file name: session %q", got.Session)
	}
}

func entry(agent, session, project, at string) Entry {
	return Entry{Agent: agent, Session: session, Project: project, Time: ts(at)}
}

func response(e Entry, id string, tok Tokens) Entry {
	e.Message, e.ID, e.Tokens = true, id, tok
	return e
}

func TestActiveTime(t *testing.T) {
	tests := []struct {
		name string
		at   []string // entries of one session, in any order
		want int64    // active seconds
	}{
		{"one entry", []string{"2026-10-28T10:00:00Z"}, 0},
		{"short gaps add up", []string{"2026-10-28T10:00:00Z", "2026-10-28T10:01:00Z", "2026-10-28T10:03:30Z"}, 210},
		{"just under the limit", []string{"2026-10-28T10:00:00Z", "2026-10-28T10:04:59Z"}, 299},
		{"the limit itself is idle", []string{"2026-10-28T10:00:00Z", "2026-10-28T10:05:00Z"}, 0},
		{"a long gap splits the session", []string{"2026-10-28T10:00:00Z", "2026-10-28T10:02:00Z", "2026-10-28T11:00:00Z", "2026-10-28T11:00:30Z"}, 150},
		{"order in the log does not matter", []string{"2026-10-28T10:02:00Z", "2026-10-28T10:00:00Z", "2026-10-28T10:01:00Z"}, 120},
		{"same instant twice", []string{"2026-10-28T10:00:00Z", "2026-10-28T10:00:00Z", "2026-10-28T10:00:10Z"}, 10},
	}
	for _, tt := range tests {
		c := NewCollector(time.Time{})
		for _, at := range tt.at {
			c.Add(entry(ClaudeCode, "s", "app", at))
		}
		// Another session's entries in between must not bridge a gap.
		c.Add(entry(ClaudeCode, "other", "app", "2026-10-28T10:30:00Z"))
		c.Add(entry(Codex, "s", "app", "2026-10-28T10:04:00Z"))
		rep := c.Report(BySession, time.UTC)
		for _, r := range rep.Rows {
			if r.Agent == ClaudeCode && r.Session == "s" && r.ActiveSeconds != tt.want {
				t.Errorf("%s: active %ds, want %ds", tt.name, r.ActiveSeconds, tt.want)
			}
		}
		if rep.Total.ActiveSeconds != tt.want || len(rep.Rows) != 3 {
			t.Errorf("%s: total active %ds in %d rows, want %ds in 3", tt.name, rep.Total.ActiveSeconds, len(rep.Rows), tt.want)
		}
	}
}

func TestMessageCountedOnce(t *testing.T) {
	c := NewCollector(time.Time{})
	// One response: a line per content block, the first written with
	// fewer output tokens, and a copy in the log of a resumed session.
	c.Add(response(entry(ClaudeCode, "s1", "app", "2026-10-28T10:00:01Z"), "msg_1", Tokens{Input: 5, Output: 10, CacheWrite: 100, CacheRead: 1000}))
	c.Add(response(entry(ClaudeCode, "s1", "app", "2026-10-28T10:00:02Z"), "msg_1", Tokens{Input: 5, Output: 80, CacheWrite: 100, CacheRead: 1000}))
	c.Add(response(entry(ClaudeCode, "s2", "app", "2026-10-28T10:00:01Z"), "msg_1", Tokens{Input: 5, Output: 80, CacheWrite: 100, CacheRead: 1000}))
	c.Add(response(entry(ClaudeCode, "s1", "app", "2026-10-28T10:00:03Z"), "msg_2", Tokens{Input: 1, Output: 2}))
	// The same id from another agent kind is another response, and
	// responses without an id are never merged.
	c.Add(response(entry(Codex, "c1", "app", "2026-10-28T10:00:01Z"), "msg_1", Tokens{Input: 7}))
	c.Add(response(entry(Codex, "c1", "app", "2026-10-28T10:00:02Z"), "", Tokens{Input: 7}))
	c.Add(response(entry(Codex, "c1", "app", "2026-10-28T10:00:03Z"), "", Tokens{Input: 7}))

	rep := c.Report(BySession, time.UTC)
	want := map[string]Row{
		"s1": {Messages: 2, Tokens: Tokens{Input: 6, Output: 82, CacheWrite: 100, CacheRead: 1000}},
		"s2": {},
		"c1": {Messages: 3, Tokens: Tokens{Input: 21}},
	}
	for _, r := range rep.Rows {
		if w := want[r.Session]; r.Messages != w.Messages || r.Tokens != w.Tokens {
			t.Errorf("%s: %d messages, %+v; want %d, %+v", r.Session, r.Messages, r.Tokens, w.Messages, w.Tokens)
		}
	}
	if rep.Total.Messages != 5 || rep.Total.Total() != 6+82+100+1000+21 {
		t.Errorf("total: %d messages, %d tokens", rep.Total.Messages, rep.Total.Total())
	}
}

func TestSince(t *testing.T) {
	c := NewCollector(ts("2026-10-28T10:00:00Z"))
	c.Add(response(entry(ClaudeCode, "s", "app", "2026-10-28T09:59:59Z"), "old", Tokens{Input: 1000}))
	c.Add(response(entry(ClaudeCode, "s", "app", "2026-10-28T10:00:00Z"), "edge", Tokens{Input: 1}))
	c.Add(response(entry(ClaudeCode, "s", "app", "2026-10-28T10:00:30Z"), "new", Tokens{Input: 2}))
	c.Add(response(entry(ClaudeCode, "before", "app", "2026-10-27T10:00:30Z"), "older", Tokens{Input: 5}))
	c.Add(Entry{Agent: ClaudeCode, Session: "s", Tokens: Tokens{Input: 9}, Message: true}) // no time
	c.Add(Entry{Agent: ClaudeCode, Time: ts("2026-10-28T11:00:00Z")})                      // no session
	rep := c.Report(BySession, time.UTC)
	if len(rep.Rows) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(rep.Rows), rep.Rows)
	}
	r := rep.Rows[0]
	// The gap from the entry before the start is not counted either.
	if r.Session != "s" || r.Input != 3 || r.Messages != 2 || r.ActiveSeconds != 30 || !r.First.Equal(ts("2026-10-28T10:00:00Z")) || !r.Last.Equal(ts("2026-10-28T10:00:30Z")) {
		t.Errorf("row %+v", r)
	}
	if !rep.Since.Equal(ts("2026-10-28T10:00:00Z")) || rep.ActiveGapSeconds != 300 {
		t.Errorf("since %v, gap %d", rep.Since, rep.ActiveGapSeconds)
	}
}

func TestReportGrouping(t *testing.T) {
	fill := func() *Collector {
		c := NewCollector(time.Time{})
		c.Add(entry(ClaudeCode, "s1", "app", "2026-10-28T09:58:00Z"))
		c.Add(response(entry(ClaudeCode, "s1", "app", "2026-10-28T10:01:00Z"), "a", Tokens{Input: 1, Output: 10}))
		c.Add(response(entry(ClaudeCode, "s1", "lib", "2026-10-28T10:02:00Z"), "b", Tokens{Input: 2, Output: 20}))
		c.Add(entry(Codex, "c1", "app", "2026-10-28T12:00:00Z"))
		c.Add(response(entry(Codex, "c1", "app", "2026-10-28T12:00:40Z"), "", Tokens{Input: 4, CacheRead: 40}))
		return c
	}
	hour := func(h int) *time.Time {
		t := time.Date(2026, 10, 28, h, 0, 0, 0, time.UTC)
		return &t
	}
	type row struct {
		agent, session, project string
		hour                    *time.Time
		sessions, messages      int
		total, active           int64
	}
	tests := []struct {
		by   By
		want []row
	}{
		// Most recently active first; a session is in its first project.
		{BySession, []row{
			{agent: Codex, session: "c1", project: "app", sessions: 1, messages: 1, total: 44, active: 40},
			{agent: ClaudeCode, session: "s1", project: "app", sessions: 1, messages: 2, total: 33, active: 240},
		}},
		// A gap counts for the project and hour of the entry that ends it.
		{ByProject, []row{
			{project: "app", sessions: 2, messages: 2, total: 55, active: 220},
			{project: "lib", sessions: 1, messages: 1, total: 22, active: 60},
		}},
		{ByHour, []row{
			{hour: hour(9), sessions: 1},
			{hour: hour(10), sessions: 1, messages: 2, total: 33, active: 240},
			{hour: hour(12), sessions: 1, messages: 1, total: 44, active: 40},
		}},
	}
	for _, tt := range tests {
		rep := fill().Report(tt.by, time.UTC)
		if len(rep.Rows) != len(tt.want) {
			t.Fatalf("%s: %d rows, want %d", tt.by, len(rep.Rows), len(tt.want))
		}
		for i, w := range tt.want {
			r := rep.Rows[i]
			sameHour := (r.Hour == nil) == (w.hour == nil) && (r.Hour == nil || r.Hour.Equal(*w.hour))
			if r.Agent != w.agent || r.Session != w.session || r.Project != w.project || !sameHour ||
				r.Sessions != w.sessions || r.Messages != w.messages || r.Total() != w.total || r.ActiveSeconds != w.active {
				t.Errorf("%s row %d: %+v, want %+v", tt.by, i, r, w)
			}
		}
		if len(rep.Agents) != 2 || rep.Agents[0].Agent != ClaudeCode || rep.Agents[1].Agent != Codex ||
			rep.Agents[0].ActiveSeconds != 240 || rep.Agents[1].Total() != 44 || rep.Agents[0].Sessions != 1 {
			t.Errorf("%s agents: %+v", tt.by, rep.Agents)
		}
		if rep.Total.Sessions != 2 || rep.Total.Messages != 3 || rep.Total.ActiveSeconds != 280 || rep.By != tt.by {
			t.Errorf("%s total: %+v", tt.by, rep.Total)
		}
	}
}

func TestParseBy(t *testing.T) {
	for _, s := range []string{"session", "project", "hour"} {
		if by, err := ParseBy(s); err != nil || string(by) != s {
			t.Errorf("%s: %v, %v", s, by, err)
		}
	}
	if _, err := ParseBy("day"); err == nil {
		t.Error("day: no error")
	}
}

func TestSources(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	got := Sources(get, "/home/u")
	want := []Source{{ClaudeCode, "/home/u/.claude/projects"}, {Codex, "/home/u/.codex/sessions"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("defaults: %+v", got)
	}
	env["CLAUDE_CONFIG_DIR"], env["CODEX_HOME"] = "/cfg/claude", "/cfg/codex"
	got = Sources(get, "/home/u")
	want = []Source{{ClaudeCode, "/cfg/claude/projects"}, {Codex, "/cfg/codex/sessions"}}
	if got[0] != want[0] || got[1] != want[1] {
		t.Errorf("environment: %+v", got)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, lines ...string) string {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	claude := func(at, id string, out int) string {
		b, _ := json.Marshal(map[string]any{
			"type": "assistant", "timestamp": at, "sessionId": "s1", "cwd": "/work/app",
			"message": map[string]any{"id": id, "model": "claude-x", "content": "SECRET", "usage": map[string]int{"output_tokens": out}},
		})
		return string(b) + "\n"
	}
	huge := `{"type":"user","timestamp":"2026-10-28T10:00:20Z","sessionId":"s1","message":{"content":"` + strings.Repeat("S", maxLine) + `"}}` + "\n"
	write("claude/projects/-work-app/s1.jsonl",
		claude("2026-10-28T10:00:00Z", "m1", 10),
		"SECRET not json\n",
		"\n",
		huge,
		claude("2026-10-28T10:00:30Z", "m2", 20),
		// Still being written: no newline yet.
		`{"type":"assistant","timestamp":"2026-10-28T10:00:40Z","sessionId":"s1","message":{"id":"m3","usage":{"output_tokens":4000}}}`,
	)
	write("claude/projects/-work-app/s1/subagents/agent-a.jsonl", claude("2026-10-28T10:01:00Z", "m4", 5))
	write("claude/projects/-work-app/s1/tool-results/out.txt", "SECRET\n")
	old := write("claude/projects/-work-app/old.jsonl", claude("2026-10-28T10:00:00Z", "m5", 7000))
	stale := ts("2026-10-01T00:00:00Z")
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	write("codex/sessions/2026/10/28/rollout-2026-10-28T11-00-00-01a00000-0000-7000-8000-000000000001.jsonl",
		`{"timestamp":"2026-10-28T11:00:00Z","type":"session_meta","payload":{"id":"c1","cwd":"/work/app"}}`+"\n",
		`{"timestamp":"2026-10-28T11:00:10Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":7},"last_token_usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":7}}}}`+"\n",
	)
	// A rollout without token counts reports its time only.
	write("codex/sessions/2026/10/28/rollout-2026-10-28T12-00-00-01a00000-0000-7000-8000-000000000002.jsonl",
		`{"timestamp":"2026-10-28T12:00:00Z","type":"session_meta","payload":{"id":"c2","cwd":"/work/app"}}`+"\n",
		`{"timestamp":"2026-10-28T12:01:00Z","type":"response_item","payload":{"type":"message","content":"SECRET"}}`+"\n",
	)

	c := NewCollector(ts("2026-10-15T00:00:00Z"))
	sources := []Source{
		{ClaudeCode, filepath.Join(dir, "claude", "projects")},
		{Codex, filepath.Join(dir, "codex", "sessions")},
		{Codex, filepath.Join(dir, "missing")},
	}
	if err := Scan(context.Background(), sources, c); err != nil {
		t.Fatal(err)
	}
	rep := c.Report(BySession, time.UTC)
	if rep.Files != 4 || rep.SkippedLines != 2 {
		t.Errorf("%d files, %d skipped lines; want 4, 2", rep.Files, rep.SkippedLines)
	}
	want := map[string]Row{
		"s1": {Messages: 3, Tokens: Tokens{Output: 35}, ActiveSeconds: 60},
		"c1": {Messages: 1, Tokens: Tokens{Input: 40, Output: 7, CacheRead: 60}, ActiveSeconds: 10},
		"c2": {ActiveSeconds: 60},
	}
	if len(rep.Rows) != len(want) {
		t.Errorf("%d rows, want %d", len(rep.Rows), len(want))
	}
	for _, r := range rep.Rows {
		if w := want[r.Session]; r.Messages != w.Messages || r.Tokens != w.Tokens || r.ActiveSeconds != w.ActiveSeconds || r.Project != "app" {
			t.Errorf("%s: %+v", r.Session, r)
		}
	}
	// Nothing of the conversation reaches the report.
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SECRET") || strings.Contains(string(out), "/work") {
		t.Errorf("report has log content or a full path: %s", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Scan(ctx, sources, NewCollector(time.Time{})); err == nil {
		t.Error("cancelled scan: no error")
	}
}
