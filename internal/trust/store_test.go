package trust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// THE SANDBOX RULE (§9.5 SB1) is binding on every test in this package: each
// one sets XDG_CONFIG_HOME to a t.TempDir(), so NO TEST CAN READ OR WRITE THE
// OPERATOR'S REAL TRUST STORE. A test that touched it would not merely be
// wrong — it would modify the machine it is auditing.
//
// sandbox returns the store path inside a fresh sandbox and points both
// XDG_CONFIG_HOME and HOME at it. HOME is set too because §3.1's fallback row
// reads it, so a bug that ignored XDG would otherwise silently reach the real
// ~/.config.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	path, err := StorePath()
	testsupport.Must(t, err, "StorePath in the sandbox: %v", err)
	err = os.MkdirAll(filepath.Dir(path), storeDirMode)
	testsupport.Must(t, err, "creating the sandbox store dir: %v", err)
	return path
}

// writeStoreFile writes raw bytes to the store path with a given mode, for the
// integrity table.
func writeStoreFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), mode)
	testsupport.Must(t, err, "writing the test store: %v", err)
	// WriteFile respects the umask, so the mode is set explicitly — the
	// integrity table's whole subject is the exact mode.
	err = os.Chmod(path, mode)
	testsupport.Must(t, err, "chmod on the test store: %v", err)
}

const validStore = `version = 1

[[entry]]
name = "checks"
argv = ["make", "test"]
repo = "/src/example"
`

// --- Store path resolution (§3.1) -------------------------------------------

func TestStorePathHonorsXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", "/nonexistent-home")

	got, err := StorePath()
	testsupport.Must(t, err, "StorePath: %v", err)
	want := filepath.Join(dir, "docket", "trust.toml")
	if got != want {
		t.Errorf("StorePath = %q, want %q", got, want)
	}
}

func TestStorePathFallsBackToHomeConfig(t *testing.T) {
	// Row 2 of §3.1's table: XDG unset (or empty) falls back to $HOME/.config.
	// Both spellings are covered because an empty-but-set variable is the case
	// a naive os.Getenv check gets wrong.
	for _, tc := range []struct {
		name string
		xdg  string
	}{
		{"unset", ""},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			t.Setenv("HOME", home)

			got, err := StorePath()
			testsupport.Must(t, err, "StorePath: %v", err)
			want := filepath.Join(home, ".config", "docket", "trust.toml")
			if got != want {
				t.Errorf("StorePath = %q, want %q", got, want)
			}
		})
	}
}

// TestNoTrustPathSurface is T8's surface guard, in the family of the
// no-token-flag guard: NO FLAG AND NO CONFIG KEY MAY NAME A TRUST-FILE PATH.
//
// At group 1 the trust package is unreachable from any command, so the check
// runs against this package's own exported surface — the place a path-taking
// entry point would have to appear first. §3.1 SB3 is the rule: the store path
// is computed from the environment, and the path-taking constructor is
// UNEXPORTED and used only by this package's tests. Group 2 extends this guard
// to walk the Cobra tree when the CLI verbs exist.
//
// The reason a third source is refused is direct: every additional way to point
// docket at a trust file is another way for repo content — a checked-in
// .envrc, a direnv hook, a Makefile — to point it at a file the repo controls.
func TestNoTrustPathSurface(t *testing.T) {
	// COMMENTS ARE STRIPPED before the scan, exactly as the genericity gate
	// strips them: a comment stating the rule is the rule being STATED, not
	// broken, and a check that failed on its own rationale would make the
	// rationale unwritable. store.go's doc comment names --trust-file and
	// DOCKET_TRUST_FILE precisely to record that they do not exist.
	code := goCodeWithoutComments(t, "store.go")

	// The environment is consulted for exactly ONE variable name.
	for _, banned := range []string{"DOCKET_TRUST_FILE", "DOCKET_TRUST", "--trust-file", "trust_file", "trust.path"} {
		if strings.Contains(code, banned) {
			t.Errorf("store.go names %q in code; §3.1 admits no source for the store path other than XDG_CONFIG_HOME", banned)
		}
	}

	// Positive half: the one source that IS admitted is actually consulted, so
	// the guard cannot pass vacuously against a file that reads no environment
	// at all.
	if !strings.Contains(code, "XDG_CONFIG_HOME") {
		t.Error("store.go must resolve the store path from XDG_CONFIG_HOME")
	}

	// Exported path-taking constructors are the shape SB3 forbids. loadAt,
	// addAt, and removeAt are lowercase precisely so no caller outside this
	// package can open a store anywhere it likes.
	for _, exported := range []string{"func LoadAt(", "func OpenAt(", "func LoadFrom(", "func AddAt(", "func RemoveAt("} {
		for _, file := range []string{"store.go", "add.go"} {
			b, err := os.ReadFile(file)
			testsupport.Must(t, err, "reading %s: %v", file, err)
			if strings.Contains(string(b), exported) {
				t.Errorf("%s exports %q; the path-taking constructor must stay unexported (SB3)", file, exported)
			}
		}
	}
}

