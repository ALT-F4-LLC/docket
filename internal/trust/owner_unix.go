//go:build unix

package trust

import (
	"os"
	"syscall"
)

// fileOwner returns the owning uid of a stat result. The second return is false
// when the platform does not expose one, in which case I3 and I4's ownership
// halves are skipped rather than guessed — a check that cannot be performed is
// not a check that passes silently, and every platform docket supports is unix.
func fileOwner(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
