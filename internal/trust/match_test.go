package trust

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// storeWith builds an in-memory snapshot for the matcher table. The matcher is
// pure, so these cases need no file at all — which is the property that makes
// the security-critical logic testable exhaustively.
func storeWith(entries ...Entry) *Store {
	for i := range entries {
		if entries[i].ArgvSHA256 == "" {
			entries[i].ArgvSHA256 = ArgvSHA256(entries[i].Argv)
		}
	}
	return &Store{Version: FormatVersion, Entries: entries}
}

// --- The matching table M1–M6 (§7.2) ----------------------------------------

func TestMatchingTable(t *testing.T) {
	const thisRepo = "/src/example"
	const otherRepo = "/src/other"

	tests := []struct {
		name string
		// store, the repo we are standing in, the gate, and the candidate argv
		// (nil for a named gate, non-nil for a fence line).
		store     *Store
		repo      string
		gate      string
		candidate []string

		wantMatched bool
		wantArgv    []string
		wantReason  string // substring
	}{
		{
			name:        "M1 exact match on a named gate returns the entry argv",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "checks",
			wantMatched: true,
			wantArgv:    []string{"make", "test"},
		},
		{
			// M2: a name that exists only for another repo is unmatched, and
			// the reason NAMES THAT REPO. This is the moved-repo diagnostic —
			// without it an operator cannot tell "I moved the repo" from "I am
			// standing in a clone".
			name:        "M2 name matches but the repo does not",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: otherRepo}),
			repo:        thisRepo,
			gate:        "checks",
			wantMatched: false,
			wantReason:  otherRepo,
		},
		{
			name:        "M2 a global entry matches any repo",
			store:       storeWith(Entry{Name: "fmt", Argv: []string{"gofmt", "-l", "."}, Global: true}),
			repo:        thisRepo,
			gate:        "fmt",
			wantMatched: true,
			wantArgv:    []string{"gofmt", "-l", "."},
		},
		{
			name:        "M2 a global entry matches a different repo too",
			store:       storeWith(Entry{Name: "fmt", Argv: []string{"gofmt"}, Global: true}),
			repo:        otherRepo,
			gate:        "fmt",
			wantMatched: true,
			wantArgv:    []string{"gofmt"},
		},
		{
			name:        "M3 a fence line hash-equal to the entry matches",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "checks",
			candidate:   []string{"make", "test"},
			wantMatched: true,
			wantArgv:    []string{"make", "test"},
		},
		{
			name:        "M3 a fence line differing from the entry does not match",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "checks",
			candidate:   []string{"make", "deploy"},
			wantMatched: false,
		},
		{
			name:        "M4 a prefix entry that opted in matches a longer argv",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make"}, Repo: thisRepo, Prefix: true}),
			repo:        thisRepo,
			gate:        "checks",
			candidate:   []string{"make", "test"},
			wantMatched: true,
			wantArgv:    []string{"make", "test"},
		},
		{
			// M6: byte-exact on every element. Normalization is a
			// match-widening operation, and every match-widening operation is
			// an authorization decision.
			name:        "M6 matching is case-sensitive",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "checks",
			candidate:   []string{"MAKE", "test"},
			wantMatched: false,
		},
		{
			name:        "M6 the gate name is case-sensitive",
			store:       storeWith(Entry{Name: "checks", Argv: []string{"make"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "CHECKS",
			wantMatched: false,
		},
		{
			// M5: NO ENTRY means unmatched. Never a fallback, never a
			// default-allow, never a "this command looks safe" heuristic.
			name:        "M5 no entry at all is unmatched with a reason",
			store:       storeWith(),
			repo:        thisRepo,
			gate:        "checks",
			wantMatched: false,
			wantReason:  "no trust entry",
		},
		{
			name:        "M5 a different gate name does not match",
			store:       storeWith(Entry{Name: "other", Argv: []string{"make"}, Repo: thisRepo}),
			repo:        thisRepo,
			gate:        "checks",
			wantMatched: false,
			wantReason:  "no trust entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.store.Lookup(tt.repo, tt.gate, tt.candidate)

			if got.Matched != tt.wantMatched {
				t.Fatalf("Matched = %v, want %v (reason: %s)", got.Matched, tt.wantMatched, got.Reason)
			}
			if tt.wantMatched && !reflect.DeepEqual(got.Argv, tt.wantArgv) {
				t.Errorf("Argv = %q, want %q", got.Argv, tt.wantArgv)
			}
			if !tt.wantMatched {
				// N2: every unmatched result carries a reason, because the
				// four causes need four different remedies.
				if got.Reason == "" {
					t.Error("an unmatched result must carry a reason")
				}
				if got.Argv != nil {
					t.Errorf("an unmatched result must carry no argv, got %q", got.Argv)
				}
				if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
					t.Errorf("reason %q must contain %q", got.Reason, tt.wantReason)
				}
			}
		})
	}
}

