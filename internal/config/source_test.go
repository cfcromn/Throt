package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEffectiveSources(t *testing.T) {
	path := writeConfig(t, `{"profile": "cool", "scheduler": {"mode": "observe", "adaptive": false}}`)
	cfg, settings, err := Effective(path)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := Load(path); Diff(want, cfg) != nil {
		t.Fatalf("Effective config differs from Load: %v", Diff(want, cfg))
	}
	got := map[string]Source{}
	for _, s := range settings {
		got[s.Key] = s.Source
	}
	want := map[string]Source{
		"profile":                     SourceFile,
		"scheduler.mode":              SourceFile,
		"scheduler.adaptive":          SourceFile,
		"scheduler.heavy_limit":       SourceProfile,
		"scheduler.pause_on_critical": SourceProfile,
		"scheduler.resume_cooldown":   SourceDefault,
		"sample_interval":             SourceDefault,
		"agents":                      SourceDefault,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("source of %s = %q, want %q", k, got[k], w)
		}
	}
	if len(settings) != len(flatten(cfg)) {
		t.Errorf("got %d settings, want one per key", len(settings))
	}
}

func TestEffectiveMissingFile(t *testing.T) {
	_, settings, err := Effective(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range settings {
		// The default profile, balanced, still accounts for the keys it sets.
		want := SourceDefault
		if s.Key == "scheduler.heavy_limit" || s.Key == "scheduler.adaptive" {
			want = SourceProfile
		}
		if s.Source != want {
			t.Errorf("source of %s = %q, want %q", s.Key, s.Source, want)
		}
	}
}

func TestEffectiveInvalid(t *testing.T) {
	if _, _, err := Effective(writeConfig(t, `{"bogus": 1}`)); err == nil {
		t.Fatal("want an error")
	}
}
