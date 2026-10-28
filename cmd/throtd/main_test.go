package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseFlagsDefaults(t *testing.T) {
	o, err := parseFlags(nil, &bytes.Buffer{})
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
		o, err := parseFlags(tc.args, &bytes.Buffer{})
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
		if _, err := parseFlags(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%q: want an error", args)
		}
	}
}

func TestHelpDocumentsLogFlags(t *testing.T) {
	var b bytes.Buffer
	if _, err := parseFlags([]string{"-h"}, &b); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"-log-format", "text or json", "-log-level", "debug, info, warn, or error", "-config"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, b.String())
		}
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
	} {
		if got := goroutineLogInterval(v); got != want {
			t.Errorf("goroutineLogInterval(%q) = %s, want %s", v, got, want)
		}
	}
}
