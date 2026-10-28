package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/update"
)

func TestParseUpdateArgs(t *testing.T) {
	o, err := parseUpdateArgs([]string{"--check", "--version", "v0.8.0", "--pre", "--yes"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := (update.Options{Check: true, Version: "v0.8.0", Pre: true, Yes: true}); o != want {
		t.Errorf("got %+v, want %+v", o, want)
	}
	if _, err := parseUpdateArgs([]string{"now"}, io.Discard, io.Discard); err == nil {
		t.Error("stray argument accepted")
	}
	var out bytes.Buffer
	if _, err := parseUpdateArgs([]string{"-h"}, &out, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
	for _, want := range []string{"Usage: throt update", "10 when an update", "--check", "GH_TOKEN"} {
		if !strings.Contains(out.String(), strings.TrimPrefix(want, "--")) {
			t.Errorf("help lacks %q:\n%s", want, out.String())
		}
	}
}

// TestUpdateExitCodes checks the documented --check statuses: 0 when up to
// date, 10 when an update is available (printed by Run, so no message), 1
// with a message on errors.
func TestUpdateExitCodes(t *testing.T) {
	if err := updateExit(nil); err != nil {
		t.Errorf("up to date: %v", err)
	}
	var ee exitError
	if err := updateExit(update.ErrUpdateAvailable); !errors.As(err, &ee) || ee.code != 10 || ee.msg != "" {
		t.Errorf("update available: %#v", err)
	}
	err := updateExit(errors.New("boom"))
	if errors.As(err, &ee) || err == nil || !strings.Contains(err.Error(), "update: boom") {
		t.Errorf("error: %#v", err)
	}
	if !strings.Contains(usageText(), "update [--check]") || !strings.Contains(usageText(), "10 when an update is available") {
		t.Error("usage does not document update and its exit codes")
	}
}
