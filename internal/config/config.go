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
	"runtime"
	"strconv"
	"strings"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/pressure"
	"time"
)

// MaxSampleInterval is the longest sample interval: the pressure window
// must hold at least three samples, or its medians come from one noisy
// reading and swap growth (a rate between samples) is never measured.
const MaxSampleInterval = pressure.DefaultWindow / 3

// MaxResumeCooldown is the longest resume_cooldown. The scheduler resumes
// at most one queued workload per cooldown, so a longer one would leave
// queued work stopped for no benefit.
const MaxResumeCooldown = 10 * time.Minute

// Config is the user-facing configuration. Unknown keys are rejected.
type Config struct {
	// SocketPath is the daemon's Unix socket. Empty means DefaultSocketPath.
	SocketPath string `json:"socket_path"`
	// SampleInterval is how often the daemon samples metrics.
	SampleInterval Duration `json:"sample_interval"`
	// EventHistory is how many recent events the daemon retains in memory.
	EventHistory int `json:"event_history"`
	// Profile is a preset for the scheduler:
	// "performance", "balanced" (default), or "cool". Explicit settings
	// override it.
	Profile string `json:"profile"`
	// Scheduler configures automatic heavy-workload scheduling.
	Scheduler Scheduler `json:"scheduler"`
	// Agents adds agent adapters beyond the built-in ones. They are matched
	// first, so they can also override a built-in name.
	Agents []Agent `json:"agents"`
}

// Agent recognizes a coding agent by its exact executable base names.
type Agent struct {
	// Name labels instances, e.g. "myagent" gives "myagent-1".
	Name        string   `json:"name"`
	Executables []string `json:"executables"`
}

// genericRuntimes may run anything, so matching them would attribute
// unrelated processes to an agent.
var genericRuntimes = map[string]bool{
	"node": true, "python": true, "python3": true, "bun": true, "deno": true,
	"ruby": true, "java": true, "sh": true, "bash": true, "zsh": true, "fish": true,
	"perl": true, "php": true, "lua": true, "env": true, "npx": true, "uvx": true, "tsx": true,
}

// Scheduler is the automatic scheduling policy. It is off by default.
type Scheduler struct {
	// Mode is "off", "observe" (log decisions, never act), or "enforce".
	Mode string `json:"mode"`
	// HeavyLimit is how many heavy workloads may run at once. Unattributed
	// ones count toward it but are never queued. With Adaptive it is the
	// ceiling the effective limit grows to.
	HeavyLimit int `json:"heavy_limit"`
	// StartLimit is where the adaptive limit starts, between 1 and
	// HeavyLimit. 0 derives it from the profile, capped at HeavyLimit.
	// Without Adaptive it is ignored and HeavyLimit is the fixed limit.
	StartLimit int `json:"start_limit"`
	// ResumeCooldown is the minimum time between two scheduler actions.
	ResumeCooldown Duration `json:"resume_cooldown"`
	// PauseOnCritical lets the scheduler pause a running workload (never
	// the last one) under critical pressure.
	PauseOnCritical bool `json:"pause_on_critical"`
	// MaxQueueWait is how long a queued workload waits before it outranks
	// all others.
	MaxQueueWait Duration `json:"max_queue_wait"`
	// Adaptive starts the running limit at StartLimit, lowers it under
	// pressure and raises it (up to HeavyLimit) while the machine copes.
	Adaptive bool `json:"adaptive"`
	// PerAgentLimit caps heavy workloads per agent instance; 0 is no cap.
	PerAgentLimit int `json:"per_agent_limit"`
}

// Profile presets for a machine with ncpu CPUs. The scheduler mode is never
// part of a profile: enabling automatic control is always an explicit
// choice.
//
// Balanced is adaptive with a ceiling of every CPU and a modest start, so
// the limit can grow while only the CPUs are busy (see
// scheduler.Input.ThroughputOnly). Performance stays a fixed limit, since
// adaptive control would also halve it under memory pressure; its ceiling
// is its fixed limit, so it keeps its old value rather than 2x CPUs, which
// would hardly ever queue anything.
var profiles = map[string]func(c *Config, ncpu int){
	"performance": func(c *Config, ncpu int) {
		c.Scheduler.HeavyLimit = min(64, max(2, ncpu/2))
		c.Scheduler.StartLimit = c.Scheduler.HeavyLimit
		c.Scheduler.Adaptive = false
	},
	"balanced": func(c *Config, ncpu int) {
		c.Scheduler.HeavyLimit = min(64, max(2, ncpu))
		c.Scheduler.StartLimit = min(64, max(2, ncpu/4))
		c.Scheduler.Adaptive = true
	},
	"cool": func(c *Config, ncpu int) {
		c.Scheduler.HeavyLimit = 1
		c.Scheduler.StartLimit = 1
		c.Scheduler.Adaptive = true
		c.Scheduler.PauseOnCritical = true
	},
}