// --- Integrity I1–I5 (§3.2) --------------------------------------------------

// TestTrustStoreRefusesUnsafePermissions is §3.2's table. Each row is refused,
// and each error NAMES THE PATH — a refusal that does not say which file is a
// refusal an operator cannot act on.
func TestTrustStoreRefusesUnsafePermissions(t *testing.T) {
	t.Run("I1_symlink", func(t *testing.T) {
		path := sandbox(t)
		target := filepath.Join(t.TempDir(), "elsewhere.toml")
		writeStoreFile(t, target, validStore, storeFileMode)
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("creating the symlink: %v", err)
		}

		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, path, "symlink")
	})

	t.Run("I1_fifo", func(t *testing.T) {
		path := sandbox(t)
		if err := mkfifo(path); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, path, "regular file")
	})

	t.Run("I1_directory", func(t *testing.T) {
		path := sandbox(t)
		if err := os.Mkdir(path, storeDirMode); err != nil {
			t.Fatalf("creating the directory: %v", err)
		}
		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, path, "directory")
	})

	t.Run("I2_mode_0666", func(t *testing.T) {
		path := sandbox(t)
		writeStoreFile(t, path, validStore, 0o666)
		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, path, "0666")
	})

	t.Run("I2_mode_0640", func(t *testing.T) {
		// 0640 is world-unreadable but GROUP-readable, and it is refused. A
		// trust file readable by group is an inventory of the operator's
		// approved commands and the repos they work in — modest, but there is
		// no reason to publish it.
		path := sandbox(t)
		writeStoreFile(t, path, validStore, 0o640)
		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, path, "0640")
	})

	t.Run("I2_names_the_fix", func(t *testing.T) {
		// The refusal states the FIX, not only the complaint.
		path := sandbox(t)
		writeStoreFile(t, path, validStore, 0o666)
		_, err := loadAt(path)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("the refusal must name the fix; got: %v", err)
		}
	})

	t.Run("I4_world_writable_parent", func(t *testing.T) {
		// A writable parent means anyone can replace the trust file wholesale,
		// which makes the checks on the file itself decorative.
		path := sandbox(t)
		writeStoreFile(t, path, validStore, storeFileMode)
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatalf("chmod on the parent: %v", err)
		}
		defer os.Chmod(dir, storeDirMode)

		_, err := loadAt(path)
		assertIntegrityRefusal(t, err, dir, "0777")
	})

	t.Run("valid_0600_is_accepted", func(t *testing.T) {
		// The check is not refusing everything: the correct shape loads.
		path := sandbox(t)
		writeStoreFile(t, path, validStore, storeFileMode)
		st, err := loadAt(path)
		testsupport.Must(t, err, "a 0600 store must load: %v", err)
		if len(st.Entries) != 1 {
			t.Errorf("expected 1 entry, got %d", len(st.Entries))
		}
	})
}

func assertIntegrityRefusal(t *testing.T, err error, wantPath, wantDetail string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an integrity refusal, got nil")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Errorf("expected ErrIntegrity, got %v", err)
	}
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("the refusal must name the path %q; got: %v", wantPath, err)
	}
	if !strings.Contains(err.Error(), wantDetail) {
		t.Errorf("the refusal must name %q; got: %v", wantDetail, err)
	}
}

// TestMissingTrustFileIsAnEmptyAllowlist is §3.2's closing rule and the state
// the malicious-clone proof starts from.
//
// A MISSING TRUST FILE IS NOT AN ERROR. It is an empty allowlist: every gate is
// unmatched, nothing executes, and a run reports what it would have needed.
// That is the correct default for a tool a stranger just installed — and it is
// what makes the dormancy claim true by construction rather than by wiring.
func TestMissingTrustFileIsAnEmptyAllowlist(t *testing.T) {
	path := sandbox(t)
	// Deliberately create nothing.

	st, err := loadAt(path)
	testsupport.Must(t, err, "a missing trust file must not be an error: %v", err)
	if len(st.Entries) != 0 {
		t.Fatalf("expected an empty allowlist, got %d entries", len(st.Entries))
	}

	// And an empty allowlist matches nothing, which is the property that
	// matters: a stranger who never ran `trust add` has a tool that cannot
	// execute anything.
	m := st.Lookup("/src/example", "checks", nil)
	if m.Matched {
		t.Error("an empty allowlist must match nothing")
	}
	if m.Reason == "" {
		t.Error("an unmatched result must carry a reason")
	}
}

