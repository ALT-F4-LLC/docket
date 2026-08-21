package trust

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/exec"
)

// AddRequest is one entry to write, as the caller describes it before binding
// and hashing are computed.
//
// Argv is taken VERBATIM. At the CLI boundary the operator's own shell has
// already tokenized (§5.2 K1), and docket stores those tokens — no splitting,
// no globbing, no expansion. That is what makes argv injection closed at the
// point of entry rather than patched at the point of execution.
type AddRequest struct {
	Name       string
	Argv       []string
	RepoRoot   string // the repo to bind to; ignored when Global
	Global     bool
	Prefix     bool
	ReRunnable bool
	Tree       bool
	Flaky      bool
	// Stub declares the argv a placeholder rather than the check its name
	// implies (DKT-265). It changes no execution behavior; it travels with the
	// verdict so a hollow pass reads as hollow.
	Stub    bool
	Timeout string
	// Network is the host list this command must reach. It declares
	// a requirement; it grants nothing.
	Network []string
	NowMS   int64
	// OnChange runs INSIDE the store lock, after the change has been decided
	// and BEFORE the store is written. A non-nil error aborts the add with
	// NOTHING written.
	//
	// WHY THE HOOK EXISTS. §3.6's event is the only record that a grant
	// happened, and a record written AFTER the store — the shape this had —
	// leaves a granted entry with no trace of the grant whenever the write of
	// the record fails. An auditor reads that absence as "no grant happened",
	// which is a worse answer than no trail at all. Running the record first
	// inverts the surviving failure: what can be left behind now is a record of
	// an add that then failed to land, and the verb fails loudly saying so.
	//
	// IT IS A FUNC, not a handle to whatever does the recording, because this
	// package stays pure — no database, no engine, no CLI (see the package
	// comment). The ordering guarantee belongs here, next to the lock that makes
	// it meaningful; the recording belongs to the caller.
	//
	// IT IS NOT CALLED FOR AN IDEMPOTENT RE-ADD. Nothing is written, so there is
	// nothing to record: a record of a change must prove a change happened.
	OnChange func(Entry) error
}

// RemoveRequest is one entry to delete. It is a struct rather than a parameter
// list for the same reason AddRequest is: the hook made a fourth argument, and a
// growing positional signature is how a call site ends up passing the wrong
// bool.
type RemoveRequest struct {
	Name     string
	RepoRoot string // the repo whose binding to remove; ignored when Global
	Global   bool
	// OnChange runs INSIDE the lock, after the entry to delete has been found
	// and BEFORE the store is written, receiving the entry AS IT STOOD. Same
	// contract and same reasoning as AddRequest.OnChange.
	//
	// It receives the entry rather than leaving the caller to look one up
	// beforehand, because a caller's own pre-read cannot see the binding this
	// remove resolved: a lookup by name alone finds an entry bound to some other
	// repository and would describe THAT one in the record.
	OnChange func(Entry) error
}

// AddResult reports what an Add did, so a caller can render the disclosure
// (§3.5) without re-deriving it.
type AddResult struct {
	// Entry is the entry as written (or as already present, when Idempotent).
	Entry Entry
	// Idempotent is true when an identical entry already existed and nothing
	// was written.
	Idempotent bool
	// Warnings carries the --prefix over-authorization warning (§3.3). It is
	// NEVER suppressed — not by --yes, not by anything. Suppressing it is
	// exactly what would make the conversational posture unsafe.
	Warnings []string
}

// Add writes one entry, holding W1's lock across the whole read-modify-write.
//
// The sequence is exactly W1's: acquire, RE-READ the store from disk, apply the
// change, temp-write, rename, release. The re-read after acquiring is the point
// — a snapshot taken before the lock is a snapshot that may already be stale.
func Add(req AddRequest) (*AddResult, error) {
	path, err := StorePath()
	if err != nil {
		return nil, err
	}
	return addAt(path, req)
}

