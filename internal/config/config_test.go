package config

import (
	"os"
	"path/filepath"
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
			if c != Default() {
				t.Errorf("got %+v, want defaults", c)
			}
		}},
		{name: "partial override", body: `{"sample_interval":"5s"}`, check: func(t *testing.T, c Config) {
			if time.Duration(c.SampleInterval) != 5*time.Second || c.EventHistory != Default().EventHistory {
				t.Errorf("got %+v", c)
			}
		}},
		{name: "unknown key", body: `{"sample_intervl":"5s"}`, wantErr: "unknown field"},
		{name: "bad duration", body: `{"sample_interval":"soon"}`, wantErr: "invalid duration"},
		{name: "interval too small", body: `{"sample_interval":"1ms"}`, wantErr: "at least 100ms"},
		{name: "history out of range", body: `{"event_history":0}`, wantErr: "event_history"},
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
