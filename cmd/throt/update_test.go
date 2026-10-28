package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
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

// TestUpdateCheckGoInstall checks that --check on a development build or a
// go install build reports the latest release and how to install it,
// instead of refusing as an update in place does.
func TestUpdateCheckGoInstall(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/cfcromn/Throt/releases" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `[{"tag_name":"v0.9.0"},{"tag_name":"v0.8.0"}]`)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "throt")
	for _, name := range []string{"throt", "throtd"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(exe), name), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := func(current string, bi *debug.BuildInfo, out io.Writer) update.Env {
		return update.Env{
			HTTP: srv.Client(), BaseURL: srv.URL, Repo: update.DefaultRepo,
			Executable: exe, Current: current, BuildInfo: bi, GoBinDirs: []string{filepath.Dir(exe)},
			Getenv: func(string) string { return "" }, Stdout: out,
		}
	}
	module := &debug.BuildInfo{Main: debug.Module{Path: "github.com/cfcromn/Throt", Version: "v0.8.0"}}
	for _, tt := range []struct {
		name, current string
		bi            *debug.BuildInfo
		want          error
		lines         []string
	}{
		{name: "dev", current: "dev", lines: []string{"current   dev\nlatest    0.9.0\n", "development build", "cmd/throt@latest", "GOPRIVATE="}},
		{name: "go install, behind", current: "v0.8.0", bi: module, want: update.ErrUpdateAvailable, lines: []string{"current   0.8.0\nlatest    0.9.0\n", "installed with go install", "GOPRIVATE="}},
		{name: "go install, current", current: "v0.9.0", bi: module, lines: []string{"throt is up to date"}},
	} {
		var out bytes.Buffer
		err := runUpdate(context.Background(), update.Options{Check: true}, env(tt.current, tt.bi, &out))
		if err != tt.want {
			t.Errorf("%s: err %v, want %v", tt.name, err, tt.want)
		}
		for _, l := range tt.lines {
			if !strings.Contains(out.String(), l) {
				t.Errorf("%s: output lacks %q:\n%s", tt.name, l, out.String())
			}
		}
	}
	// Without --check a dev build still refuses, with the go install hint.
	err := runUpdate(context.Background(), update.Options{}, env("dev", nil, io.Discard))
	var gi *update.GoInstallError
	if !errors.As(err, &gi) || !strings.Contains(err.Error(), "gh auth setup-git") {
		t.Errorf("dev update: %v", err)
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