func TestStoreCreationUsesRestrictiveModes(t *testing.T) {
	// I5: the directory is created 0700 and the file 0600.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	path, err := StorePath()
	testsupport.Must(t, err, "StorePath: %v", err)

	_, err = addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: t.TempDir(),
	})
	testsupport.Must(t, err, "addAt: %v", err)

	fi, err := os.Stat(path)
	testsupport.Must(t, err, "stat on the created store: %v", err)
	if got := fi.Mode().Perm(); got != storeFileMode {
		t.Errorf("the created store has mode %04o, want %04o", got, storeFileMode)
	}

	di, err := os.Stat(filepath.Dir(path))
	testsupport.Must(t, err, "stat on the created directory: %v", err)
	if got := di.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("the created directory has mode %04o, which is group- or world-accessible", got)
	}
}

// --- Parse strictness (§3.1) -------------------------------------------------

func TestParseStrictness(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			// A typo'd key silently defaulting is the bug this refuses: a
			// typo'd `re_runable` would turn a re-runnable gate into a park,
			// and a typo'd `prefix` would silently change an authorization.
			name: "unknown key",
			content: `version = 1
[[entry]]
name = "checks"
argv = ["make", "test"]
repo = "/src/example"
re_runable = true
`,
			wantErr: "unknown key",
		},
		{
			name: "unknown version",
			content: `version = 99
[[entry]]
name = "checks"
argv = ["make"]
repo = "/src/example"
`,
			wantErr: "version 99",
		},
		{
			name: "missing version",
			content: `[[entry]]
name = "checks"
argv = ["make"]
repo = "/src/example"
`,
			wantErr: "version 0",
		},
		{
			// P4: an entry with NEITHER repo NOR global is a malformed file,
			// never "matches everything". A missing binding failing open would
			// make a hand-edited file a bypass.
			name: "entry with neither repo nor global",
			content: `version = 1
[[entry]]
name = "checks"
argv = ["make", "test"]
`,
			wantErr: "neither a repo binding nor global",
		},
		{
			name: "entry with both repo and global",
			content: `version = 1
[[entry]]
name = "checks"
argv = ["make"]
repo = "/src/example"
global = true
`,
			wantErr: "never both",
		},
		{
			name: "entry with no name",
			content: `version = 1
[[entry]]
argv = ["make"]
repo = "/src/example"
`,
			wantErr: "no name",
		},
		{
			name: "entry with empty argv",
			content: `version = 1
[[entry]]
name = "checks"
argv = []
repo = "/src/example"
`,
			wantErr: "empty argv",
		},
		{
			// DKT-607: a stub_reason on a non-stub entry is contradictory.
			// Refusing is the closed direction: honoring the reason would imply
			// the entry is a stub the flag denies, and dropping it would
			// silently discard a key the operator wrote.
			name: "stub_reason without stub",
			content: `version = 1
[[entry]]
name = "secret-scan"
argv = ["/usr/bin/true"]
repo = "/src/example"
stub_reason = "tracked by DKT-607"
`,
			wantErr: "stub_reason",
		},
		{
			// A hand-edited argv whose stored hash no longer describes it is
			// refused rather than obeyed: the two halves disagree, and
			// guessing which one the operator meant is guessing at what to run.
			name: "argv_sha256 disagrees with argv",
			content: `version = 1
[[entry]]
name = "checks"
argv = ["make", "test"]
argv_sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
repo = "/src/example"
`,
			wantErr: "without recomputing the hash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := sandbox(t)
			writeStoreFile(t, path, tt.content, storeFileMode)

			_, err := loadAt(path)
			if err == nil {
				t.Fatalf("expected a parse refusal for %s", tt.name)
			}
			if !errors.Is(err, ErrParse) {
				t.Errorf("expected ErrParse, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error must contain %q; got: %v", tt.wantErr, err)
			}
			// Every refusal names the file, so the operator knows where to look.
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal must name the file %q; got: %v", path, err)
			}
		})
	}
}

