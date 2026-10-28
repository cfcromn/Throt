package update

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// modulePath is the module go install builds Throt from.
const modulePath = "github.com/cfcromn/Throt"

// versionTimeout bounds running a new binary to check its version.
const versionTimeout = 10 * time.Second

// Installed is where the running throt and its throtd live, with symlinks
// resolved, so the files themselves are replaced and links keep pointing at
// them.
type Installed struct {
	Throt, Throtd string
}

// GoInstallError means the binaries are managed by the Go toolchain (or are
// a development build) and should be updated with go install, which Command
// spells out.
type GoInstallError struct {
	Reason  string
	Command string
}

func (e *GoInstallError) Error() string {
	return e.Reason + "; update it with:\n" + e.Command
}

// goInstallCommand is the go install line for both programs at ver.
func goInstallCommand(ver string) string {
	if ver == "" {
		ver = "latest"
	} else if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	return fmt.Sprintf("  go install %s/cmd/throt@%s\n  go install %s/cmd/throtd@%s", modulePath, ver, modulePath, ver)
}

// Locate finds the installed binaries from exe, the running executable.
// version is the running version, bi its build info, and goBinDirs the
// directories go install writes to. want is the --version asked for, if
// any, used in the go install hint.
func Locate(exe, version string, bi *debug.BuildInfo, goBinDirs []string, want string) (Installed, error) {
	if strings.HasPrefix(version, "dev") {
		return Installed{}, &GoInstallError{Reason: "this is a development build (" + version + ")", Command: goInstallCommand(want)}
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return Installed{}, fmt.Errorf("locating the running throt: %w", err)
	}
	if bi != nil && bi.Main.Version != "" && bi.Main.Version != "(devel)" && underAny(filepath.Dir(real), goBinDirs) {
		return Installed{}, &GoInstallError{Reason: "throt was installed with go install (" + bi.Main.Version + ")", Command: goInstallCommand(want)}
	}
	if _, ok := parseVersion(version); !ok {
		return Installed{}, fmt.Errorf("the running version %q is not a release version; reinstall from a release", version)
	}
	d := filepath.Join(filepath.Dir(real), "throtd")
	rd, err := filepath.EvalSymlinks(d)
	if err != nil {
		return Installed{}, fmt.Errorf("throtd is not next to throt (%s): %w; install both from the same release", d, err)
	}
	for _, p := range []string{real, rd} {
		fi, err := os.Stat(p)
		if err != nil {
			return Installed{}, err
		}
		if !fi.Mode().IsRegular() {
			return Installed{}, fmt.Errorf("%s is not a regular file", p)
		}
	}
	return Installed{Throt: real, Throtd: rd}, nil
}

// underAny reports whether dir is one of dirs, comparing resolved paths.
func underAny(dir string, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// GoBinDirs returns where go install puts binaries: $GOBIN, else each
// $GOPATH entry's bin, else ~/go/bin.
func GoBinDirs(getenv func(string) string, home string) []string {
	var dirs []string
	if b := getenv("GOBIN"); b != "" {
		dirs = append(dirs, b)
	}
	if gp := getenv("GOPATH"); gp != "" {
		for _, p := range filepath.SplitList(gp) {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	} else if home != "" {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// NotWritableError means a binary's directory cannot be written, so nothing
// was changed.
type NotWritableError struct {
	Dir string
	Err error
}

func (e *NotWritableError) Error() string {
	return fmt.Sprintf("cannot write to %s (%v); nothing was changed. Re-run throt update as a user who can write there (for example with sudo when it is a system directory), or reinstall manually", e.Dir, e.Err)
}

func (e *NotWritableError) Unwrap() error { return e.Err }

// staged is a verified new binary waiting next to the one it replaces.
type staged struct {
	tmp, dst string
}

// Replace installs the new throtd and throt over inst. Each new binary is
// written to a temporary file in its destination's directory, made 0755,
// and run with its version argument (throt version, throtd -version); its
// output must be want. Only when both are staged and verified are they
// renamed over the old files, throtd first and throt last. Any failure
// before the renames leaves the old binaries untouched and removes the
// temporary files.
func Replace(ctx context.Context, inst Installed, bins map[string][]byte, want string, run Runner) (err error) {
	plan := []struct {
		name, dst string
		args      []string
	}{
		{"throtd", inst.Throtd, []string{"-version"}},
		{"throt", inst.Throt, []string{"version"}},
	}
	var done []staged
	defer func() {
		if err != nil {
			for _, s := range done {
				os.Remove(s.tmp)
			}
		}
	}()
	for _, p := range plan {
		data := bins[p.name]
		if len(data) == 0 {
			return fmt.Errorf("no new %s to install", p.name)
		}
		tmp, err := writeTemp(filepath.Dir(p.dst), p.name, data)
		if err != nil {
			return err
		}
		done = append(done, staged{tmp: tmp, dst: p.dst})
		if err := checkVersion(ctx, run, tmp, p.args, want); err != nil {
			return fmt.Errorf("the new %s failed its check, so nothing was changed: %w", p.name, err)
		}
	}
	for i, s := range done {
		if err := os.Rename(s.tmp, s.dst); err != nil {
			if i == 0 {
				return fmt.Errorf("replacing %s: %w; nothing was changed", s.dst, err)
			}
			return fmt.Errorf("throtd was updated but replacing %s failed (%w); throt and throtd are now different releases, so re-run throt update or reinstall manually", s.dst, err)
		}
	}
	return nil
}

// writeTemp writes data to a new 0755 file in dir and syncs it.
func writeTemp(dir, name string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, "."+name+".update-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) || isReadOnly(err) {
			return "", &NotWritableError{Dir: dir, Err: err}
		}
		return "", fmt.Errorf("staging %s in %s: %w", name, dir, err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(0o755)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("staging %s in %s: %w", name, dir, werr)
	}
	return tmp, nil
}

func isReadOnly(err error) bool { return errors.Is(err, syscall.EROFS) }

// checkVersion runs path with args and compares its output with want.
func checkVersion(ctx context.Context, run Runner, path string, args []string, want string) error {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := run(ctx, path, args...)
	if err != nil {
		return fmt.Errorf("running %s %s: %w", filepath.Base(path), strings.Join(args, " "), err)
	}
	got := strings.TrimSpace(string(out))
	if len(got) > 200 {
		got = got[:200]
	}
	if !sameVersion(got, want) {
		return fmt.Errorf("it reports version %q, want %q", got, strings.TrimPrefix(want, "v"))
	}
	return nil
}
