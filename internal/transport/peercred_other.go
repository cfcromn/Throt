//go:build !darwin && !linux

package transport

import (
	"errors"
	"fmt"
	"net"
	"runtime"
)

// PeerUID is not implemented on this OS; it always fails with
// errors.ErrUnsupported, so privileged methods are always denied.
func PeerUID(net.Conn) (uint32, error) {
	return 0, fmt.Errorf("peer uid on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
