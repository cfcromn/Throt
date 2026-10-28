package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFlagsDefaults(t *testing.T) {
	o, err := parseFlags(nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if o.logFormat != "text" || o.logLevel != slog.LevelInfo || o.configPath == "" || o.version {
		t.Errorf("defaults = %+v", o)
	}
}

func TestParseFlagsLogging(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		format string
		level  slog.Level
	}{
		{[]string{"-log-format", "json"}, "json", slog.LevelInfo},
		{[]string{"-log-format=text", "-log-level=debug"}, "text", slog.LevelDebug},
		{[]string{"--log-level", "warn"}, "text", slog.LevelWarn},
		{[]string{"-log-level", "error", "-log-format", "json"}, "json", slog.LevelError},
	} {
		o, err := parseFlags(tc.args, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil {
			t.Errorf("%q: %v", tc.args, err)
			continue
		}
		if o.logFormat != tc.format || o.logLevel != tc.level {
			t.Errorf("%q: format %s level %v, want %s %v", tc.args, o.logFormat, o.logLevel, tc.format, tc.level)
		}
	}
}

func TestParseFlagsInvalid(t *testing.T) {
	for _, args := range [][]string{
		{"-log-format", "xml"},
		{"-log-level", "trace"},
		{"-log-level", "INFO"},
		{"-bogus"},
		{"extra"},
	} {
		if _, err := parseFlags(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%q: want an error", args)
		}
	}
}

func TestHelpDocumentsLogFlags(t *testing.T) {
	var b, errw bytes.Buffer
	if _, err := parseFlags([]string{"-h"}, &b, &errw); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	if errw.Len() != 0 {
		t.Errorf("-h wrote to stderr: %q", errw.String())
	}
	for _, want := range []string{"-log-format", "text or json", "-log-level", "debug, info, warn, or error", "-config"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, b.String())
		}
	}
}

// TestHelpGoesToStdout checks that -h and -help print the usage to stdout
// (main then exits 0), while a bad flag prints the error and the usage to
// stderr.
func TestHelpGoesToStdout(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		var out, errw bytes.Buffer
		if _, err := parseFlags([]string{arg}, &out, &errw); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: err = %v, want flag.ErrHelp", arg, err)
		}
		if !strings.HasPrefix(out.String(), "Usage: throtd") || !strings.Contains(out.String(), "-version") || errw.Len() != 0 {
			t.Errorf("%s: stdout %q, stderr %q", arg, out.String(), errw.String())
		}
	}
	var out, errw bytes.Buffer
	if _, err := parseFlags([]string{"-bogus"}, &out, &errw); err == nil || errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-bogus: err = %v", err)
	}
	if out.Len() != 0 || !strings.Contains(errw.String(), "-bogus") || !strings.Contains(errw.String(), "Usage: throtd") {
		t.Errorf("-bogus: stdout %q, stderr %q", out.String(), errw.String())
	}
}

func TestNewLogger(t *testing.T) {
	var b bytes.Buffer
	newLogger(&b, options{logFormat: "json", logLevel: slog.LevelInfo}).Info("hello", "k", "v")
	newLogger(&b, options{logFormat: "json", logLevel: slog.LevelInfo}).Debug("hidden")
	var line map[string]any
	if err := json.Unmarshal(b.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %q", b.String())
	}
	if line["msg"] != "hello" || line["k"] != "v" {
		t.Errorf("line = %v", line)
	}
	b.Reset()
	newLogger(&b, options{logFormat: "text", logLevel: slog.LevelDebug}).Debug("shown")
	if !strings.Contains(b.String(), "level=DEBUG msg=shown") {
		t.Errorf("text = %q", b.String())
	}
}

func TestGoroutineLogInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"":    0,
		"30s": 30 * time.Second,
		"1":   time.Minute,
		"yes": time.Minute,
		"-1s": 0,
		"0s":  0,
		"1ns": time.Second,
	} {
		if got := goroutineLogInterval(v); got != want {
			t.Errorf("goroutineLogInterval(%q) = %s, want %s", v, got, want)
		}
	}
}

// TestRunStopsBeforeServing covers the paths where run returns before
// starting a daemon: -version, -h, bad flags, and a config that does not
// load. None of them may create a socket.
func TestRunStopsBeforeServing(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "config.json")
	if err := os.WriteFile(invalid, []byte(`{"socket_path": "`+filepath.Join(dir, "throtd.sock")+`", "scheduler": {"mode": "sometimes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unparsable := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(unparsable, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string // "" means no error
	}{
		{[]string{"-version"}, ""},
		{[]string{"-version", "-config", unparsable}, ""}, // -version never reads the config
		{[]string{"-h"}, flag.ErrHelp.Error()},
		{[]string{"-log-level", "loud"}, "-log-level must be"},
		{[]string{"-config", unparsable}, "parse config"},
		{[]string{"-config", invalid}, `scheduler.mode must be off, observe, or enforce, not "sometimes"`},
	} {
		err := run(tc.args)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("run(%q) = %v, want nil", tc.args, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("run(%q) = %v, want an error containing %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "throtd.sock")); !os.IsNotExist(err) {
		t.Errorf("socket created: %v", err)
	}
}

func TestLogGoroutines(t *testing.T) {
	count := func(b *bytes.Buffer) int { return strings.Count(b.String(), "msg=goroutines n=") }
	var b bytes.Buffer
	log := newLogger(&b, options{logFormat: "text", logLevel: slog.LevelDebug})

	// It logs at once, so even a short-lived daemon reports a count.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logGoroutines(ctx, log, time.Hour)
	if n := count(&b); n != 1 {
		t.Fatalf("%d lines with a cancelled context, want 1:\n%s", n, b.String())
	}

	b.Reset()
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	logGoroutines(ctx, log, 10*time.Millisecond)
	if n := count(&b); n < 3 {
		t.Errorf("%d lines in 100ms at 10ms, want several:\n%s", n, b.String())
	}

	// At the default info level the debug lines are dropped.
	b.Reset()
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	logGoroutines(ctx, newLogger(&b, options{logFormat: "text", logLevel: slog.LevelInfo}), time.Hour)
	if b.Len() != 0 {
		t.Errorf("info logger wrote %q", b.String())
	}
}
