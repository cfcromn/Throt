//go:build darwin || linux

package transport

import (
	"fmt"
	"net"
)

// PeerUID returns the effective uid of the process on the other end of a
// Unix domain socket connection, as recorded by the kernel.
func PeerUID(conn net.Conn) (uint32, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer uid: %T is not a Unix socket connection", conn)
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer uid: %w", err)
	}
	var (
		uid    uint32
		getErr error
	)
	// Control borrows the descriptor without duplicating it.
	if err := rc.Control(func(fd uintptr) { uid, getErr = peerUIDFD(int(fd)) }); err != nil {
		return 0, fmt.Errorf("peer uid: %w", err)
	}
	if getErr != nil {
		return 0, fmt.Errorf("peer uid: %w", getErr)
	}
	return uid, nil
}
