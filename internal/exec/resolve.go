package exec

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve turns argv[0] into the absolute path of the binary that will actually
// execute, and REFUSES if that binary lives inside the repository.
//
// entryArgv0 is the trust entry's own argv[0], which the absolute-path
// exception needs: when the operator trusted an ABSOLUTE PATH, they trusted
// that file, and it runs even when it is repo-resident.
//
// TWO DISTINCT ATTACKS ARE CLOSED HERE, and conflating them is how one of them
// gets reopened:
//
// First, a bare `make` resolved against the WORKING DIRECTORY, so a repo that
// ships ./make wins. Go's exec.ErrDot exists for exactly this, and it is
// treated as a hard refusal rather than a thing to work around.
//
// Second, a PATH ENTRY that points INSIDE the repo. An operator allows a repo's
// .envrc for legitimate dev tooling and it prepends a repo-resident bin
// directory; or the repo ships a bin/ the operator added to PATH once. From
// then on, a trust entry naming `make` authorizes whatever executable the repo
// places there. The working-directory check does not see this at all — the
// PATH is the operator's own, unmodified by docket, and contains no `.`. The
// trust decision was about a command NAME; the repo supplies the CODE.
//
// WHY REFUSE RATHER THAN SANITIZE PATH: stripping repo-resident entries out of
// the child's PATH silently changes what the operator's own tooling resolves to
// (a repo-managed toolchain is often exactly what a check is SUPPOSED to use),
// and it fails open on the next way a PATH entry can reach repo content — a
// symlink farm, a home-relative path the repo can write. Refusing at the
// RESOLVED PATH is a single check at the one place that matters, it fails
// CLOSED, and its message names the exact remedy.
//
// WHAT THIS DELIBERATELY DOES NOT CLOSE: a repo that supplies a library, a
// build target, or any other INPUT to a trusted binary still influences what
// that binary does. That is the accepted residual — a trusted command is
// trusted — and no resolution rule reaches it. The containment rules close the
// narrower and sharper hole: the EXECUTABLE ITSELF being repo content chosen by
// name.
func Resolve(argv0, repoRoot, entryArgv0 string) (string, error) {
	if argv0 == "" {
		return "", fmt.Errorf("%w: the command is empty", ErrRefused)
	}

	resolved, err := exec.LookPath(argv0)
	if err != nil {
		// exec.ErrDot means LookPath found the program relative to the
		// CURRENT DIRECTORY. A repo-planted ./make is exactly that, so it is a
		// refusal, never something to re-resolve into an absolute path and run
		// anyway.
		if errors.Is(err, exec.ErrDot) {
			return "", fmt.Errorf("%w: %s resolved against the working directory; docket never resolves a command name against the directory it runs in", ErrRefused, Render(argv0))
		}
		return "", fmt.Errorf("%w: %s was not found on PATH: %v", ErrRefused, Render(argv0), err)
	}

	// The SYMLINK-RESOLVED path of the binary that would actually execute.
	// Resolving symlinks is required, not cosmetic: a PATH directory OUTSIDE
	// the repo containing `make -> <repo>/bin/make` is the same attack with one
	// indirection, and a check on the unresolved path would wave it through.
	resolvedAbs, err := NormalizePath(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve %s: %v", ErrRefused, Render(resolved), err)
	}

	// The repo root computed THE SAME WAY — the same function on both
	// sides, so the comparison cannot be defeated by a symlinked checkout.
	rootAbs, err := NormalizePath(repoRoot)
	if err != nil {
		return "", &environmentRefusal{fmt.Errorf("%w: cannot resolve the repo path %s: %v", ErrRefused, Render(repoRoot), err)}
	}

	// THE ONE EXCEPTION. The trust entry's own argv[0] is an absolute path
	// which, after the same normalization, equals the resolved path. Then the
	// operator trusted THAT FILE, not a name that happened to resolve to it,
	// and it executes. Repo-owned scripts therefore stay usable — trusting an
	// absolute path works and always did the right thing.
	if filepath.IsAbs(entryArgv0) {
		if entryAbs, err := NormalizePath(entryArgv0); err == nil && entryAbs == resolvedAbs {
			return resolvedAbs, nil
		}
	}

	// Refuse a binary at or under the repo root.
	if Under(rootAbs, resolvedAbs) {
		return "", fmt.Errorf("%w: %s resolved to %s, which is inside the repository; trust the absolute path explicitly if this is intended",
			ErrRefused, Render(argv0), Render(resolvedAbs))
	}

	return resolvedAbs, nil
}

// NormalizePath is the shared normalization of the binary and the repo root:
// absolute, then symlink-resolved. Using ONE function for both sides is the
// requirement — two functions that agree today are two functions that can
// drift.
//
// Exported because internal/trust's RepoIdentity needs the identical
// normalization to bind trust entries to a repo path: the same function on
// both sides is what keeps the two comparisons from drifting apart.
func NormalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Under reports whether path is AT or UNDER root, component-wise.
//
// COMPONENT-WISE, NEVER A STRING PREFIX. This is the rule with a trap in it,
// and the trap has its own test row: with root = /src/docket, the path
// /src/docket-evil/bin/make must NOT count as contained. A strings.HasPrefix
// implementation says it is — "/src/docket" is a prefix of "/src/docket-evil" —
// and would refuse a perfectly legitimate binary in a NEIGHBOURING directory
// while looking correct in every other test.
//
// filepath.Rel gives the component-wise answer: a result that begins with ".."
// escaped the root, and a result of "." means the path IS the root.
//
// Exported because internal/config's local-store walk bound check needs the
// same component-wise containment test; one function shared is one function
// that cannot drift.
func Under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		// Different volumes, or otherwise incomparable. Not contained — and
		// erring toward "not contained" here is safe because the two paths
		// could not be in the same tree.
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// rel == "." means the resolved binary IS the repo root, which is not a
	// file that can execute; treat it as contained anyway, since anything that
	// produced it is confused and refusing is the closed direction.
	return true
}

// ErrEnvironment marks a refusal that says nothing about trust: the repo root
// the containment check compares against does not resolve, so the check could
// not be made (DKT-4251). It is never returned alone. Every environment refusal
// is also an ErrRefused (`errors.Is` finds both), so a caller that only asks
// "did nothing run" keeps failing closed; a caller that routes on WHY can tell
// a vanished checkout from a trust miss.
//
// It is deliberately narrow. Only the failure to resolve the repo root is an
// environment fact. A binary found inside the repository, a name resolved
// against the working directory, a binary not on PATH, and a pin that no
// longer holds are all about what would run, and stay plain refusals.
var ErrEnvironment = errors.New("execution environment unavailable")

// environmentRefusal is a refusal that is both ErrRefused and ErrEnvironment.
// Its message is the wrapped error's, unchanged.
type environmentRefusal struct{ err error }

func (e *environmentRefusal) Error() string { return e.err.Error() }
func (e *environmentRefusal) Unwrap() error { return e.err }
func (e *environmentRefusal) Is(target error) bool {
	return target == ErrEnvironment
}
