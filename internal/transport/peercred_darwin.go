//go:build darwin

package transport

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// peerUIDFD reads LOCAL_PEERCRED, the credentials the peer had at connect
// time.
func peerUIDFD(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	// XUCRED_VERSION is 0 in <sys/ucred.h>; x/sys/unix does not export it.
	if cred.Version != 0 {
		return 0, fmt.Errorf("unexpected xucred version %d", cred.Version)
	}
	return cred.Uid, nil
}
