// Package trust is the user-level allowlist of commands Docket may execute
// (docs/design/engine-spec.md §4, docs/tdd/gates-trust.md §3).
//
// The package is PURE: no database, no engine, no CLI. A file, a parser, a
// matcher, and a writer. That purity is what makes §9.1's unit tables possible
// without building a run, and it is why the security-sensitive mechanism can be
// reviewed on its own.
//
// The one sentence that governs everything here: trust entries are USER-LEVEL
// and are never read from a repository. An adversary with write access to the
// store or to the docket binary has already won and is out of scope (§2); the
// adversary this package defends against is hostile REPOSITORY CONTENT — a
// cloned repo, a pulled branch, an issue body someone else wrote — reaching an
// operator who runs docket in it.
package trust

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Entry is one trusted command, in §3.1's shape.
//
// An entry names a COMMAND, not a role: there is no slot in this struct for who
// or what will run it, which is the genericity rule holding by construction
// (§1.1).
type Entry struct {
	// Name is the gate name a workflow references. It is an OPAQUE string:
	// nothing in this package interprets it, there is no registry of known
	// names, and no name has behavior.
	Name string `toml:"name"`
	// Argv is the resolved argv; NEVER a shell string. argv[0] is the program.
	Argv []string `toml:"argv"`
	// ArgvSHA256 is the hash of the canonical argv (§3.3), hex-encoded. It is
	// stored so a hand-edited or corrupted file is caught rather than obeyed.
	ArgvSHA256 string `toml:"argv_sha256"`
	// Repo is the repo binding (§3.4) — the absolute, symlink-resolved path of
	// the directory containing the repo's .docket store. Empty when Global.
	Repo string `toml:"repo"`
	// Global makes the entry apply in any repo. It requires an explicit
	// --global at add time (P3); there is no way to get it implicitly.
	Global bool `toml:"global"`
	// Prefix opts the entry into prefix matching (§3.3). Explicit opt-in only:
	// a full-argv entry NEVER matches by prefix, in either direction (M4).
	Prefix bool `toml:"prefix"`
	// ReRunnable declares the command safe to re-run after a crash (§7.5).
	// It is the operator's assertion about their own command; core never
	// infers idempotence.
	ReRunnable bool `toml:"re_runnable"`
	// Tree declares that the command touches the working tree, so it
	// serializes on the per-repo mutex (§7.4).
	Tree bool `toml:"tree"`
	// Flaky declares that re-runs are recorded individually (§5.6). It names a
	// property of a PROCESS — that it does not always produce the same exit
	// code on the same input — and is declared by the operator, never inferred.
	Flaky bool `toml:"flaky"`
	// Stub declares that this entry authorizes a PLACEHOLDER rather than the
	// check its name implies (DKT-265) — an `echo ok`, a `/usr/bin/true`, a
	// script that exits 0 without looking at anything.
	//
	// Stubs are legitimate. They are how a repo with no scanner installed still
	// exercises the workflow's shape, and forbidding them would only push the
	// same placeholder into a script with a more convincing name. What is not
	// legitimate is the resulting row: RUN-17's build, secret-scan, and tests
	// all recorded `pass`, every one of them an echo, and nothing in
	// gate_results, the run report, or the review packet told them apart from a
	// scanner that ran and found nothing. A reviewer reading "secret-scan:
	// pass" reasonably concludes a secret scan happened.
	//
	// It is a DECLARATION, exactly like ReRunnable, Tree, and Flaky: core
	// cannot look at an argv and tell a real check from a convincing one, and a
	// heuristic that tried would be wrong in both directions. The operator who
	// writes the entry knows, and this is where they say so.
	//
	// It changes NO EXECUTION BEHAVIOR. A stub entry matches, spawns, and is
	// recorded exactly as any other; the flag only travels with the verdict so
	// that hollow green stays visibly hollow.
	Stub bool `toml:"stub"`
	// Timeout is a per-entry override of the default, as a duration string.
	Timeout string `toml:"timeout"`
	// Network declares the hosts this command must reach, as bare hostnames.
	// An empty list — the default — declares no network need.
	//
	// WHY THIS IS DECLARED RATHER THAN INFERRED. A gate runs as a subprocess
	// of whichever executor claimed the step, inside that executor's sandbox.
	// A gate that needs the network therefore succeeds or DNS-fails depending
	// on a property of the claimant, not of the gate — and the failure lands
	// two process layers down, after the step is recorded, where the executor
	// never sees it and the step simply parks. Declaring the requirement makes
	// it a fact about the COMMAND, auditable in the trust file next to the
	// argv it authorizes.
	//
	// WHAT IT DOES, PRECISELY. It is a DECLARATION, not a grant. Core cannot
	// widen a sandbox it is running inside; nothing here punches holes in
	// anything. What the declaration buys is:
	//
	//   - proxy variables reach the child (see internal/exec.BuildEnv), which
	//     is the one concrete capability core CAN supply and which the
	//     allowlist otherwise withholds — a gate behind a corporate proxy
	//     could not use it at all before;
	//   - a failure in a network-declaring gate is reported as a possible
	//     reachability problem naming the hosts, instead of an opaque exit
	//     code the operator has to reverse-engineer;
	//   - the requirement is visible to anyone reading the trust file, so it
	//     can be provisioned deliberately rather than discovered mid-run.
	//
	// The names are OPAQUE to core: nothing resolves, validates, or connects
	// to them. They are the operator's statement of intent about their own
	// command, exactly like ReRunnable and Flaky.
	Network []string `toml:"network"`
	// AddedAtMS is when the entry was written.
	AddedAtMS int64 `toml:"added_at_ms"`
}

