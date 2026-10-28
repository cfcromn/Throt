// Package update implements throt update: it finds the installed throt and
// throtd, asks GitHub for the newest matching release, downloads and
// verifies the archive for this platform, and swaps both binaries in place.
//
// It touches the network only when the user runs throt update; nothing here
// runs in the background or on any other command.
package update

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// ExitUpdateAvailable is the exit status of throt update --check when a
// newer release exists. Up to date is 0; errors are 1.
const ExitUpdateAvailable = 10

// ErrUpdateAvailable is returned by Run with Check set when an update
// exists. The report has already been printed.
var ErrUpdateAvailable = errors.New("an update is available")

// Options are the throt update flags.
type Options struct {
	Check   bool
	Version string
	Pre     bool
	Yes     bool
}

// Env is everything Run needs from the outside world, injected for tests.
type Env struct {
	HTTP    *http.Client
	BaseURL string
	Repo    string

	Executable string
	Current    string
	BuildInfo  *debug.BuildInfo
	GoBinDirs  []string
	Home       string
	GOOS       string
	GOARCH     string

	Getenv   func(string) string
	LookPath func(string) (string, error)
	// Run runs gh auth token and the new binaries' version checks.
	Run Runner

	Stdin  io.Reader
	IsTTY  bool
	Stdout io.Writer

	// Ping returns the running throtd's version, or an error when it is
	// not reachable.
	Ping func(ctx context.Context) (string, error)
}

// Run performs throt update.
func Run(ctx context.Context, o Options, e Env) error {
	inst, err := Locate(e.Executable, e.Current, e.BuildInfo, e.GoBinDirs, o.Version)
	var gi *GoInstallError
	switch {
	case errors.As(err, &gi):
		return err
	case err != nil && !o.Check:
		return err
	}

	token, _ := Token(ctx, e.Getenv, e.LookPath, e.Run)
	c := &Client{HTTP: e.HTTP, BaseURL: e.BaseURL, Repo: e.Repo, Token: token}
	var rs []Release
	if o.Version != "" {
		tag := "v" + strings.TrimPrefix(o.Version, "v")
		r, err := c.ReleaseByTag(ctx, tag)
		if err != nil {
			return err
		}
		rs = []Release{r}
	} else if rs, err = c.Releases(ctx); err != nil {
		return err
	}
	choice, err := Select(rs, e.Current, o.Version, o.Pre)
	if err != nil {
		return err
	}
	cur, next := strings.TrimPrefix(e.Current, "v"), strings.TrimPrefix(choice.Release.TagName, "v")

	if o.Check {
		label := "latest"
		if choice.Explicit {
			label = "requested"
		}
		fmt.Fprintf(e.Stdout, "current   %s\n%-9s %s\n", cur, label, next)
		if choice.UpToDate() {
			fmt.Fprintln(e.Stdout, "throt is up to date")
			return nil
		}
		fmt.Fprintf(e.Stdout, "update available; see what changed in CHANGELOG.md or %s, then run throt update\n", releasePage(choice.Release))
		return ErrUpdateAvailable
	}
	if choice.UpToDate() {
		fmt.Fprintf(e.Stdout, "throt %s is up to date (newest release: %s)\n", cur, next)
		return nil
	}

	verb := "Update"
	if choice.Cmp < 0 {
		verb = "Downgrade"
	}
	question := fmt.Sprintf("%s throt and throtd in %s from %s to %s?", verb, filepath.Dir(inst.Throt), cur, next)
	if !o.Yes {
		if !e.IsTTY {
			return fmt.Errorf("%s Refusing without confirmation because stdin is not a terminal; re-run with --yes", question)
		}
		fmt.Fprint(e.Stdout, question+" [y/N] ")
		line, _ := bufio.NewReader(io.LimitReader(e.Stdin, 256)).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(e.Stdout, "not updated")
			return nil
		}
	}

	archive, err := c.Download(ctx, choice.Release, e.GOOS, e.GOARCH)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "downloaded and verified %s (SHA-256 matches checksums.txt)\n", ArchiveName(choice.Release.TagName, e.GOOS, e.GOARCH))
	bins, err := Extract(archive)
	if err != nil {
		return err
	}
	if err := Replace(ctx, inst, bins, choice.Release.TagName, e.Run); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "updated %s and %s to %s\n", inst.Throtd, inst.Throt, next)

	if e.Ping != nil {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if v, err := e.Ping(pctx); err == nil {
			fmt.Fprintf(e.Stdout, "throtd is still running %s. Restart it to use %s (stopping throtd resumes everything it paused):\n  %s\n",
				strings.TrimPrefix(v, "v"), next, RestartHint(e.GOOS, e.Home, fileExists))
		}
	}
	return nil
}

// releasePage is the release's web page, where its notes (the CHANGELOG
// section) are shown.
func releasePage(r Release) string {
	if strings.HasPrefix(r.HTMLURL, "https://") {
		return r.HTMLURL
	}
	return "https://github.com/" + DefaultRepo + "/releases/tag/" + r.TagName
}

// RestartHint is the command that restarts throtd under the service
// manager the README sets up, or advice to restart it by hand.
func RestartHint(goos, home string, exists func(string) bool) string {
	switch {
	case goos == "darwin" && home != "" && exists(filepath.Join(home, "Library", "LaunchAgents", "dev.throt.throtd.plist")):
		return "launchctl kickstart -k gui/$(id -u)/dev.throt.throtd"
	case goos == "linux" && home != "" && exists(filepath.Join(home, ".config", "systemd", "user", "throtd.service")):
		return "systemctl --user restart throtd"
	}
	return "stop throtd (Ctrl-C or kill PID; see throt ping) and start it again"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
