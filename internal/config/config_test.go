package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		body    string // empty means no file
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{name: "missing file uses defaults", check: func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c, Default()) {
				t.Errorf("got %+v, want defaults", c)
			}
		}},
		{name: "partial override", body: `{"sample_interval":"5s"}`, check: func(t *testing.T, c Config) {
			if time.Duration(c.SampleInterval) != 5*time.Second || c.EventHistory != Default().EventHistory {
				t.Errorf("got %+v", c)
			}
		}},
		{name: "unknown key", body: `{"sample_intervl":"5s"}`, wantErr: `unknown key "sample_intervl" at the top level; did you mean "sample_interval"?`},
		{name: "bad duration", body: `{"sample_interval":"soon"}`, wantErr: "invalid duration"},
		{name: "interval too small", body: `{"sample_interval":"1ms"}`, wantErr: "at least 100ms"},
		// Longer intervals leave the 30s pressure window a single sample:
		// no swap growth rate, and medians of one noisy reading.
		{name: "interval too large", body: `{"sample_interval":"31s"}`, wantErr: "at most 10s"},
		{name: "largest interval", body: `{"sample_interval":"10s"}`, check: func(t *testing.T, c Config) {
			if time.Duration(c.SampleInterval) != 10*time.Second {
				t.Errorf("got %v", c.SampleInterval)
			}
		}},
		{name: "history out of range", body: `{"event_history":0}`, wantErr: "event_history"},
		{name: "scheduler override", body: `{"scheduler":{"mode":"enforce","heavy_limit":3}}`, check: func(t *testing.T, c Config) {
			if c.Scheduler.Mode != "enforce" || c.Scheduler.HeavyLimit != 3 || c.Scheduler.MaxQueueWait != Default().Scheduler.MaxQueueWait {
				t.Errorf("got %+v", c.Scheduler)
			}
		}},
		{name: "cool profile", body: `{"profile":"cool"}`, check: func(t *testing.T, c Config) {
			if c.Scheduler.HeavyLimit != 1 || !c.Scheduler.Adaptive || !c.Scheduler.PauseOnCritical || c.Scheduler.Mode != "off" {
				t.Errorf("got %+v", c.Scheduler)
			}
		}},
		{name: "explicit setting overrides profile", body: `{"profile":"cool","scheduler":{"heavy_limit":4}}`, check: func(t *testing.T, c Config) {
			if c.Scheduler.HeavyLimit != 4 || !c.Scheduler.PauseOnCritical {
				t.Errorf("got %+v", c.Scheduler)
			}
		}},
		{name: "custom agent", body: `{"agents":[{"name":"myagent","executables":["my-agent","myagent-cli"]}]}`, check: func(t *testing.T, c Config) {
			if len(c.Agents) != 1 || c.Agents[0].Executables[1] != "myagent-cli" {
				t.Errorf("got %+v", c.Agents)
			}
		}},
		{name: "custom agent generic runtime", body: `{"agents":[{"name":"x","executables":["node"]}]}`, wantErr: "generic runtime"},
		{name: "custom agent build tool", body: `{"agents":[{"name":"x","executables":["go"]}]}`, wantErr: "build tool"},
		{name: "custom agent path", body: `{"agents":[{"name":"x","executables":["/usr/bin/x"]}]}`, wantErr: "bare base name"},
		{name: "custom agent bad name", body: `{"agents":[{"name":"My Agent","executables":["x"]}]}`, wantErr: "lowercase"},
		{name: "unknown profile", body: `{"profile":"turbo"}`, wantErr: "profile must be"},
		{name: "empty profile is the default", body: `{"profile":""}`, check: func(t *testing.T, c Config) {
			if !reflect.DeepEqual(c, Default()) {
				t.Errorf("got %+v, want defaults", c)
			}
		}},
		{name: "bad scheduler mode", body: `{"scheduler":{"mode":"auto"}}`, wantErr: "scheduler.mode"},
		{name: "unknown scheduler key", body: `{"scheduler":{"limit":3}}`, wantErr: `unknown key "limit" in scheduler`},
		{name: "resume cooldown too long", body: `{"scheduler":{"resume_cooldown":"11m"}}`, wantErr: "scheduler.resume_cooldown must be at most 10m0s"},
		{name: "longest resume cooldown", body: `{"scheduler":{"resume_cooldown":"10m"}}`, check: func(t *testing.T, c Config) {
			if time.Duration(c.Scheduler.ResumeCooldown) != 10*time.Minute {
				t.Errorf("got %v", c.Scheduler.ResumeCooldown)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Load(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

// TestLoadErrorLocation checks that parse errors say where the problem is:
// line and column for malformed JSON and wrong types, the section and the
// closest known key for an unknown one.
func TestLoadErrorLocation(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"syntax", "{\n  \"profile\": \"cool\",\n  \"scheduler\": {\"mode\" \"enforce\"}\n}", "line 3, column 24: invalid character"},
		{"syntax first line", `{"profile" "cool"}`, "line 1, column 12: invalid character"},
		{"truncated", "{\n  \"profile\": \"cool\"", "line 2, column 20: unexpected end of JSON input"},
		{"wrong type", "{\n  \"scheduler\": {\"heavy_limit\": \"3\"}\n}", "line 2, column 34: json: cannot unmarshal string"},
		{"scheduler typo", `{"scheduler":{"mode":"enforce","heavy_limt":3}}`, `unknown key "heavy_limt" in scheduler; did you mean "heavy_limit"?`},
		{"agents typo", `{"agents":[{"name":"x","executable":["x"]}]}`, `unknown key "executable" in agents[0]; did you mean "executables"?`},
		{"no suggestion", `{"scheduler":{"turbo":true}}`, `unknown key "turbo" in scheduler`},
		// A bad duration used to carry no location, and a non-string one
		// the offset of its own value (always line 1).
		{"bad duration", "{\n\n  \"sample_interval\": \"soon\"}", `line 3, column 22: sample_interval: time: invalid duration "soon"`},
		{"duration not a string", "{\n\n\n  \"scheduler\": {\"resume_cooldown\": 5}}", `line 4, column 36: scheduler.resume_cooldown: duration must be a string like "2s"`},
		{"duration key case", "{\"scheduler\": {\"max_queue_wait\": \"1s\"},\n\"Sample_Interval\": null}", `line 2, column 20: Sample_Interval: time: invalid duration ""`},
		// The decoder accepts "Scheduler"; the unknown key is inside it.
		{"key case", `{"Scheduler":{"zz":1}}`, `unknown key "zz" in Scheduler`},
		{"duration object", `{"scheduler":{"max_queue_wait":{"a":[1]}}}`, `line 1, column 32: scheduler.max_queue_wait: duration must be`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "did you mean") != strings.Contains(tt.want, "did you mean") {
				t.Errorf("err = %v: unexpected suggestion", err)
			}
		})
	}
}

func TestEditDistance(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "", 3}, {"", "ab", 2}, {"kitten", "sitting", 3}, {"heavy_limt", "heavy_limit", 1}, {"mode", "mode", 0},
	} {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