// addAt is the path-taking form, unexported per SB3.
func addAt(path string, req AddRequest) (*AddResult, error) {
	entry, warnings, err := buildEntry(req)
	if err != nil {
		return nil, err
	}

	if err := ensureStoreDir(filepath.Dir(path)); err != nil {
		return nil, err
	}

	lock, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	// W1: re-read UNDER the lock. Everything before this point may be stale.
	st, err := loadAt(path)
	if err != nil {
		return nil, err
	}

	if existing := findEntry(st, entry.Name, entry.Repo, entry.Global); existing != nil {
		if entriesEquivalent(*existing, entry) {
			// Idempotent success: nothing written, exit 0. Re-approving the
			// same command with the same flags is not a change.
			return &AddResult{Entry: *existing, Idempotent: true, Warnings: warnings}, nil
		}
		// A SILENT OVERWRITE would mean a trusted name's meaning can change
		// without the operator ever seeing the old value — the same reasoning
		// that makes a re-register with differing bytes a CONFLICT, applied to
		// a security-relevant file. Every differing property is named, so the
		// operator can see what would have changed.
		return nil, fmt.Errorf("%w: %q is already trusted in this repo as %s; the add would change %s. Remove it first with `docket trust rm %s` if the change is intended",
			ErrConflict, entry.Name, CanonicalArgv(existing.Argv),
			strings.Join(entryChanges(*existing, entry), "; "), entry.Name)
	}

	if req.OnChange != nil {
		if err := req.OnChange(entry); err != nil {
			return nil, err
		}
	}

	st.Entries = append(st.Entries, entry)
	if err := writeStore(path, st); err != nil {
		return nil, err
	}
	return &AddResult{Entry: entry, Warnings: warnings}, nil
}

// Remove deletes the entry bound to this repo (or the global one), under the
// same lock discipline as Add.
//
// Returns false when no such entry existed, which callers render as a
// NOT_FOUND rather than a failure — removing something absent is not an error
// worth an exit code of its own.
func Remove(req RemoveRequest) (bool, error) {
	path, err := StorePath()
	if err != nil {
		return false, err
	}
	return removeAt(path, req)
}

// removeAt is the path-taking form, unexported per SB3.
func removeAt(path string, req RemoveRequest) (bool, error) {
	var repo string
	if !req.Global {
		var err error
		repo, err = RepoIdentity(req.RepoRoot)
		if err != nil {
			return false, err
		}
	}

	lock, err := acquireLock(path)
	if err != nil {
		return false, err
	}
	defer lock.release()

	st, err := loadAt(path)
	if err != nil {
		return false, err
	}

	idx := findEntryIndex(st, req.Name, repo, req.Global)
	if idx < 0 {
		return false, nil
	}

	if req.OnChange != nil {
		if err := req.OnChange(st.Entries[idx]); err != nil {
			return false, err
		}
	}

	st.Entries = slices.Delete(st.Entries, idx, idx+1)
	if err := writeStore(path, st); err != nil {
		return false, err
	}
	return true, nil
}

// List returns the entries visible from a repo: those bound to it, plus every
// global one. It is a pure read and takes NO LOCK (W4).
func (s *Store) List(repoIdentity string) []Entry {
	var out []Entry
	for _, e := range s.Entries {
		if bindingMatches(&e, repoIdentity) {
			out = append(out, e)
		}
	}
	return out
}

// buildEntry computes the binding and the hash, and produces the --prefix
// warning when the request opts in.
func buildEntry(req AddRequest) (Entry, []string, error) {
	if req.Name == "" {
		return Entry{}, nil, fmt.Errorf("%w: a trust entry needs a name", ErrParse)
	}
	if len(req.Argv) == 0 {
		return Entry{}, nil, fmt.Errorf("%w: a trust entry needs a command; pass it after `--`", ErrParse)
	}

	e := Entry{
		Name:       req.Name,
		Argv:       append([]string(nil), req.Argv...),
		ArgvSHA256: ArgvSHA256(req.Argv),
		Global:     req.Global,
		Prefix:     req.Prefix,
		ReRunnable: req.ReRunnable,
		Tree:       req.Tree,
		Flaky:      req.Flaky,
		Stub:       req.Stub,
		Timeout:    req.Timeout,
		Network:    req.Network,
		AddedAtMS:  req.NowMS,
	}

	// P3: global requires the explicit flag; there is no implicit path to it.
	if !req.Global {
		repo, err := RepoIdentity(req.RepoRoot)
		if err != nil {
			return Entry{}, nil, err
		}
		e.Repo = repo
	}

	var warnings []string
	if req.Prefix {
		// §3.3's over-authorization warning. It names the repo because the
		// blast radius is repo-scoped, and it says what to do instead.
		scope := fmt.Sprintf("in repo %s", e.Repo)
		if e.Global {
			scope = "in EVERY repo"
		}
		warnings = append(warnings, fmt.Sprintf(
			"warning: prefix entry — this authorizes ANY command beginning with:\n  %s\n%s. A workflow or issue there may run further arguments without approval. Use a full argv instead unless you need this.",
			exec.RenderArgv(req.Argv), scope))
	}

	return e, warnings, nil
}

// findEntry locates an entry by the identity Add uses: name plus binding.
func findEntry(st *Store, name, repo string, global bool) *Entry {
	if i := findEntryIndex(st, name, repo, global); i >= 0 {
		return &st.Entries[i]
	}
	return nil
}

