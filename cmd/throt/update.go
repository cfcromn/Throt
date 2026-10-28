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
	"strings"
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
available, 1 on error. A development build or one installed with go install
is updated with go install instead; --check still reports the latest
release for it (exit 0 for a development build, which it cannot compare).

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
		return o, usageError("throt update", fmt.Sprintf("unexpected argument %q", ufs.Arg(0)))
	}
	return o, nil
}

// cmdUpdate runs 'throt update' with the real environment.
func cmdUpdate(ctx context.Context, e env, args []string) error {
	o, err := parseUpdateArgs(args, e.stdout, e.stderr)
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
	if cfg, err := e.loadConfig(); err == nil {
		socket = cfg.SocketPath
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runUpdate(ctx, o, update.Env{
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
		Stdout:     e.stdout,
		Ping: func(ctx context.Context) (string, error) {
			var r app.PingResult
			err := transport.Call(ctx, socket, "ping", nil, &r)
			return r.Version, err
		},
	})
	return updateExit(err)
}

// runUpdate runs update.Run, except that --check on a development build or
// a go install build, which update.Run refuses because it cannot replace
// them in place, still reports the latest release: the check only reads.
func runUpdate(ctx context.Context, o update.Options, e update.Env) error {
	if !o.Check {
		return update.Run(ctx, o, e)
	}
	var gi *update.GoInstallError
	if _, err := update.Locate(e.Executable, e.Current, e.BuildInfo, e.GoBinDirs, o.Version); !errors.As(err, &gi) {
		return update.Run(ctx, o, e)
	}
	token, _ := update.Token(ctx, e.Getenv, e.LookPath, e.Run)
	c := &update.Client{HTTP: e.HTTP, BaseURL: e.BaseURL, Repo: e.Repo, Token: token}
	var rs []update.Release
	if o.Version != "" {
		r, err := c.ReleaseByTag(ctx, "v"+strings.TrimPrefix(o.Version, "v"))
		if err != nil {
			return err
		}
		rs = []update.Release{r}
	} else {
		var err error
		if rs, err = c.Releases(ctx); err != nil {
			return err
		}
	}
	// A development build is older than any release.
	dev := strings.HasPrefix(e.Current, "dev")
	current := e.Current
	if dev {
		current = "0.0.0"
	}
	choice, err := update.Select(rs, current, o.Version, o.Pre)
	if err != nil {
		return err
	}
	label := "latest"
	if choice.Explicit {
		label = "requested"
	}
	fmt.Fprintf(e.Stdout, "current   %s\n%-9s %s\n", strings.TrimPrefix(e.Current, "v"), label, strings.TrimPrefix(choice.Release.TagName, "v"))
	if !dev && choice.UpToDate() {
		fmt.Fprintln(e.Stdout, "throt is up to date")
		return nil
	}
	fmt.Fprintf(e.Stdout, "%s; install the release with:\n%s\n%s\n", gi.Reason, gi.Command, update.PrivateRepoHint)
	if dev {
		return nil // a development build has no version to compare
	}
	return update.ErrUpdateAvailable
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
