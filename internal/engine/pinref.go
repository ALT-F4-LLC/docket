package engine

import (
	"path/filepath"
	"strings"
)

// configRelativeRef is the ONE place a file path becomes a pin ref.
//
// Every reader of a file pin — verifyFilePin, readPinnedPacketFile, and the
// template check in render — resolves a ref the same two ways: the
// config-relative form joined to each instance-config root, then the absolute
// form a pre-v12 run recorded. A writer that records anything else produces a
// pin that activates cleanly and never resolves again, so both
// writers (activation's --pin and auto-registration's walk) derive the ref
// here, and the readers agree with them by construction.
//
// The path is made absolute and symlink-resolved best-effort before the
// containment test, so `.docket/config/workflows/x.toml` from the repository
// root, `workflows/x.toml` from the config root, and the absolute path all
// yield the same ref. Roots are expected in precedence order and already
// resolved (scanConfigDirs and instanceConfigRoots both do that); the first
// root holding the path wins, matching the readers' first-found rule.
//
// ok is false when the path sits under no root. The caller decides what that
// means: auto-registration keeps the walked absolute path, activation keeps an
// absolute path and refuses a relative one.
func configRelativeRef(roots []string, path string) (ref string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return rel, true
	}
	return "", false
}
