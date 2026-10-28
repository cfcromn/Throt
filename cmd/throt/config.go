package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"text/tabwriter"

	"github.com/cfcromn/Throt/internal/agent"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
)

const configUsage = "usage: throt config check|show [--config PATH] [--json]"

// cmdConfig is 'throt config'.
func cmdConfig(_ context.Context, e env, args []string) error {
	return runConfig(args, e.cfgPath, e.stdout)
}

// runConfig handles 'throt config check' and 'throt config show'. Both load
// the file with config.Load, as throtd does.
func runConfig(args []string, cfgPath string, w io.Writer) error {
	if len(args) == 0 {
		return usageLine(configUsage)
	}
	sub := args[0]
	if sub == "-h" || sub == "-help" || sub == "--help" {
		fmt.Fprintln(w, configUsage)
		return flag.ErrHelp
	}
	cfs := flag.NewFlagSet("throt config "+sub, flag.ContinueOnError)
	cfs.SetOutput(io.Discard)
	cfs.Usage = func() { fmt.Fprintln(cfs.Output(), configUsage); cfs.PrintDefaults() }
	cfs.StringVar(&cfgPath, "config", cfgPath, "path to config file")
	asJSON := false
	if sub == "show" {
		cfs.BoolVar(&asJSON, "json", false, "print the effective config as JSON")
	}
	if sub != "check" && sub != "show" {
		return usageLine(configUsage)
	}
	if err := parseFlags(cfs, args[1:], w); err != nil {
		return err
	} else if cfs.NArg() > 0 {
		return usageLine(configUsage)
	}
	switch sub {
	case "check":
		return configCheck(w, cfgPath, runtime.NumCPU())
	case "show":
		return configShow(w, cfgPath, asJSON)
	}
	return usageLine(configUsage)
}

// configCheck validates the file at path and prints "ok", what it changes
// from the defaults, and warnings about settings that are allowed but risky
// or ineffective. An invalid file is an error; a missing one is not, since
// throtd then uses the defaults.
func configCheck(w io.Writer, path string, ncpu int) error {
	cfg, settings, err := config.Effective(path)
	if err != nil {
		return err
	}
	source := make(map[string]config.Source, len(settings))
	for _, s := range settings {
		source[s.Key] = s.Source
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(w, "no config file at %s; defaults apply\n", path)
	} else {
		fmt.Fprintln(w, "ok: "+path)
	}
	changes := config.Diff(config.Default(), cfg)
	if len(changes) == 0 {
		fmt.Fprintln(w, "same as the defaults")
	}
	for _, c := range changes {
		fmt.Fprintf(w, "  %s (%s)\n", c, source[c.Key])
	}
	for _, msg := range configWarnings(cfg, ncpu) {
		fmt.Fprintln(w, "warning: "+msg)
	}
	return nil
}

// configWarnings lists valid settings that are likely mistakes.
func configWarnings(cfg config.Config, ncpu int) []string {
	var out []string
	s := cfg.Scheduler
	// With adaptive the heavy limit is only a ceiling, which the balanced
	// profile sets to the CPU count on purpose.
	if s.Mode == "enforce" && !s.Adaptive && s.HeavyLimit >= ncpu {
		out = append(out, fmt.Sprintf("scheduler.heavy_limit %d is at least the CPU count (%d) and adaptive is off, so enforce mode will rarely queue anything", s.HeavyLimit, ncpu))
	}
	if s.Mode == "off" && s.PauseOnCritical {
		out = append(out, "scheduler.pause_on_critical has no effect while scheduler.mode is off")
	}
	builtin := agent.DefaultAdapters()
	for _, a := range cfg.Agents {
		for _, e := range a.Executables {
			for _, b := range builtin {
				if b.Match(domain.ProcessSample{Name: e}) {
					out = append(out, fmt.Sprintf("agents.%s: executable %q shadows the built-in %q adapter", a.Name, e, b.Name()))
				}
			}
		}
	}
	return out
}

// configShowJSON is the 'throt config show --json' document.
type configShowJSON struct {
	Path     string           `json:"path"`
	File     bool             `json:"file"`
	Profile  string           `json:"profile"`
	Config   config.Config    `json:"config"`
	Settings []config.Setting `json:"settings"`
}

// configShow prints the effective config (defaults, then the profile, then
// the file) and where each value came from.
func configShow(w io.Writer, path string, asJSON bool) error {
	cfg, settings, err := config.Effective(path)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(configShowJSON{Path: path, File: statErr == nil, Profile: cfg.Profile, Config: cfg, Settings: settings})
	}
	file := path
	if statErr != nil {
		file += " (not found, defaults apply)"
	}
	fmt.Fprintln(w, "file:    "+file)
	fmt.Fprintln(w, "profile: "+cfg.Profile)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	for _, s := range settings {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Key, s.Value, s.Source)
	}
	return tw.Flush()
}
