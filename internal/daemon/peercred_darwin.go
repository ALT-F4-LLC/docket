//go:build darwin

package daemon

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the effective uid the kernel recorded for the process at the
// other end of conn: the connecting process on an accepted conn, the listening
// process on a dialed one. The uid is meaningless when err is non-nil.
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("peer credentials: %w", err)
	}
	if credErr != nil {
		return 0, fmt.Errorf("peer credentials: %w", credErr)
	}
	// getsockopt can succeed without filling the struct (a wrong level, a
	// non-local socket), which would read as uid 0. A real xucred always
	// carries at least one group.
	if cred.Ngroups < 1 {
		return 0, errors.New("peer credentials: kernel returned an empty xucred")
	}
	return cred.Uid, nil
}
