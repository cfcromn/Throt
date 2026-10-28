package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidationErrors gives every check in Validate, and Load's own
// refusals, a file that trips it, so a dropped or loosened check fails here.
func TestValidationErrors(t *testing.T) {
	agents := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"a%d","executables":["agent%d"]}`, i, i)
		}
		return `{"agents":[` + b.String() + `]}`
	}
	executables := func(n int) string {
		var names []string
		for i := range n {
			names = append(names, fmt.Sprintf(`"x%d"`, i))
		}
		return `{"agents":[{"name":"x","executables":[` + strings.Join(names, ",") + `]}]}`
	}
	tests := []struct {
		name, body, want string
	}{
		{"duration not a string", `{"sample_interval":5}`, `duration must be a string like "2s"`},
		{"history too large", `{"event_history":100001}`, "event_history must be between 1 and 100000"},
		{"history negative", `{"event_history":-1}`, "event_history must be between 1 and 100000"},
		{"too many agents", agents(33), "at most 32 custom agents"},
		{"agent name empty", `{"agents":[{"name":"","executables":["x"]}]}`, `name "" must be 1-32`},
		{"agent name too long", `{"agents":[{"name":"` + strings.Repeat("a", 33) + `","executables":["x"]}]}`, "must be 1-32"},
		{"agent without executables", `{"agents":[{"name":"x","executables":[]}]}`, "agents.x: list 1-16 executables"},
		{"agent with too many executables", executables(17), "agents.x: list 1-16 executables"},
		{"empty executable", `{"agents":[{"name":"x","executables":[""]}]}`, "must be a bare base name"},
		{"executable with a space", `{"agents":[{"name":"x","executables":["my agent"]}]}`, "must be a bare base name"},
		{"executable with a tab", `{"agents":[{"name":"x","executables":["my\tagent"]}]}`, "must be a bare base name"},
		{"per-agent limit negative", `{"scheduler":{"per_agent_limit":-1}}`, "scheduler.per_agent_limit must be between 0 and 64"},
		{"per-agent limit too large", `{"scheduler":{"per_agent_limit":65}}`, "scheduler.per_agent_limit must be between 0 and 64"},
		{"heavy limit zero", `{"scheduler":{"heavy_limit":0}}`, "scheduler.heavy_limit must be between 1 and 64"},
		{"heavy limit too large", `{"scheduler":{"heavy_limit":65}}`, "scheduler.heavy_limit must be between 1 and 64"},
		{"start limit above heavy limit", `{"scheduler":{"heavy_limit":2,"start_limit":3}}`, "scheduler.start_limit must be between 0 and scheduler.heavy_limit (2)"},
		{"start limit negative", `{"scheduler":{"start_limit":-1}}`, "scheduler.start_limit must be between 0"},
		{"resume cooldown negative", `{"scheduler":{"resume_cooldown":"-1s"}}`, "scheduler.resume_cooldown must be >= 0"},
		{"max queue wait too short", `{"scheduler":{"max_queue_wait":"999ms"}}`, "scheduler.max_queue_wait >= 1s"},
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
			if !strings.Contains(err.Error(), path) {
				t.Errorf("err = %v does not name the file", err)
			}
		})
	}
}

// TestValidationBoundaries checks that the limits themselves are accepted.
func TestValidationBoundaries(t *testing.T) {
	for _, body := range []string{
		`{"event_history":1}`,
		`{"event_history":100000}`,
		`{"agents":[{"name":"` + strings.Repeat("a", 32) + `","executables":["x"]}]}`,
		`{"scheduler":{"per_agent_limit":64,"heavy_limit":64,"start_limit":64}}`,
		`{"scheduler":{"heavy_limit":1,"start_limit":0}}`,
		`{"scheduler":{"resume_cooldown":"0s","max_queue_wait":"1s"}}`,
		`{"sample_interval":"100ms"}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestLoadReadError(t *testing.T) {
	// A directory exists but cannot be read as a file; that is not "no
	// config", so Load must not fall back to the defaults.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.HasPrefix(err.Error(), "read config:") {
		t.Fatalf("err = %v, want a read error", err)
	}
}

func TestEmptySocketPathIsDefault(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"socket_path":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.SocketPath != "/run/user/test/throt/throtd.sock" {
		t.Errorf("socket_path = %q, want the default", c.SocketPath)
	}
}

func TestDefaultPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, err := DefaultPath(); err != nil || p != "/xdg/throt/config.json" {
		t.Errorf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, err := DefaultPath(); err != nil || p != "/home/u/.config/throt/config.json" {
		t.Errorf("DefaultPath without XDG = %q, %v", p, err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	want := filepath.Join(os.TempDir(), fmt.Sprintf("throt-%d", os.Getuid()), "throtd.sock")
	if p := DefaultSocketPath(); p != want {
		t.Errorf("DefaultSocketPath = %q, want %q", p, want)
	}
}