// derivedStart is the start limit for profile on ncpu CPUs, capped at
// heavyLimit. An unknown or empty profile derives as balanced.
func derivedStart(profile string, ncpu, heavyLimit int) int {
	apply, ok := profiles[profile]
	if !ok {
		apply = profiles["balanced"]
	}
	var c Config
	apply(&c, ncpu)
	return min(c.Scheduler.StartLimit, heavyLimit)
}

// defaultProfile is the profile used when none is set.
const defaultProfile = "balanced"

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
	c := Config{
		SocketPath:     DefaultSocketPath(),
		SampleInterval: Duration(2 * time.Second),
		EventHistory:   1000,
		Scheduler: Scheduler{
			Mode:           "off",
			HeavyLimit:     2,
			ResumeCooldown: Duration(10 * time.Second),
			MaxQueueWait:   Duration(5 * time.Minute),
		},
		Profile: defaultProfile,
	}
	profiles[c.Profile](&c, runtime.NumCPU())
	return c
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
	return load(path, runtime.NumCPU())
}

func load(path string, ncpu int) (Config, error) {
	cfg := Default()
	profiles[cfg.Profile](&cfg, ncpu)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// Apply the profile's presets first, then decode the file again over
	// them so explicit settings win.
	var head struct {
		Profile   string `json:"profile"`
		Scheduler struct {
			StartLimit *int `json:"start_limit"`
		} `json:"scheduler"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return cfg, parseError(path, b, err)
	}
	if head.Profile != "" {
		apply, ok := profiles[head.Profile]
		if !ok {
			return cfg, fmt.Errorf("config %s: profile must be performance, balanced, or cool, not %q", path, head.Profile)
		}
		apply(&cfg, ncpu)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, parseError(path, b, err)
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	if cfg.Profile == "" {
		// An explicit "" is the default profile, whose presets apply.
		cfg.Profile = defaultProfile
	}
	if head.Scheduler.StartLimit == nil || *head.Scheduler.StartLimit == 0 {
		// An explicit heavy_limit below the profile's start caps it, so a
		// file written before start_limit existed stays valid.
		cfg.Scheduler.StartLimit = derivedStart(cfg.Profile, ncpu, cfg.Scheduler.HeavyLimit)
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
	if time.Duration(c.SampleInterval) > MaxSampleInterval {
		return fmt.Errorf("sample_interval must be at most %v, so the %v pressure window holds several samples", MaxSampleInterval, pressure.DefaultWindow)
	}
	if c.EventHistory < 1 || c.EventHistory > 100_000 {
		return errors.New("event_history must be between 1 and 100000")
	}
	switch c.Scheduler.Mode {
	case "off", "observe", "enforce":
	default:
		return fmt.Errorf("scheduler.mode must be off, observe, or enforce, not %q", c.Scheduler.Mode)
	}
	if len(c.Agents) > 32 {
		return errors.New("agents: at most 32 custom agents")
	}
	for _, a := range c.Agents {
		if !validLabel(a.Name) {
			return fmt.Errorf("agents: name %q must be 1-32 lowercase letters, digits, or dashes", a.Name)
		}
		if len(a.Executables) == 0 || len(a.Executables) > 16 {
			return fmt.Errorf("agents.%s: list 1-16 executables", a.Name)
		}
		for _, e := range a.Executables {
			switch {
			case e == "" || strings.ContainsAny(e, "/ \t"):
				return fmt.Errorf("agents.%s: executable %q must be a bare base name", a.Name, e)
			case genericRuntimes[e]:
				return fmt.Errorf("agents.%s: %q is a generic runtime and would match unrelated processes", a.Name, e)
			case agent.Classify(domain.ProcessSample{Name: e}).Heavy():
				// It would turn every such build into an "agent" and take
				// real agents' builds away from them.
				return fmt.Errorf("agents.%s: %q is a build tool, not an agent", a.Name, e)
			}
		}
	}
	if c.Scheduler.PerAgentLimit < 0 || c.Scheduler.PerAgentLimit > 64 {
		return errors.New("scheduler.per_agent_limit must be between 0 and 64")
	}
	if c.Scheduler.HeavyLimit < 1 || c.Scheduler.HeavyLimit > 64 {
		return errors.New("scheduler.heavy_limit must be between 1 and 64")
	}
	if c.Scheduler.StartLimit < 0 || c.Scheduler.StartLimit > c.Scheduler.HeavyLimit {
		return fmt.Errorf("scheduler.start_limit must be between 0 and scheduler.heavy_limit (%d)", c.Scheduler.HeavyLimit)
	}
	if c.Scheduler.ResumeCooldown < 0 || c.Scheduler.MaxQueueWait < Duration(time.Second) {
		return errors.New("scheduler.resume_cooldown must be >= 0 and scheduler.max_queue_wait >= 1s")
	}
	if time.Duration(c.Scheduler.ResumeCooldown) > MaxResumeCooldown {
		// Resumes are spaced by the cooldown, so a huge one freezes the queue.
		return fmt.Errorf("scheduler.resume_cooldown must be at most %v", MaxResumeCooldown)
	}
	return nil
}

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