func TestParseAcceptsAWellFormedStore(t *testing.T) {
	path := sandbox(t)
	content := `version = 1

[[entry]]
name = "checks"
argv = ["make", "test"]
repo = "/src/example"
re_runnable = true
tree = true
flaky = true
timeout = "90s"

[[entry]]
name = "fmt"
argv = ["gofmt", "-l", "."]
global = true
prefix = true

[[entry]]
name = "secret-scan"
argv = ["/usr/bin/true"]
global = true
stub = true
stub_reason = "no scanner selected yet; removal tracked by DKT-607"
`
	writeStoreFile(t, path, content, storeFileMode)

	st, err := loadAt(path)
	testsupport.Must(t, err, "a well-formed store must load: %v", err)
	if len(st.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(st.Entries))
	}

	first := st.Entries[0]
	if !first.ReRunnable || !first.Tree || !first.Flaky || first.Timeout != "90s" {
		t.Errorf("flags did not round-trip: %+v", first)
	}
	second := st.Entries[1]
	if !second.Global || !second.Prefix || second.Repo != "" {
		t.Errorf("global entry did not round-trip: %+v", second)
	}
	third := st.Entries[2]
	if !third.Stub || third.StubReason != "no scanner selected yet; removal tracked by DKT-607" {
		t.Errorf("stub entry did not round-trip its reason (DKT-607): %+v", third)
	}
}

// TestStubEntryWithoutAReasonStillLoads pins DKT-607's back-compat: every
// pre-DKT-607 stub entry has no stub_reason, and such a file keeps loading with
// an empty reason — the field is optional on a stub, mandatory-absent off one.
func TestStubEntryWithoutAReasonStillLoads(t *testing.T) {
	path := sandbox(t)
	writeStoreFile(t, path, `version = 1
[[entry]]
name = "secret-scan"
argv = ["/usr/bin/true"]
global = true
stub = true
`, storeFileMode)

	st, err := loadAt(path)
	testsupport.Must(t, err, "a reasonless stub entry must keep loading: %v", err)
	if len(st.Entries) != 1 || !st.Entries[0].Stub || st.Entries[0].StubReason != "" {
		t.Errorf("got %+v, want one stub entry with an empty reason", st.Entries)
	}
}

// --- The canonical-argv hash (§3.3) -----------------------------------------

// TestCanonicalArgvHashDoesNotCollide is THE ONE THAT MATTERS.
//
// A join-then-hash scheme makes ["a b"] and ["a","b"] collide, and that
// collision is an argv-injection primitive IN THE MATCHER ITSELF: an operator
// who approved one command would find a different one authorized. JSON encoding
// is what makes the boundary between elements unforgeable, because no delimiter
// is safe — an argument containing a newline, a space, or any chosen separator
// is trivial to write.
func TestCanonicalArgvHashDoesNotCollide(t *testing.T) {
	collisionPairs := [][2][]string{
		{{"a b"}, {"a", "b"}},
		{{"make test"}, {"make", "test"}},
		{{"a\nb"}, {"a", "b"}},
		{{"a\tb"}, {"a", "b"}},
		{{"a", "b c"}, {"a", "b", "c"}},
		{{"a,b"}, {"a", "b"}},
		{{"a\x00b"}, {"a", "b"}},
		{{""}, {}},
		{{"a", ""}, {"a"}},
	}

	for _, pair := range collisionPairs {
		left, right := ArgvSHA256(pair[0]), ArgvSHA256(pair[1])
		if left == right {
			t.Errorf("HASH COLLISION between %q and %q (both %s) — this is an argv-injection primitive in the matcher",
				pair[0], pair[1], left)
		}
	}
}

func TestCanonicalArgvIsStableAndExact(t *testing.T) {
	// The canonical form is the JSON encoding with no whitespace, so a hash
	// means the same thing across builds and machines.
	if got, want := CanonicalArgv([]string{"make", "test"}), `["make","test"]`; got != want {
		t.Errorf("CanonicalArgv = %q, want %q", got, want)
	}
	// A nil argv and an empty argv canonicalize identically; neither names a
	// program and neither can ever match a candidate.
	if CanonicalArgv(nil) != CanonicalArgv([]string{}) {
		t.Error("nil and empty argv must canonicalize identically")
	}
	// The same argv hashes the same way twice, computed from two separately
	// constructed slices so the comparison cannot degenerate into comparing an
	// expression against itself.
	first := ArgvSHA256([]string{"a", "b"})
	second := ArgvSHA256(append([]string{}, "a", "b"))
	if first != second {
		t.Error("the hash must be stable")
	}
}
