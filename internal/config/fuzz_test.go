package config

import (
	"os"
	"path/filepath"
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
