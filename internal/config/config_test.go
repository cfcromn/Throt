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
		{name: "unknown key", body: `{"sample_intervl":"5s"}`, wantErr: "unknown field"},
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
		{name: "bad scheduler mode", body: `{"scheduler":{"mode":"auto"}}`, wantErr: "scheduler.mode"},
		{name: "unknown scheduler key", body: `{"scheduler":{"limit":3}}`, wantErr: "unknown field"},
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
