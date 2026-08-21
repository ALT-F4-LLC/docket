//go:build unix

package trust

import "syscall"

// mkfifo creates a named pipe, for the I1 row that proves a FIFO at the store
// path is refused rather than opened — an open on a FIFO blocks until a writer
// appears, so a runner that did not check would hang instead of refusing.
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
