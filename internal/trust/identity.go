package trust

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/exec"
)

// RepoIdentity computes what binds an entry to "this repo" (§3.4).
//
// THE RULE: a repo's identity is the absolute, symlink-resolved filesystem path
// of the directory containing its .docket store (P1).
//
// This is the load-bearing decision of the whole trust model — get it wrong and
// the malicious-clone threat (T1) reopens — so the rejected candidates are
// recorded here rather than only in the TDD:
//
//   - The git remote URL is REPO-CONTROLLED. .git/config is a file the clone
//     ships or the attacker edits; a hostile clone sets its origin to the
//     victim's trusted URL and inherits every entry. It also fails for repos
//     with no remote, and for the same project cloned twice, where the operator
//     plausibly wants different answers.
//   - The git root commit hash is also repo-controlled — a clone has the same
//     root commit, which is the point of a clone. It is WORSE than the remote
//     URL because it looks cryptographic.
//   - A stored repo UUID or the issues database is repo content; it is
//     committed, so a clone carries it. Same failure again.
//   - The absolute filesystem path is NOT repo-controlled. A clone lives
//     somewhere else on the operator's disk; the path differs; the entries do
//     not match. That is why it is chosen.
//
// Symlink resolution is why this holds under the obvious dodge: without it, an
// attacker who can create a symlink in the operator's home makes a fork REPORT
// the trusted path. Resolving both sides to their real paths defeats it. The
// residual — an attacker who can move or replace the trusted directory itself —
// is the file-permission boundary again (§2), where it belongs.
//
// The known cost, recorded rather than discovered later: moving a repository
// invalidates its trust entries. That is the correct direction of failure — a
// moved repo re-earns trust; a hostile clone never inherits it — and the
// unmatched diagnostic names the bound path so the operator can see why.
func RepoIdentity(repoRoot string) (string, error) {
	// exec.NormalizePath is the same abs-then-symlink-resolve internal/exec's
	// R1/R2 use to compute a repo root for the containment check — one
	// function on both sides so the two comparisons cannot drift apart.
	resolved, err := exec.NormalizePath(repoRoot)
	if err != nil {
		// A repo root that does not resolve cannot be matched against. Failing
		// here rather than falling back to the unresolved path is the closed
		// direction: an unresolved path could string-equal an entry a resolved
		// one would not.
		return "", fmt.Errorf("resolving the repo path %s: %w", repoRoot, err)
	}
	return resolved, nil
}