// TestPrefixMatchingRequiresOptIn is T10's closure, asserted in BOTH
// DIRECTIONS as §9.1 requires.
//
// The two directions are different bugs. Forward: a full-argv entry must never
// match a longer candidate, or every entry silently becomes a prefix entry and
// `--prefix` means nothing. Backward: a prefix entry must not match a SHORTER
// candidate, or an entry for `make test` would authorize a bare `make`.
func TestPrefixMatchingRequiresOptIn(t *testing.T) {
	const repo = "/src/example"

	t.Run("a full-argv entry does NOT match by prefix", func(t *testing.T) {
		st := storeWith(Entry{Name: "checks", Argv: []string{"make"}, Repo: repo, Prefix: false})

		got := st.Lookup(repo, "checks", []string{"make", "test"})
		if got.Matched {
			t.Fatal("a full-argv entry must never match a longer candidate by prefix")
		}
		// The reason distinguishes this cause from the others, because its
		// remedy is different: re-add with --prefix.
		if !strings.Contains(got.Reason, "did not opt in to prefix matching") {
			t.Errorf("the reason must name the missing opt-in; got: %s", got.Reason)
		}
	})

	t.Run("a prefix entry does NOT match a shorter candidate", func(t *testing.T) {
		st := storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: repo, Prefix: true})

		got := st.Lookup(repo, "checks", []string{"make"})
		if got.Matched {
			t.Fatal("a prefix entry must not match a candidate shorter than itself")
		}
	})

	t.Run("a prefix entry that opted in does match", func(t *testing.T) {
		st := storeWith(Entry{Name: "checks", Argv: []string{"make"}, Repo: repo, Prefix: true})

		got := st.Lookup(repo, "checks", []string{"make", "anything"})
		if !got.Matched {
			t.Fatalf("an opted-in prefix entry must match: %s", got.Reason)
		}
		// The CANDIDATE argv is what runs for a prefix match — that is what the
		// operator authorized: this command line, beginning with the prefix.
		if !reflect.DeepEqual(got.Argv, []string{"make", "anything"}) {
			t.Errorf("Argv = %q, want the candidate argv", got.Argv)
		}
	})
}

// TestPrefixMatchingIsElementWiseNotStringPrefix is the trap row.
//
// ELEMENT-WISE, NEVER STRING-PREFIX, because the wrong reading is both easy and
// dangerous: a string-prefix comparison lets ["make"] match ["make-release",
// "--prod"], authorizing a command the operator never saw and would never have
// approved.
func TestPrefixMatchingIsElementWiseNotStringPrefix(t *testing.T) {
	const repo = "/src/example"
	st := storeWith(Entry{Name: "checks", Argv: []string{"make"}, Repo: repo, Prefix: true})

	for _, candidate := range [][]string{
		{"make-release", "--prod"},
		{"makefoo"},
		{"make_test"},
		{"maketest"},
	} {
		got := st.Lookup(repo, "checks", candidate)
		if got.Matched {
			t.Errorf("prefix [make] must NOT match %q — the comparison is per element, never per character", candidate)
		}
	}

	// The positive control: the same entry DOES match a real element-wise
	// extension, so the test is not passing by refusing everything.
	if got := st.Lookup(repo, "checks", []string{"make", "test"}); !got.Matched {
		t.Error("prefix [make] must match [make test]")
	}
}

