package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/ALT-F4-LLC/docket/internal/exec"
)

// CanonicalArgv returns the canonical encoding of an argv: the JSON
// encoding of the string list, with no whitespace.
//
// JSON RATHER THAN A DELIMITER-JOINED STRING, because NO DELIMITER IS SAFE. An
// argument containing a NUL is impossible, but one containing a newline, a
// space, or any chosen separator is trivial — and a join-then-hash scheme makes
// ["a b"] and ["a","b"] collide. That collision is an argv-injection primitive
// in the matcher itself: it would let an operator who approved one command
// find a different one authorized. TestCanonicalArgvHashDoesNotCollide is the
// test that matters.
func CanonicalArgv(argv []string) string {
	// A nil argv and an empty argv canonicalize identically, to "[]", which is
	// correct: neither names a program and neither can ever match a candidate.
	if argv == nil {
		argv = []string{}
	}
	b, err := json.Marshal(argv)
	if err != nil {
		// json.Marshal of []string cannot fail; the encoder has no path to an
		// error for this type. Returning the empty canonical form rather than
		// panicking keeps the failure closed: it can never equal a real hash.
		return "[]"
	}
	return string(b)
}

// ArgvSHA256 is the hex-encoded SHA-256 of the canonical argv. It is what an
// entry stores and what a candidate is compared against.
func ArgvSHA256(argv []string) string {
	sum := sha256.Sum256([]byte(CanonicalArgv(argv)))
	return hex.EncodeToString(sum[:])
}

// argv0ContentSHA256 hashes the file an absolute argv[0] names, returning the
// symlink-resolved path it read and the hex-encoded SHA-256 of its bytes. The
// resolution is exec.NormalizePath, the same one exec uses to decide which file
// executes, and the hash is exec.FileSHA256, the one hasher exec.RunPinned
// verifies with, so the pin trust add stores and the check Lookup makes cannot
// disagree with the spawn about which bytes a pin names.
func argv0ContentSHA256(argv0 string) (resolved, sum string, err error) {
	resolved, err = exec.NormalizePath(argv0)
	if err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", argv0, err)
	}
	sum, err = exec.FileSHA256(resolved)
	if err != nil {
		return resolved, "", err
	}
	return resolved, sum, nil
}

// declaredFileSHA256 hashes a declared repo-relative path under root, the
// hasher behind both the pins trust add stores and the check LookupAt makes.
//
// The root is explicit and must be absolute: resolving against the process
// cwd would hash whichever copy the caller happens to stand in, not the one the
// gate reads. Root and file are both symlink-resolved before the containment
// check, so a declared path that is a link out of the root is refused rather
// than pinning bytes the repository does not own.
func declaredFileSHA256(root, rel string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the declared-file root %q is not an absolute path", root)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolving the declared-file root %s: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, rel))
	if err != nil {
		return "", fmt.Errorf("resolving declared file %s under %s: %w", rel, resolvedRoot, err)
	}
	if !exec.Under(resolvedRoot, resolved) {
		return "", fmt.Errorf("declared file %s resolves to %s, outside the root %s", rel, resolved, resolvedRoot)
	}
	return exec.FileSHA256(resolved)
}