// findEntryIndex locates the index of the entry identified by name plus
// binding, or -1 if none matches. It is the one scan both findEntry and
// removeAt key off of, so the match rule — name, Global, and (global or a
// matching Repo) — cannot drift between the two call sites.
func findEntryIndex(st *Store, name, repo string, global bool) int {
	for i := range st.Entries {
		e := &st.Entries[i]
		if e.Name != name || e.Global != global {
			continue
		}
		if global || e.Repo == repo {
			return i
		}
	}
	return -1
}

// entriesEquivalent decides §3.5's idempotence row: an add of an IDENTICAL argv
// and IDENTICAL flags is a no-op success.
//
// Every field that changes behavior is compared. AddedAtMS is deliberately not:
// re-approving the same command with the same flags is the same authorization,
// and treating a differing timestamp as a conflict would make idempotence
// unreachable.
// NETWORK IS COMPARED, and its absence here was a hole. The declared host list
// changes what the child may reach and is exactly the property §3.6's trail
// exists to expose; leaving it out meant an add that asked for new egress at an
// existing name reported "already trusted", wrote nothing, and returned exit 0 —
// the operator's requested change silently discarded under a success message.
func entriesEquivalent(a, b Entry) bool {
	return len(entryChanges(a, b)) == 0
}

// entryChanges names the properties in which a proposed entry differs from the
// one already stored, oldest value first.
//
// It is one list serving two callers, and that is deliberate: idempotence asks
// whether the list is empty and the conflict message renders it, so the set of
// properties that "counts as a change" cannot drift between the check and the
// explanation. AddedAtMS is absent by the same reasoning as before — re-approving
// the same command with the same flags is the same authorization, and a differing
// timestamp as a conflict would make idempotence unreachable. Repo and Global are
// absent because findEntry already keyed on them.
func entryChanges(existing, proposed Entry) []string {
	var changes []string
	if existing.ArgvSHA256 != proposed.ArgvSHA256 || !slices.Equal(existing.Argv, proposed.Argv) {
		changes = append(changes, fmt.Sprintf("argv %s to %s",
			CanonicalArgv(existing.Argv), CanonicalArgv(proposed.Argv)))
	}
	for _, f := range []struct {
		name     string
		old, new bool
	}{
		{"prefix", existing.Prefix, proposed.Prefix},
		{"re_runnable", existing.ReRunnable, proposed.ReRunnable},
		{"tree", existing.Tree, proposed.Tree},
		{"flaky", existing.Flaky, proposed.Flaky},
		// `stub` joins the conflict set for the same reason the others are in
		// it: a re-add that silently flipped it would turn a hollow pass into a
		// pass that reads as real, or the reverse, with the store showing only
		// that something of that name was re-approved.
		{"stub", existing.Stub, proposed.Stub},
	} {
		if f.old != f.new {
			changes = append(changes, fmt.Sprintf("%s %t to %t", f.name, f.old, f.new))
		}
	}
	if !slices.Equal(existing.Network, proposed.Network) {
		// Order-sensitive, because the stored list is what an operator reads and
		// a reorder is still an edit to the file they audit. The remedy the
		// conflict names — remove, then add — costs one command either way.
		changes = append(changes, fmt.Sprintf("network [%s] to [%s]",
			strings.Join(existing.Network, " "), strings.Join(proposed.Network, " ")))
	}
	if existing.Timeout != proposed.Timeout {
		changes = append(changes, fmt.Sprintf("timeout %q to %q", existing.Timeout, proposed.Timeout))
	}
	return changes
}

// ensureStoreDir creates the config directory 0700 (I5).
func ensureStoreDir(dir string) error {
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return fmt.Errorf("creating the trust store directory %s: %w", dir, err)
	}
	return checkDirIntegrity(dir)
}

// writeStore is I5's atomic publish: a temp file in the SAME directory, created
// O_EXCL with mode 0600, then renamed over the target.
//
// Same directory because rename is only atomic within a filesystem. O_EXCL so a
// pre-planted temp file is not written through. The rename is what makes a
// reader either see the whole old file or the whole new one, never a partial
// write — which is also why the read path needs no lock (W4).
func writeStore(path string, st *Store) error {
	data, err := encode(st)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".trust-*.toml")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op once the rename succeeded
	}()

	// CreateTemp makes the file 0600 already; the explicit Chmod states the
	// requirement rather than relying on that, so a future stdlib change or a
	// restrictive umask cannot silently widen it.
	if err := tmp.Chmod(storeFileMode); err != nil {
		return fmt.Errorf("setting mode on %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	// fsync before rename: a rename that lands before the data is durable
	// leaves an empty trust file after a power loss, which is an allowlist that
	// silently lost every entry.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing the trust store %s: %w", path, err)
	}
	return nil
}
