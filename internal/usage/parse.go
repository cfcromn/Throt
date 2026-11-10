package usage

import (
	"cmp"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// A parser turns the lines of one log file into entries. ok is false for
// a line with nothing to report; err is non-nil for a malformed one.
type parser interface {
	parse(line []byte) (e Entry, ok bool, err error)
}

// decode unmarshals line into v, which has only the fields this package
// may read. A field of an unexpected type is left zero, not an error:
// lines of other kinds reuse the names.
func decode(line []byte, v any) error {
	err := json.Unmarshal(line, v)
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return nil
	}
	return err
}

// claudeLine is the part of a Claude Code log line that is read.
type claudeLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// claudeSynthetic is the model of the responses Claude Code writes itself
// (an API error shown as a message); they used no tokens.
const claudeSynthetic = "<synthetic>"

// claudeParser reads one Claude Code session log.
type claudeParser struct {
	session, project string // of the last line that had one
}

// newClaudeParser returns the parser of the log at path, which is
// <project>/<session>.jsonl, or <project>/<session>/subagents/<agent>.jsonl
// for a subagent of that session.
func newClaudeParser(path string) *claudeParser {
	p := &claudeParser{session: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	if dir := filepath.Dir(path); filepath.Base(dir) == "subagents" {
		p.session = filepath.Base(filepath.Dir(dir))
	}
	return p
}

func (p *claudeParser) parse(line []byte) (Entry, bool, error) {
	var l claudeLine
	if err := decode(line, &l); err != nil {
		return Entry{}, false, err
	}
	if l.SessionID != "" {
		p.session = l.SessionID
	}
	if l.CWD != "" {
		p.project = projectName(l.CWD)
	}
	if l.Timestamp.IsZero() {
		return Entry{}, false, nil
	}
	e := Entry{Agent: ClaudeCode, Session: p.session, Project: p.project, Time: l.Timestamp}
	if m := l.Message; l.Type == "assistant" && m != nil && m.Usage != nil && m.ID != "" && m.Model != claudeSynthetic {
		e.Message, e.ID = true, m.ID
		e.Tokens = Tokens{
			Input:      max(m.Usage.Input, 0),
			Output:     max(m.Usage.Output, 0),
			CacheWrite: max(m.Usage.CacheWrite, 0),
			CacheRead:  max(m.Usage.CacheRead, 0),
		}
	}
	return e, true, nil
}

// codexLine is the part of a Codex rollout line that is read.
type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type string `json:"type"`
		// Of session_meta: the thread, and the session it belongs to (its
		// own, or the one that started it as a subagent).
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"` // of session_meta and turn_context
		Info      *struct {
			Total *codexUsage `json:"total_token_usage"`
			Last  *codexUsage `json:"last_token_usage"`
		} `json:"info"` // of a token_count event
	} `json:"payload"`
}

// codexUsage is a Codex token count. Its input tokens include the cached
// ones.
type codexUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

func (u codexUsage) sub(v codexUsage) codexUsage {
	return codexUsage{u.Input - v.Input, u.Cached - v.Cached, u.CacheWrite - v.CacheWrite, u.Output - v.Output}
}

func (u codexUsage) negative() bool {
	return u.Input < 0 || u.Cached < 0 || u.CacheWrite < 0 || u.Output < 0
}

func (u codexUsage) tokens() Tokens {
	return Tokens{
		Input:      max(u.Input-u.Cached, 0),
		Output:     max(u.Output, 0),
		CacheWrite: max(u.CacheWrite, 0),
		CacheRead:  max(u.Cached, 0),
	}
}

// codexParser reads one Codex rollout file.
type codexParser struct {
	session, project string
	total            *codexUsage // of the last token_count event
}

// newCodexParser returns the parser of the rollout at path, named
// rollout-<time>-<session uuid>.jsonl.
func newCodexParser(path string) *codexParser {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	const uuidLen = 36
	if len(name) > uuidLen && name[len(name)-uuidLen-1] == '-' {
		name = name[len(name)-uuidLen:]
	}
	return &codexParser{session: name}
}

func (p *codexParser) parse(line []byte) (Entry, bool, error) {
	var l codexLine
	if err := decode(line, &l); err != nil {
		return Entry{}, false, err
	}
	switch l.Type {
	case "session_meta":
		// Written when the session starts and again each time it is
		// resumed. A subagent's thread counts for its parent session.
		if id := cmp.Or(l.Payload.SessionID, l.Payload.ID); id != "" {
			p.session = id
		}
		if l.Payload.CWD != "" {
			p.project = projectName(l.Payload.CWD)
		}
	case "turn_context":
		if l.Payload.CWD != "" {
			p.project = projectName(l.Payload.CWD)
		}
	}
	if l.Timestamp.IsZero() {
		return Entry{}, false, nil
	}
	e := Entry{Agent: Codex, Session: p.session, Project: p.project, Time: l.Timestamp}
	if l.Type != "event_msg" || l.Payload.Type != "token_count" || l.Payload.Info == nil {
		return e, true, nil
	}
	// The event carries the running total and the last response's own
	// counts, and is repeated without a new response. The growth of the
	// total is what the response used; the event's own counts stand in
	// for the first event of a file and when the total went backwards.
	total, last := l.Payload.Info.Total, l.Payload.Info.Last
	var used codexUsage
	switch {
	case total == nil && last == nil:
		return e, true, nil
	case total == nil:
		used = *last
	case p.total == nil || total.sub(*p.total).negative():
		used = *total
		if last != nil {
			used = *last
		}
	default:
		used = total.sub(*p.total)
	}
	if total != nil {
		p.total = total
	}
	e.Tokens = used.tokens()
	e.Message = e.Tokens != Tokens{}
	return e, true, nil
}
