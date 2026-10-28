package app

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cfcromn/Throt/internal/transport"
)

// lockInstance takes an exclusive lock next to the socket so only one daemon
// can own it, even when two start at once. The lock dies with the process.
func lockInstance(socket string) (unlock func(), err error) {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := transport.CheckPrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("throtd is already running on %s", socket)
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

// listenUnix creates the socket; tests replace it to inspect the socket as
// soon as it exists.
var listenUnix = func(path string) (net.Listener, error) { return net.Listen("unix", path) }

// listen creates an owner-only socket at path. The caller holds the instance
// lock, so an existing socket is stale. Only an actual socket is removed;
// any other file at path is an error rather than collateral damage.
func listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	// The socket is created with the process umask and only restricted by
	// the Chmod below. The directory check allows group/other traversal,
	// so under a lax umask (e.g. 002) another user could connect in
	// between. Mask group and other while the socket is created: no other
	// goroutine creates files yet, and the mask only removes permissions.
	old := syscall.Umask(0o077)
	l, err := listenUnix(path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return l, nil
}