// TestMatchReturnsTheExecutedArgv is T4's closure, asserted on the TYPE.
//
// The TOCTOU shape this forecloses: a matcher that returns a bare boolean
// produces a PERMISSION, which a caller then applies to an argv it is holding
// separately — and between the permission and the spawn, that argv can be
// rewritten by a concurrent `trust add`, a store rewrite, or a symlink swap.
// Because Lookup returns THE ARGV ITSELF, there is no second source for a
// caller to get wrong and no window to exploit. There are not two reads of two
// different things.
func TestMatchReturnsTheExecutedArgv(t *testing.T) {
	const repo = "/src/example"
	entryArgv := []string{"make", "test", "--verbose"}
	st := storeWith(Entry{Name: "checks", Argv: entryArgv, Repo: repo})

	got := st.Lookup(repo, "checks", nil)
	if !got.Matched {
		t.Fatalf("expected a match: %s", got.Reason)
	}

	// The match CARRIES the argv to execute.
	if !reflect.DeepEqual(got.Argv, entryArgv) {
		t.Fatalf("the match must carry the argv to execute; got %q, want %q", got.Argv, entryArgv)
	}

	// And it carries the entry, so the runner reads the timeout and flags from
	// the same matched object rather than looking them up again.
	if got.Entry == nil {
		t.Fatal("the match must carry its entry, so flags come from the matched object and not a second lookup")
	}

	// The returned argv is a COPY: a caller that mutates it cannot reach back
	// into the snapshot and change what a later match returns.
	got.Argv[0] = "rm"
	again := st.Lookup(repo, "checks", nil)
	if again.Argv[0] != "make" {
		t.Error("the match's argv must be a copy; mutating it changed the store snapshot")
	}

	// THE TYPE-LEVEL HALF: this package exposes no API that answers the trust
	// question with a bare boolean. Such an API is the TOCTOU shape itself, and
	// the guard is a source-level check because the rule is one line to state
	// and one convenience function to violate.
	for _, forbidden := range []string{
		"func (s *Store) IsTrusted(",
		"func (s *Store) Allows(",
		"func (s *Store) Permits(",
		"func IsTrusted(",
	} {
		if strings.Contains(goCodeWithoutComments(t, "match.go"), forbidden) {
			t.Errorf("match.go exposes %q; a boolean answer is the TOCTOU shape T4 forecloses", forbidden)
		}
	}
}

// --- Repo identity P1–P4 (§3.4) ---------------------------------------------

// TestRepoIdentityResolvesSymlinks is P1.
//
// Without EvalSymlinks, an attacker who can create a symlink in the operator's
// home makes a fork REPORT the trusted path. Resolving both sides to their real
// paths defeats it.
func TestRepoIdentityResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	realResolved, err := filepath.EvalSymlinks(real)
	testsupport.Must(t, err, "resolving the real path: %v", err)

	link := filepath.Join(t.TempDir(), "link-to-repo")
	err = os.Symlink(real, link)
	testsupport.Must(t, err, "creating the symlink: %v", err)

	got, err := RepoIdentity(link)
	testsupport.Must(t, err, "RepoIdentity: %v", err)
	if got != realResolved {
		t.Errorf("RepoIdentity(%q) = %q, want the resolved path %q", link, got, realResolved)
	}

	// The symlinked path and the real path produce the SAME identity, which is
	// what makes the dodge fail.
	direct, err := RepoIdentity(real)
	testsupport.Must(t, err, "RepoIdentity on the real path: %v", err)
	if direct != got {
		t.Errorf("a symlinked and a direct path must yield the same identity: %q vs %q", got, direct)
	}
}

// TestTwoCheckoutsAreDistinctIdentities is the malicious-clone argument, as a
// test. It is the reason the identity is a PATH and not a git remote, a root
// commit, or a stored UUID — every one of those is repo-controlled and a clone
// carries it.
func TestTwoCheckoutsAreDistinctIdentities(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "docket")
	clone := filepath.Join(parent, "docket-fork")
	for _, d := range []string{original, clone} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}

	origID, err := RepoIdentity(original)
	testsupport.Must(t, err, "RepoIdentity(original): %v", err)
	cloneID, err := RepoIdentity(clone)
	testsupport.Must(t, err, "RepoIdentity(clone): %v", err)
	if origID == cloneID {
		t.Fatal("two checkouts must have distinct identities, or a hostile clone inherits every entry")
	}

	// The proof, end to end: an entry approved in the original does NOT match
	// in the clone. The clone executes nothing without a new, explicit
	// `trust add` performed while standing in it.
	st := storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: origID})

	if m := st.Lookup(origID, "checks", nil); !m.Matched {
		t.Errorf("the entry must match in the repo it was approved in: %s", m.Reason)
	}
	if m := st.Lookup(cloneID, "checks", nil); m.Matched {
		t.Fatal("A HOSTILE CLONE MUST NOT INHERIT TRUST — this is the malicious-clone threat")
	}
}

