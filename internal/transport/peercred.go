//go:build darwin || linux

package transport

import (
	"fmt"
	"net"
)

// PeerUID returns the effective uid of the process on the other end of a
// Unix domain socket connection, as recorded by the kernel.
func PeerUID(conn net.Conn) (uint32, error) {
	var uid uint32
	err := withPeerFD(conn, "peer uid", func(fd int) (err error) {
		uid, err = peerUIDFD(fd)
		return err
	})
	return uid, err
}

// PeerPID returns the PID of the process on the other end of a Unix domain
// socket connection, as recorded by the kernel when it connected.
func PeerPID(conn net.Conn) (int, error) {
	var pid int
	err := withPeerFD(conn, "peer pid", func(fd int) (err error) {
		pid, err = peerPIDFD(fd)
		return err
	})
	return pid, err
}

// withPeerFD runs get on conn's socket descriptor.
func withPeerFD(conn net.Conn, what string, get func(fd int) error) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%s: %T is not a Unix socket connection", what, conn)
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	var getErr error
	// Control borrows the descriptor without duplicating it.
	if err := rc.Control(func(fd uintptr) { getErr = get(int(fd)) }); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if getErr != nil {
		return fmt.Errorf("%s: %w", what, getErr)
	}
	return nil
}
