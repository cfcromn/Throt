// Package usage reports the tokens and active time of coding-agent
// sessions from the session logs Claude Code and Codex keep on disk.
//
// It is read-only and reads counts, ids, model names, timestamps and the
// working directory only: lines are decoded into structs that have no
// field for message text, prompts, or tool input and output, so that
// content is never kept, and no line is ever printed or logged.
package usage

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"time"
)

// ActiveGap bounds active time: the time between two consecutive log
// entries of a session counts as active when it is under ActiveGap.
const ActiveGap = 5 * time.Minute

// Agent kinds, as reported.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
)

// By is how Report groups its rows.
type By string

const (
	BySession By = "session"
	ByProject By = "project"
	ByHour    By = "hour"
)

// ParseBy parses the value of --by.
func ParseBy(s string) (By, error) {
	switch By(s) {
	case BySession, ByProject, ByHour:
		return By(s), nil
	}
	return "", fmt.Errorf("unknown grouping %q (want session, project or hour)", s)
}

// Tokens are token counts as the agent logged them.
type Tokens struct {
	Input      int64 `json:"input_tokens"` // not read from or written to the cache
	Output     int64 `json:"output_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
}

func (t *Tokens) add(u Tokens) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheWrite += u.CacheWrite
	t.CacheRead += u.CacheRead
}

// Total is the sum of the four counts.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// Entry is what one log line contributes: a timestamp of a session and,
// for a model response, its token counts.
type Entry struct {
	Agent   string
	Session string
	Project string // base name of the working directory
	Time    time.Time
	// Message marks a model response. Entries with the same non-empty ID
	// are one response and are counted once.
	Message bool
	ID      string
	Tokens  Tokens
}

// Row is the usage of one agent kind, session, project or hour.
type Row struct {
	Agent   string     `json:"agent,omitempty"`
	Session string     `json:"session,omitempty"`
	Project string     `json:"project,omitempty"`
	Hour    *time.Time `json:"hour,omitempty"`
	// Sessions is how many sessions the row covers (1 for a session).
	Sessions int `json:"sessions"`
	Tokens
	Messages      int       `json:"messages"`
	ActiveSeconds int64     `json:"active_seconds"`
	First         time.Time `json:"first"`
	Last          time.Time `json:"last"`

	active time.Duration
	seen   map[int32]struct{} // sessions
}

func (r *Row) stamp(t time.Time, sess int32) {
	if r.First.IsZero() || t.Before(r.First) {
		r.First = t
	}
	if t.After(r.Last) {
		r.Last = t
	}
	if r.seen == nil {
		r.seen = map[int32]struct{}{}
	}
	r.seen[sess] = struct{}{}
}

func (r *Row) finish() {
	r.Sessions = len(r.seen)
	r.ActiveSeconds = int64(r.active.Round(time.Second) / time.Second)
	r.seen = nil
}

// Report is the result of 'throt usage'.
type Report struct {
	Since            time.Time `json:"since"`
	By               By        `json:"by"`
	ActiveGapSeconds int       `json:"active_gap_seconds"`
	// Agents has one row per agent kind that has entries, by name.
	Agents []Row `json:"agents"`
	Total  Row   `json:"total"`
	// Rows are sessions or projects, most recently active first, or
	// hours in time order.
	Rows []Row `json:"rows"`
	// Files is how many log files were read, and SkippedLines how many of
	// their lines were malformed or too long to read.
	Files        int `json:"files"`
	SkippedLines int `json:"skipped_lines"`
}

type sessionKey struct{ agent, id string }

// stamp is one log entry: 16 bytes, so a long history stays small.
type stamp struct {
	t       int64 // Unix nanoseconds
	session int32
	project int32
}

type message struct {
	stamp
	tokens Tokens
}

// Collector gathers entries at or after a time.
type Collector struct {
	since    time.Time
	sessions []sessionKey
	sessIdx  map[sessionKey]int32
	projects []string
	projIdx  map[string]int32
	stamps   []stamp
	messages []message
	byID     map[string]int // index in messages

	files, skipped int
}

// NewCollector returns a collector of the entries at or after since.
func NewCollector(since time.Time) *Collector {
	return &Collector{
		since:   since,
		sessIdx: map[sessionKey]int32{},
		projIdx: map[string]int32{},
		byID:    map[string]int{},
	}
}

// Add records e, unless it is older than the collector's start or has no
// time or session.
func (c *Collector) Add(e Entry) {
	if e.Time.IsZero() || e.Time.Before(c.since) || e.Session == "" {
		return
	}
	sk := sessionKey{e.Agent, e.Session}
	si, ok := c.sessIdx[sk]
	if !ok {
		si = int32(len(c.sessions))
		c.sessions = append(c.sessions, sk)
		c.sessIdx[sk] = si
	}
	pi, ok := c.projIdx[e.Project]
	if !ok {
		pi = int32(len(c.projects))
		c.projects = append(c.projects, e.Project)
		c.projIdx[e.Project] = pi
	}
	st := stamp{t: e.Time.UnixNano(), session: si, project: pi}
	c.stamps = append(c.stamps, st)
	if !e.Message {
		return
	}
	if e.ID == "" {
		c.messages = append(c.messages, message{st, e.Tokens})
		return
	}
	// One response is logged once per content block, and again in the log
	// of a session resumed from it: keep the earliest line's time and
	// session, and the largest counts (a line written while the response
	// streams can have fewer output tokens).
	i, dup := c.byID[sk.agent+"\x00"+e.ID]
	if !dup {
		c.byID[sk.agent+"\x00"+e.ID] = len(c.messages)
		c.messages = append(c.messages, message{st, e.Tokens})
		return
	}
	m := &c.messages[i]
	m.tokens.Input = max(m.tokens.Input, e.Tokens.Input)
	m.tokens.Output = max(m.tokens.Output, e.Tokens.Output)
	m.tokens.CacheWrite = max(m.tokens.CacheWrite, e.Tokens.CacheWrite)
	m.tokens.CacheRead = max(m.tokens.CacheRead, e.Tokens.CacheRead)
	if st.t < m.t {
		m.stamp = st
	}
}

// Report groups what was collected. Hours are those of loc.
func (c *Collector) Report(by By, loc *time.Location) Report {
	rep := Report{Since: c.since, By: by, ActiveGapSeconds: int(ActiveGap / time.Second), Files: c.files, SkippedLines: c.skipped}

	slices.SortFunc(c.stamps, func(a, b stamp) int {
		return cmp.Or(cmp.Compare(a.session, b.session), cmp.Compare(a.t, b.t))
	})
	// A session belongs to the project of its first entry.
	sessProject := make([]int32, len(c.sessions))
	for i := len(c.stamps) - 1; i >= 0; i-- {
		sessProject[c.stamps[i].session] = c.stamps[i].project
	}

	agents := map[string]*Row{}
	rows := map[int64]*Row{}
	group := func(s stamp) *Row {
		var key int64
		switch by {
		case ByProject:
			key = int64(s.project)
		case ByHour:
			t := time.Unix(0, s.t).In(loc)
			key = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Unix()
		default:
			key = int64(s.session)
		}
		r := rows[key]
		if r != nil {
			return r
		}
		r = &Row{}
		switch by {
		case ByProject:
			r.Project = c.projects[s.project]
		case ByHour:
			h := time.Unix(key, 0).In(loc)
			r.Hour = &h
		default:
			sk := c.sessions[s.session]
			r.Agent, r.Session, r.Project = sk.agent, sk.id, c.projects[sessProject[s.session]]
		}
		rows[key] = r
		return r
	}
	agent := func(s stamp) *Row {
		name := c.sessions[s.session].agent
		r := agents[name]
		if r == nil {
			r = &Row{Agent: name}
			agents[name] = r
		}
		return r
	}

	for i, s := range c.stamps {
		t := time.Unix(0, s.t)
		var gap time.Duration
		if i > 0 && c.stamps[i-1].session == s.session {
			if d := time.Duration(s.t - c.stamps[i-1].t); d < ActiveGap {
				gap = d
			}
		}
		// A gap counts for the group of the entry that ends it.
		for _, r := range []*Row{group(s), agent(s), &rep.Total} {
			r.stamp(t, s.session)
			r.active += gap
		}
	}
	for _, m := range c.messages {
		for _, r := range []*Row{group(m.stamp), agent(m.stamp), &rep.Total} {
			r.Tokens.add(m.tokens)
			r.Messages++
		}
	}

	rep.Total.finish()
	rep.Agents = make([]Row, 0, len(agents))
	for _, r := range agents {
		r.finish()
		rep.Agents = append(rep.Agents, *r)
	}
	slices.SortFunc(rep.Agents, func(a, b Row) int { return cmp.Compare(a.Agent, b.Agent) })
	rep.Rows = make([]Row, 0, len(rows))
	for _, r := range rows {
		r.finish()
		rep.Rows = append(rep.Rows, *r)
	}
	slices.SortFunc(rep.Rows, func(a, b Row) int {
		if by == ByHour {
			return a.Hour.Compare(*b.Hour)
		}
		return cmp.Or(b.Last.Compare(a.Last), cmp.Compare(a.Agent, b.Agent),
			cmp.Compare(a.Session, b.Session), cmp.Compare(a.Project, b.Project))
	})
	return rep
}

// projectName is the base name of the working directory dir, the only
// part of it that is kept.
func projectName(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(dir)
}
