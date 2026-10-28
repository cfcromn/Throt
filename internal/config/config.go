// Package config loads Throt's user configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config is the user-facing configuration. Unknown keys are rejected.
type Config struct {
	// SocketPath is the daemon's Unix socket. Empty means DefaultSocketPath.
	SocketPath string `json:"socket_path"`
	// SampleInterval is how often the daemon samples metrics.
	SampleInterval Duration `json:"sample_interval"`
	// EventHistory is how many recent events the daemon retains in memory.
	EventHistory int `json:"event_history"`
}

// Duration is a time.Duration that reads and writes strings like "2s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"2s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		SocketPath:     DefaultSocketPath(),
		SampleInterval: Duration(2 * time.Second),
		EventHistory:   1000,
	}
}

// DefaultPath is $XDG_CONFIG_HOME/throt/config.json, falling back to
// ~/.config/throt/config.json on every platform.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "throt", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate config: %w", err)
	}
	return filepath.Join(home, ".config", "throt", "config.json"), nil
}

// DefaultSocketPath is $XDG_RUNTIME_DIR/throt/throtd.sock, falling back to a
// per-user directory under the OS temp dir.
func DefaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "throt", "throtd.sock")
	}
	return filepath.Join(os.TempDir(), "throt-"+strconv.Itoa(os.Getuid()), "throtd.sock")
}

// Load reads path, applying defaults for omitted fields. A missing file yields
// Default(); unknown keys and invalid values are errors.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports the first invalid field.
func (c Config) Validate() error {
	if time.Duration(c.SampleInterval) < 100*time.Millisecond {
		return errors.New("sample_interval must be at least 100ms")
	}
	if c.EventHistory < 1 || c.EventHistory > 100_000 {
		return errors.New("event_history must be between 1 and 100000")
	}
	return nil
}
