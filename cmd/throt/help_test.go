package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

// TestHelpGoesToStdout checks that -h on throt and on its subcommands
// prints the usage to stdout and returns flag.ErrHelp, which main turns
// into exit 0, instead of failing with "flag: help requested".
func TestHelpGoesToStdout(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		var out bytes.Buffer
		fs, _, _ := globalFlags()
		fs.SetOutput(io.Discard)
		if err := parseFlags(fs, []string{arg}, &out); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("throt %s: err = %v, want flag.ErrHelp", arg, err)
		}
		if !strings.HasPrefix(out.String(), "Usage: throt") || !strings.Contains(out.String(), "-config") {
			t.Errorf("throt %s: stdout %q", arg, out.String())
		}
	}

	tests := []struct {
		name string
		call func(stdout io.Writer) error
		want string
	}{
		{"stats -h", func(w io.Writer) error { _, err := parseStatsArgs([]string{"-h"}, w, io.Discard); return err }, "usage: throt stats"},
		{"events -h", func(w io.Writer) error {
			n := 50
			_, err := parseEventsArgs([]string{"-h"}, &n, w, io.Discard)
			return err
		}, "usage: throt events"},
		{"run -h", func(w io.Writer) error { _, err := parseRunArgs([]string{"-h"}, w, io.Discard); return err }, "Usage: throt run"},
		{"config -h", func(w io.Writer) error { return runConfig([]string{"-h"}, "", w) }, configUsage},
		{"config check -h", func(w io.Writer) error { return runConfig([]string{"check", "-h"}, "", w) }, configUsage},
		{"config show -h", func(w io.Writer) error { return runConfig([]string{"show", "-h"}, "", w) }, configUsage},
		{"hook claude-code -h", func(w io.Writer) error {
			return runHook([]string{"claude-code", "-h"}, "", strings.NewReader(""), w, io.Discard)
		}, "Usage: throt hook"},
	}
	for _, tt := range tests {
		var out bytes.Buffer
		if err := tt.call(&out); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: err = %v, want flag.ErrHelp", tt.name, err)
		}
		if !strings.Contains(out.String(), tt.want) {
			t.Errorf("%s: stdout %q, want %q", tt.name, out.String(), tt.want)
		}
	}
}

// TestEveryCommandHelp checks that -h and --help after every command print
// its usage to stdout and return flag.ErrHelp, without running it: the
// config file and socket do not exist, so contacting the daemon or
// starting anything would fail or block.
func TestEveryCommandHelp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	for _, c := range commands() {
		for _, h := range []string{"-h", "--help"} {
			var out, errw bytes.Buffer
			args := []string{"-config", dir + "/none.json", c.name, h}
			err := run(context.Background(), args, strings.NewReader(""), &out, &errw)
			if !errors.Is(err, flag.ErrHelp) {
				t.Errorf("throt %s %s: err = %v, want flag.ErrHelp", c.name, h, err)
			}
			if !strings.Contains(strings.ToLower(out.String()), "usage: throt "+c.name) || errw.Len() != 0 {
				t.Errorf("throt %s %s: stdout %q, stderr %q", c.name, h, out.String(), errw.String())
			}
		}
	}
}

// TestBadFlagIsNotHelp checks that an unknown flag is still an error, with
// nothing on stdout.
func TestBadFlagIsNotHelp(t *testing.T) {
	var out, errw bytes.Buffer
	fs, _, _ := globalFlags()
	fs.SetOutput(&errw)
	err := parseFlags(fs, []string{"-bogus"}, &out)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want a parse error", err)
	}
	if out.Len() != 0 || !strings.Contains(errw.String(), "-bogus") || !strings.Contains(errw.String(), "Usage: throt") {
		t.Errorf("stdout %q, stderr %q", out.String(), errw.String())
	}
	if _, err := parseStatsArgs([]string{"-bogus"}, &out, io.Discard); err == nil || errors.Is(err, flag.ErrHelp) || out.Len() != 0 {
		t.Errorf("stats -bogus: err %v, stdout %q", err, out.String())
	}
}