// TestMovedRepoUnmatchedReasonNamesTheOldPath is §3.4's recorded cost, with the
// diagnostic that makes it survivable.
//
// Moving a repository invalidates its trust entries. That is the CORRECT
// direction of failure — a moved repo re-earns trust; a hostile clone never
// inherits it — but "unmatched" with no explanation sends an operator hunting.
// The reason names the bound path so they can see at once what happened.
func TestMovedRepoUnmatchedReasonNamesTheOldPath(t *testing.T) {
	const oldPath = "/src/where-it-used-to-be"
	const newPath = "/src/where-it-is-now"

	st := storeWith(Entry{Name: "checks", Argv: []string{"make", "test"}, Repo: oldPath})

	got := st.Lookup(newPath, "checks", nil)
	if got.Matched {
		t.Fatal("an entry bound to a different path must not match")
	}
	if !strings.Contains(got.Reason, oldPath) {
		t.Errorf("the reason must NAME THE BOUND PATH so the operator can see the repo moved; got: %s", got.Reason)
	}
	if !strings.Contains(got.Reason, "bound to a different repo") {
		t.Errorf("the reason must distinguish this cause from 'no entry'; got: %s", got.Reason)
	}
	// DKT-64: the moved-path remedy is now an ASIDE, not the lead. This branch
	// fires whenever an entry of the name exists in another repository, and the
	// common case is a repository that simply never had one — five projects hit
	// it on a gate bound to a different repo, and a message leading with
	// "restore the repo to that path" sent every one of them hunting for a path
	// problem that did not exist. Both facts are still present; what changed is
	// which one the operator reads first.
	if !strings.HasPrefix(got.Reason, `gate "checks": no trust entry for this repo`) {
		t.Errorf("the reason must LEAD with the absence here, since that is the "+
			"case an operator is almost always in; got: %s", got.Reason)
	}
	if strings.Contains(got.Reason, "restore the repo to that path if it was moved") {
		t.Errorf("the moved-path remedy is asserted rather than offered "+
			"conditionally; got: %s", got.Reason)
	}
}

// TestP4MalformedBindingIsRefusedNotMatchedEverywhere is P4 at the matcher.
//
// An entry with neither binding is refused AT PARSE TIME, so it can never reach
// the matcher — a file containing one does not load at all. That is stronger
// than refusing it at match time and is the direction that matters: a missing
// binding failing OPEN would make a hand-edited file a bypass.
func TestP4MalformedBindingIsRefusedNotMatchedEverywhere(t *testing.T) {
	path := sandbox(t)
	writeStoreFile(t, path, `version = 1
[[entry]]
name = "checks"
argv = ["make", "test"]
`, storeFileMode)

	_, err := loadAt(path)
	if err == nil {
		t.Fatal("an entry with neither repo nor global must make the FILE refuse to load")
	}
	if !strings.Contains(err.Error(), "never treated as matching every repo") {
		t.Errorf("the refusal must say what it is refusing to do; got: %v", err)
	}
}

// TestGlobalRequiresExplicitOptIn is P3: there is no implicit path to a global
// entry. A binding is computed for every add that did not ask for global.
func TestGlobalRequiresExplicitOptIn(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	res, err := addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo})
	testsupport.Must(t, err, "addAt: %v", err)
	if res.Entry.Global {
		t.Fatal("an add that did not ask for global must not produce a global entry")
	}
	if res.Entry.Repo == "" {
		t.Fatal("an add that did not ask for global must bind to a repo")
	}

	// And the explicit flag does produce one, with no binding.
	res, err = addAt(path, AddRequest{Name: "fmt", Argv: []string{"gofmt"}, Global: true})
	testsupport.Must(t, err, "addAt --global: %v", err)
	if !res.Entry.Global || res.Entry.Repo != "" {
		t.Errorf("a global entry must carry global = true and no repo binding: %+v", res.Entry)
	}
}