// NeedsNetwork reports whether this entry declares any network requirement.
func (e Entry) NeedsNetwork() bool { return len(e.Network) > 0 }

// Store is an immutable in-memory snapshot of the trust file.
//
// Immutability is T4's closure and is not an implementation convenience: the
// store is read ONCE at the start of a gate stage, and the matched entry's own
// argv is what executes (§7.2 M1). Matching does not produce a permission that
// is later applied to a command read from somewhere else, so there is no window
// between the check and the spawn in which anything can be swapped.
type Store struct {
	// Version is the file format version (§3.1). An unknown version is a hard
	// refusal, never a best-effort parse.
	Version int
	// Entries are the trusted commands, in file order.
	Entries []Entry
	// Path is where the snapshot was read from, for diagnostics.
	Path string
}

// FormatVersion is the only trust-file version this build understands.
const FormatVersion = 1

// storeDirMode and storeFileMode are I5's creation modes: the directory 0700
// and the file 0600, so a trust file is never group- or world-readable.
//
// I2 is stricter than "not world-writable" on purpose. A trust file readable by
// group is an inventory of the operator's approved commands and the repos they
// work in — modest, but there is no reason to publish it.
const (
	storeDirMode  os.FileMode = 0o700
	storeFileMode os.FileMode = 0o600
)

// ErrIntegrity is returned when the store fails one of §3.2's I1–I4 checks.
// Callers map it to VALIDATION_ERROR (exit 3); the wrapped message always names
// the path and what was wrong, and states the fix rather than only the
// complaint.
var ErrIntegrity = errors.New("trust store integrity")

// ErrParse is returned when the store exists and is readable but does not
// parse: an unknown version, an unknown key, or a malformed binding (P4).
var ErrParse = errors.New("trust store parse")

// ErrConflict is returned by Add when a name+repo already holds a DIFFERENT
// argv or flags (§3.5). It is a CONFLICT (exit 4), never a silent overwrite:
// a trusted name's meaning must not change without the operator seeing the old
// value.
var ErrConflict = errors.New("trust entry conflict")

// StorePath resolves the trust file's location per §3.1's two-row table.
//
// THERE IS NO THIRD SOURCE. No --trust-file flag, no DOCKET_TRUST_FILE env var,
// no repo-local path, no config key. This is T8's mechanism: every additional
// way to point docket at a trust file is another way for repo content — a
// checked-in .envrc, a direnv hook, a Makefile — to point it at a file the repo
// controls.
//
// XDG_CONFIG_HOME is honored because it is the platform convention and because
// the tests need it (§9.5 SB1): every test sets it to a sandbox directory, so
// no test can read or write the operator's real store.
func StorePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "docket", "trust.toml"), nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("cannot resolve the trust store: neither XDG_CONFIG_HOME nor HOME is set")
	}
	return filepath.Join(home, ".config", "docket", "trust.toml"), nil
}

