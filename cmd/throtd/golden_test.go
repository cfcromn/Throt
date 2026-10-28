package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The golden tests run the real main in a child process (this test binary,
// re-executed) and compare its exit status, stdout, and stderr byte for
// byte. Every case stops before throtd would start serving.

const goldenArgsEnv = "THROTD_GOLDEN_ARGS"

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(goldenArgsEnv); ok {
		var args []string
		if err := json.Unmarshal([]byte(v), &args); err != nil {
			fmt.Fprintln(os.Stderr, "golden: bad args:", err)
			os.Exit(2)
		}
		os.Args = append([]string{"throtd"}, args...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const goldenUsage = `Usage: throtd [flags]

throtd samples host and process metrics, attributes heavy workloads to
coding agents, and serves throt clients on a local Unix socket. It logs to
stderr; the event log ('throt events') is the record of what it did.

Log levels: info logs daemon lifecycle, agents appearing and leaving,
pressure changes, scheduler decisions and control actions; debug adds every
heavy workload start and exit and every admission lease; warn and error
keep only failures and processes left stopped.

Flags:
  -config string
    	path to config file (default "$DIR/xdg/throt/config.json")
  -log-format string
    	log format: text or json (default "text")
  -log-level string
    	log level: debug, info, warn, or error (default "info")
  -version
    	print version and exit
`

func TestGolden(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"scheduler": {"mode": "sometimes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
	}{
		{name: "help", args: []string{"-h"}, stdout: goldenUsage},
		{name: "help long", args: []string{"--help"}, stdout: goldenUsage},
		{name: "bad flag", args: []string{"-bogus"}, code: 1,
			stderr: "flag provided but not defined: -bogus\n" + goldenUsage + "throtd: flag provided but not defined: -bogus\n"},
		{name: "version", args: []string{"-version"}, stdout: "dev\n"},
		{name: "version with config", args: []string{"-config", bad, "--version"}, stdout: "dev\n"},
		{name: "argument", args: []string{"serve"}, code: 1, stderr: "throtd: unexpected argument \"serve\"\n"},
		{name: "log format", args: []string{"-log-format", "xml"}, code: 1,
			stderr: "throtd: -log-format must be text or json, not \"xml\"\n"},
		{name: "log level", args: []string{"-log-level", "trace"}, code: 1,
			stderr: "throtd: -log-level must be debug, info, warn, or error, not \"trace\"\n"},
		{name: "invalid config", args: []string{"-config", bad}, code: 1,
			stderr: "throtd: config $DIR/bad.json: scheduler.mode must be off, observe, or enforce, not \"sometimes\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argsJSON, _ := json.Marshal(tt.args)
			cmd := exec.Command(exe)
			cmd.Env = []string{
				goldenArgsEnv + "=" + string(argsJSON),
				"HOME=" + filepath.Join(dir, "home"),
				"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
				"XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"),
				// Under -cover the child warns on stderr without it.
				"GOCOVERDIR=" + t.TempDir(),
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				t.Fatal(err)
			}
			norm := func(s string) string { return strings.ReplaceAll(s, dir, "$DIR") }
			if code != tt.code {
				t.Errorf("exit %d, want %d", code, tt.code)
			}
			if got := norm(stdout.String()); got != tt.stdout {
				t.Errorf("stdout\n%s\nwant\n%s", got, tt.stdout)
			}
			if got := norm(stderr.String()); got != tt.stderr {
				t.Errorf("stderr\n%s\nwant\n%s", got, tt.stderr)
			}
		})
	}
}
