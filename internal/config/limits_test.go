package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadBody(t *testing.T, body string, ncpu int) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return load(path, ncpu)
}

func TestProfileLimits(t *testing.T) {
	type limits struct {
		heavy, start int
		adaptive     bool
	}
	tests := []struct {
		profile string
		ncpu    int
		want    limits
	}{
		{"balanced", 1, limits{2, 2, true}},
		{"balanced", 5, limits{5, 2, true}},
		{"balanced", 16, limits{16, 4, true}},
		{"balanced", 200, limits{64, 50, true}},
		{"performance", 1, limits{2, 2, false}},
		{"performance", 5, limits{2, 2, false}},
		{"performance", 16, limits{8, 8, false}},
		{"performance", 200, limits{64, 64, false}},
		{"cool", 16, limits{1, 1, true}},
	}
	for _, tt := range tests {
		c, err := loadBody(t, `{"profile":"`+tt.profile+`"}`, tt.ncpu)
		if err != nil {
			t.Fatalf("%s on %d CPUs: %v", tt.profile, tt.ncpu, err)
		}
		s := c.Scheduler
		if got := (limits{s.HeavyLimit, s.StartLimit, s.Adaptive}); got != tt.want {
			t.Errorf("%s on %d CPUs: got %+v, want %+v", tt.profile, tt.ncpu, got, tt.want)
		}
	}
	// No profile key is balanced.
	if c, err := loadBody(t, `{}`, 16); err != nil || c.Scheduler.HeavyLimit != 16 || c.Scheduler.StartLimit != 4 {
		t.Errorf("default profile: %+v, %v", c.Scheduler, err)
	}
}

func TestStartLimitOverrides(t *testing.T) {
	tests := []struct {
		name, body   string
		heavy, start int
		wantErr      string
	}{
		// An explicit heavy_limit keeps working: the derived start is
		// capped at it.
		{name: "heavy_limit below the derived start", body: `{"scheduler":{"heavy_limit":1}}`, heavy: 1, start: 1},
		{name: "heavy_limit between", body: `{"scheduler":{"heavy_limit":3}}`, heavy: 3, start: 3},
		{name: "heavy_limit above", body: `{"scheduler":{"heavy_limit":12}}`, heavy: 12, start: 4},
		{name: "cool with a higher ceiling", body: `{"profile":"cool","scheduler":{"heavy_limit":4}}`, heavy: 4, start: 1},
		{name: "explicit start", body: `{"scheduler":{"heavy_limit":12,"start_limit":6}}`, heavy: 12, start: 6},
		{name: "start at the ceiling", body: `{"scheduler":{"start_limit":16}}`, heavy: 16, start: 16},
		{name: "zero derives", body: `{"scheduler":{"heavy_limit":12,"start_limit":0}}`, heavy: 12, start: 4},
		{name: "start above the ceiling", body: `{"scheduler":{"heavy_limit":3,"start_limit":4}}`, wantErr: "start_limit must be between 0 and scheduler.heavy_limit (3)"},
		{name: "start above the profile ceiling", body: `{"profile":"cool","scheduler":{"start_limit":2}}`, wantErr: "start_limit"},
		{name: "negative start", body: `{"scheduler":{"start_limit":-1}}`, wantErr: "start_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := loadBody(t, tt.body, 16)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Scheduler.HeavyLimit != tt.heavy || c.Scheduler.StartLimit != tt.start {
				t.Fatalf("heavy %d start %d, want %d %d", c.Scheduler.HeavyLimit, c.Scheduler.StartLimit, tt.heavy, tt.start)
			}
		})
	}
}

func TestDefaultStartLimitValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
	if s := Default().Scheduler; s.StartLimit < 1 || s.StartLimit > s.HeavyLimit {
		t.Fatalf("default start %d outside [1, %d]", s.StartLimit, s.HeavyLimit)
	}
}
