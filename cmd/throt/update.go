package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/transport"
	"github.com/cfcromn/Throt/internal/update"
)

const updateUsage = `Usage: throt update [--check] [--version vX.Y.Z] [--pre] [--yes]

Download the newest Throt release from GitHub, verify its SHA-256 against
the release's checksums.txt, and replace throt and the throtd next to it
in place. This is the only command that uses the network.

A private repository needs a token: GH_TOKEN or GITHUB_TOKEN, else the
output of 'gh auth token' when the GitHub CLI is installed.

Exit status with --check: 0 when up to date, 10 when an update is
available, 1 on error.

Flags:
`

// parseUpdateArgs parses the arguments after 'update'.
func parseUpdateArgs(args []string, stdout, errw io.Writer) (update.Options, error) {
	var o update.Options
	ufs := flag.NewFlagSet("throt update", flag.ContinueOnError)
	ufs.SetOutput(errw)
	ufs.Usage = func() { fmt.Fprint(ufs.Output(), updateUsage); ufs.PrintDefaults() }
	ufs.BoolVar(&o.Check, "check", false, "only report the current and latest versions (exit 10 when an update is available)")
	ufs.StringVar(&o.Version, "version", "", "install this release, newer or older (a rollback)")
	ufs.BoolVar(&o.Pre, "pre", false, "include prereleases (the default when the running version is one)")
	ufs.BoolVar(&o.Yes, "yes", false, "do not ask for confirmation")
	if err := parseFlags(ufs, args, stdout); err != nil {
		return o, err
	}
	if ufs.NArg() > 0 {
		ufs.Usage()
		return o, errors.New("update: unexpected arguments")
	}
	return o, nil
}

// runUpdate runs 'throt update' with the real environment.
func runUpdate(args []string, cfgPath string) error {
	o, err := parseUpdateArgs(args, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("update: locating the running throt: %w", err)
	}
	bi, _ := debug.ReadBuildInfo()
	home, _ := os.UserHomeDir()
	socket := config.DefaultSocketPath()
	if cfg, err := config.Load(cfgPath); err == nil {
		socket = cfg.SocketPath
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = update.Run(ctx, o, update.Env{
		HTTP:       update.NewHTTPClient(),
		BaseURL:    update.DefaultBaseURL,
		Repo:       update.DefaultRepo,
		Executable: exe,
		Current:    app.Version,
		BuildInfo:  bi,
		GoBinDirs:  update.GoBinDirs(os.Getenv, home),
		Home:       home,
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		Getenv:     os.Getenv,
		LookPath:   exec.LookPath,
		Run:        update.ExecRunner,
		Stdin:      os.Stdin,
		IsTTY:      isTerminal(os.Stdin),
		Stdout:     os.Stdout,
		Ping: func(ctx context.Context) (string, error) {
			var r app.PingResult
			err := transport.Call(ctx, socket, "ping", nil, &r)
			return r.Version, err
		},
	})
	return updateExit(err)
}

// updateExit maps update.Run's result to throt's exit status.
func updateExit(err error) error {
	switch {
	case errors.Is(err, update.ErrUpdateAvailable):
		return exitError{code: update.ExitUpdateAvailable}
	case err != nil:
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// isTerminal reports whether f is a character device such as a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
