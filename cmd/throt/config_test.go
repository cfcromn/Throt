package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/config"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigCheckOK(t *testing.T) {
	path := writeConfigFile(t, `{"profile": "cool", "scheduler": {"mode": "observe"}}`)
	var b bytes.Buffer
	if err := configCheck(&b, path, 8); err != nil {
		t.Fatal(err)
	}
	def := config.Default().Scheduler
	want := "ok: " + path + "\n" +
		"  profile balanced -> cool (file)\n" +
		"  scheduler.mode off -> observe (file)\n" +
		fmt.Sprintf("  scheduler.heavy_limit %d -> 1 (profile)\n", def.HeavyLimit) +
		fmt.Sprintf("  scheduler.start_limit %d -> 1 (profile)\n", def.StartLimit) +
		"  scheduler.pause_on_critical false -> true (profile)\n"
	if got := b.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigCheckSameAsDefaults(t *testing.T) {
	var b bytes.Buffer
	if err := configCheck(&b, writeConfigFile(t, `{}`), 8); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "same as the defaults") {
		t.Errorf("output = %q", b.String())
	}
}

func TestConfigCheckInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key": `{"bogus": true}`,
		"bad mode":    `{"scheduler": {"mode": "always"}}`,
		"bad json":    `{`,
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			if err := configCheck(&b, writeConfigFile(t, body), 8); err == nil {
				t.Fatalf("want an error, output %q", b.String())
			}
			if strings.Contains(b.String(), "ok") {
				t.Errorf("printed %q for an invalid file", b.String())
			}
		})
	}
	var b bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.json")
	if err := configCheck(&b, missing, 8); err != nil {
		t.Errorf("missing file: %v", err)
	}
	if want := "no config file at " + missing + "; defaults apply\nsame as the defaults\n"; b.String() != want {
		t.Errorf("missing file: output %q, want %q", b.String(), want)
	}
}

func TestConfigCheckWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"enforce with a fixed limit of every CPU", `{"scheduler": {"mode": "enforce", "heavy_limit": 4, "adaptive": false}}`, "scheduler.heavy_limit 4 is at least the CPU count (4) and adaptive is off"},
		{"pause_on_critical while off", `{"scheduler": {"pause_on_critical": true}}`, "scheduler.pause_on_critical has no effect"},
		{"shadowed built-in", `{"agents": [{"name": "mine", "executables": ["claude"]}]}`, `agents.mine: executable "claude" shadows the built-in "claude" adapter`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := configCheck(&b, writeConfigFile(t, tc.body), 4); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), "warning: "+tc.want) {
				t.Errorf("output %q lacks warning %q", b.String(), tc.want)
			}
		})
	}
	// An adaptive ceiling of every CPU is the balanced default, not a
	// mistake.
	for _, body := range []string{
		`{"scheduler": {"mode": "enforce"}}`,
		`{"profile": "balanced", "scheduler": {"mode": "enforce", "heavy_limit": 4}}`,
	} {
		var b bytes.Buffer
		if err := configCheck(&b, writeConfigFile(t, body), 4); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "at least the CPU count") {
			t.Errorf("%s: unexpected warning: %q", body, b.String())
		}
	}
	// None of them fire for a careful config.
	var b bytes.Buffer
	if err := configCheck(&b, writeConfigFile(t, `{"scheduler": {"mode": "enforce", "heavy_limit": 3}, "agents": [{"name": "mine", "executables": ["my-agent"]}]}`), 4); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "warning") {
		t.Errorf("unexpected warning: %q", b.String())
	}
}

func TestConfigShowText(t *testing.T) {
	path := writeConfigFile(t, `{"profile": "cool", "scheduler": {"mode": "observe"}}`)
	var b bytes.Buffer
	if err := configShow(&b, path, false); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"file:    " + path, "profile: cool", "scheduler.mode", "observe"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for key, src := range map[string]string{"scheduler.mode": "file", "scheduler.heavy_limit": "profile", "sample_interval": "default"} {
		found := false
		for _, line := range strings.Split(got, "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == key {
				found = true
				if f[2] != src {
					t.Errorf("%s source = %s, want %s", key, f[2], src)
				}
			}
		}
		if !found {
			t.Errorf("no row for %s:\n%s", key, got)
		}
	}
}

func TestConfigShowJSON(t *testing.T) {
	path := writeConfigFile(t, `{"scheduler": {"mode": "observe", "heavy_limit": 3}}`)
	var b bytes.Buffer
	if err := configShow(&b, path, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Path     string           `json:"path"`
		File     bool             `json:"file"`
		Profile  string           `json:"profile"`
		Config   config.Config    `json:"config"`
		Settings []config.Setting `json:"settings"`
	}
	dec := json.NewDecoder(&b)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc.Path != path || !doc.File || doc.Profile != "balanced" || doc.Config.Scheduler.HeavyLimit != 3 {
		t.Errorf("doc = %+v", doc)
	}
	src := map[string]config.Source{}
	for _, s := range doc.Settings {
		src[s.Key] = s.Source
	}
	if src["scheduler.heavy_limit"] != config.SourceFile || src["scheduler.adaptive"] != config.SourceProfile || src["profile"] != config.SourceDefault {
		t.Errorf("sources = %v", src)
	}
}

func TestConfigShowMissingFile(t *testing.T) {
	var b bytes.Buffer
	if err := configShow(&b, filepath.Join(t.TempDir(), "none.json"), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "not found, defaults apply") {
		t.Errorf("output = %q", b.String())
	}
}

func TestRunConfigArgs(t *testing.T) {
	path := writeConfigFile(t, `{}`)
	var b bytes.Buffer
	if err := runConfig([]string{"check", "--config", path}, "/nonexistent", &b); err != nil {
		t.Fatalf("--config after the subcommand: %v", err)
	}
	if err := runConfig([]string{"show", "--json"}, path, &b); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"edit"}, {"check", "--json"}, {"show", "extra"}} {
		if err := runConfig(args, path, &b); err == nil {
			t.Errorf("runConfig(%q): want a usage error", args)
		}
	}
}
