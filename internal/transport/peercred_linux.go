//go:build linux

package transport

import "syscall"

// peerUIDFD reads SO_PEERCRED, the credentials the peer had at connect time.
func peerUIDFD(fd int) (uint32, error) {
	cred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
