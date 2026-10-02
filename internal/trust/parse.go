package trust

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// rawStore mirrors Store for decoding. The trust file is TOML, matching
// workflow definitions and the repo's existing config idiom.
type rawStore struct {
	Version int     `toml:"version"`
	Entries []Entry `toml:"entry"`
}

// parse decodes a trust file STRICTLY (§3.1).
//
// Unknown keys are a hard error, exactly as workflow parsing is strict
// (engine-spine §4.2). The reasoning is specific rather than stylistic: a
// typo'd `re_runable` silently defaulting to false would turn a re-runnable
// gate into a waiting-human park; a typo'd `global` defaulting to false is the
// safe direction, but a typo'd `prefix` is NOT — it would silently widen or
// narrow an authorization. A parser that is strict about one key and lax about
// another is a parser nobody can reason about.
func parse(data []byte, path string) (*Store, error) {
	var raw rawStore
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid TOML: %v", ErrParse, path, err)
	}

	if err := rejectUndecoded(md, path); err != nil {
		return nil, err
	}

	// An unknown version is a HARD REFUSAL naming the file and the version,
	// never a best-effort parse. `version = 1` exists so a future format change
	// is a migration rather than a guess, and guessing at a file that governs
	// what may execute is the wrong kind of helpful.
	if raw.Version != FormatVersion {
		return nil, fmt.Errorf("%w: %s declares version %d, but this build understands version %d only", ErrParse, path, raw.Version, FormatVersion)
	}

	for i, e := range raw.Entries {
		if err := validateEntry(e, i, path); err != nil {
			return nil, err
		}
	}

	return &Store{Version: raw.Version, Entries: raw.Entries, Path: path}, nil
}

// validateEntry enforces the per-entry invariants a hand-edited file could
// otherwise violate. Each one fails CLOSED — the file is refused, so no gate
// matches — because the alternative for every one of them is an entry that
// authorizes more than the operator wrote.
func validateEntry(e Entry, idx int, path string) error {
	where := fmt.Sprintf("entry %d", idx)
	if e.Name != "" {
		where = fmt.Sprintf("entry %q", e.Name)
	}

	if e.Name == "" {
		return fmt.Errorf("%w: %s in %s has no name", ErrParse, where, path)
	}
	if len(e.Argv) == 0 {
		return fmt.Errorf("%w: %s in %s has an empty argv; an entry must name a command", ErrParse, where, path)
	}

	// P4: an entry with NEITHER repo NOR global is a MALFORMED FILE, never
	// "matches everything". A missing binding failing open would make a
	// hand-edited file a bypass — the single most valuable edit an attacker who
	// reached this file could make, and the cheapest one to refuse.
	if !e.Global && e.Repo == "" {
		return fmt.Errorf("%w: %s in %s has neither a repo binding nor global = true; an entry with no binding is refused, never treated as matching every repo", ErrParse, where, path)
	}

	// A global entry with a repo binding is contradictory. Refusing is the
	// closed direction: honoring `global` would widen, honoring `repo` would
	// silently ignore a key the operator wrote.
	if e.Global && e.Repo != "" {
		return fmt.Errorf("%w: %s in %s sets both global = true and repo = %q; an entry binds to one repo or to all, never both", ErrParse, where, path, e.Repo)
	}

	// A stub_reason on a non-stub entry is contradictory (DKT-607). Refusing is
	// the closed direction, same as global+repo above: honoring the reason
	// would imply the entry is a stub the flag denies, and dropping it would
	// silently discard a key the operator wrote.
	if e.StubReason != "" && !e.Stub {
		return fmt.Errorf("%w: %s in %s has a stub_reason but stub is not true; a reason describes a stub, so set stub = true or remove stub_reason", ErrParse, where, path)
	}

	// The stored hash must describe the stored argv. This catches a corrupted
	// or hand-edited file (M3): an operator who edits `argv` without recomputing
	// the hash gets a refusal rather than an entry whose two halves disagree.
	if e.ArgvSHA256 != "" {
		if want := ArgvSHA256(e.Argv); e.ArgvSHA256 != want {
			return fmt.Errorf("%w: %s in %s has argv_sha256 %s, but its argv hashes to %s; the file was edited without recomputing the hash", ErrParse, where, path, e.ArgvSHA256, want)
		}
	}

	return nil
}

// rejectUndecoded turns BurntSushi's undecoded-key report into the strict-mode
// error §3.1 requires, in the same shape internal/workflow's parser uses.
func rejectUndecoded(md toml.MetaData, path string) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}

	// Report deterministically: TOML key order is not stable across runs, and
	// an error message that reorders itself is one nobody can test against.
	keys := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)

	return fmt.Errorf("%w: %s contains unknown key(s): %s", ErrParse, path, strings.Join(keys, ", "))
}

// encode renders a store back to TOML bytes, for the writers in add.go.
//
// The header is written on every save so a hand-editing operator finds the
// same orientation the file was created with.
func encode(st *Store) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Written by `docket trust`. Hand-editing is supported;\n")
	b.WriteString("# docket re-reads this file on every use.\n\n")

	enc := toml.NewEncoder(&b)
	if err := enc.Encode(rawStore{Version: FormatVersion, Entries: st.Entries}); err != nil {
		return nil, fmt.Errorf("encoding the trust store: %w", err)
	}
	return []byte(b.String()), nil
}