// Load reads and returns an immutable snapshot of the trust store.
//
// A MISSING TRUST FILE IS NOT AN ERROR (§3.2). It is an empty allowlist: every
// gate is unmatched, nothing executes, and a run of a gate-bearing workflow
// reports what it would have needed. That is the correct default for a tool a
// stranger just installed, and it is the state §9 item 6's proof starts from.
func Load() (*Store, error) {
	path, err := StorePath()
	if err != nil {
		return nil, err
	}
	return loadAt(path)
}

// loadAt is the path-taking constructor. It is UNEXPORTED and used only by this
// package and its own tests — SB3's clause. There is no way to open a store at
// an arbitrary path from outside the package, which is the same decision that
// serves T8 (no --trust-file flag) and the sandbox rule.
func loadAt(path string) (*Store, error) {
	if err := checkIntegrity(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The empty allowlist. Nothing is trusted, so nothing runs.
			return &Store{Version: FormatVersion, Path: path}, nil
		}
		return nil, fmt.Errorf("reading the trust store %s: %w", path, err)
	}

	st, err := parse(data, path)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// checkIntegrity runs §3.2's I1–I4 against the store path and its containing
// directory, BEFORE parsing. Checked on every read and every write, because a
// file that became a symlink between two operations is exactly the swap the
// checks exist to catch.
//
// A missing file passes: it is the empty allowlist, and I5 governs its
// creation instead.
func checkIntegrity(path string) error {
	// I1: the file, if it exists, is a REGULAR FILE — not a symlink, not a
	// FIFO, not a directory. Lstat, not Stat, because Stat follows the symlink
	// this check exists to refuse.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No file yet. The containing directory is still checked when it
			// exists, so a hostile config dir cannot lie in wait.
			return checkDirIntegrity(filepath.Dir(path))
		}
		return fmt.Errorf("%w: cannot stat %s: %v", ErrIntegrity, path, err)
	}

	if err := checkFileIntegrity(path, info); err != nil {
		return err
	}
	return checkDirIntegrity(filepath.Dir(path))
}

// checkFileIntegrity is I1–I3 over an existing file: regular, 0600, owned by
// the calling uid.
func checkFileIntegrity(path string, info os.FileInfo) error {
	// I1.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink, not a regular file; the trust store is never a symlink because a link into a repository would let repo content grant itself execution", ErrIntegrity, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is %s, not a regular file", ErrIntegrity, path, describeMode(info.Mode()))
	}

	// I2: mode is exactly 0600 — no group or world bits, read or write.
	if perm := info.Mode().Perm(); perm != storeFileMode {
		return fmt.Errorf("%w: %s has mode %04o, but the trust store must be %04o; fix it with: chmod 600 %s", ErrIntegrity, path, perm, storeFileMode, path)
	}

	// I3: owned by the calling uid.
	owner, ok := fileOwner(info)
	if ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, but docket is running as uid %d; the trust store must be owned by the calling user", ErrIntegrity, path, owner, os.Getuid())
	}
	return nil
}

// checkDirIntegrity is I4: the containing directory is owned by the calling uid
// and is not group- or world-writable. A writable parent means anyone in the
// group can replace the trust file wholesale, which makes I1–I3 on the file
// itself decorative.
func checkDirIntegrity(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No config directory yet; I5 creates it 0700.
			return nil
		}
		return fmt.Errorf("%w: cannot stat %s: %v", ErrIntegrity, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrIntegrity, dir)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%w: %s has mode %04o and is group- or world-writable, so anyone with access could replace the trust store; fix it with: chmod 700 %s", ErrIntegrity, dir, perm, dir)
	}
	owner, ok := fileOwner(info)
	if ok && owner != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, but docket is running as uid %d", ErrIntegrity, dir, owner, os.Getuid())
	}
	return nil
}

// describeMode names what a non-regular file is, so a refusal tells the
// operator what it found rather than only that it refused.
func describeMode(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&os.ModeNamedPipe != 0:
		return "a FIFO"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeDevice != 0:
		return "a device"
	default:
		return "not a regular file"
	}
}
