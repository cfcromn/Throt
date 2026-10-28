package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzLoad(f *testing.F) {
	for _, s := range []string{
		`{"sample_interval":"5s"}`,
		`{"sample_intervl":"5s"}`,
		`{"sample_interval":"soon"}`,
		`{"event_history":0}`,
		`{"scheduler":{"mode":"enforce","heavy_limit":3}}`,
		`{"profile":"cool"}`,
		`{"profile":"cool","scheduler":{"heavy_limit":4}}`,
		`{"agents":[{"name":"myagent","executables":["my-agent","myagent-cli"]}]}`,
		`{"agents":[{"name":"x","executables":["node"]}]}`,
		`{"agents":[{"name":"x","executables":["go"]}]}`,
		`{"profile":"turbo"}`,
		`{"scheduler":{"limit":3}}`,
		`{"sample_interval":5}`, `{"Scheduler":{"Max_Queue_Wait":"x"}}`, `{"Scheduler":{"zz":1}}`,
		`null`, `{}`, `{} {}`, ``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil {
			// Every error names the file, and a parse error says where in
			// it: a line and column, or the unknown key and its section.
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("Load(%q): error does not name the file: %v", b, err)
			}
			if strings.HasPrefix(msg, "parse config ") && !strings.HasPrefix(msg, "parse config "+path+": line ") &&
				!strings.HasPrefix(msg, "parse config "+path+": unknown key ") {
				t.Errorf("Load(%q): parse error has no location: %v", b, err)
			}
			if name := unknownName(err); name != "" && !strings.Contains(msg, fmt.Sprintf("unknown key %q", name)) {
				t.Errorf("Load(%q): error points at another key than %q: %v", b, name, err)
			}
			return
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Load(%q) succeeded but Validate fails: %v", b, err)
		}
		if c.SocketPath == "" {
			t.Errorf("Load(%q): empty socket path", b)
		}
	})
}
