package model

import (
	"strings"
	"testing"
)

// DKT-256 — an issue id renders and resolves under the project that OWNS it.
//
// Issue ids are STORE-GLOBAL: DKT-267 and DOT-268 were minted consecutively
// across two different projects. The prefix is therefore purely a rendering
// choice, and rendering it from the READER's cwd made every cross-project
// reference silently wrong rather than loudly absent.

// withOwners installs an owner resolver and a prefix roster for one test, and
// restores both. The package variables are per-invocation constants in
// production; a test that left one set would leak into the next.
func withOwners(t *testing.T, owners map[int]string, roster []string) {
	t.Helper()
	prevResolver, prevRoster, prevDisplay := ownerPrefix, knownProjectPrefixes, displayPrefix
	t.Cleanup(func() {
		ownerPrefix, knownProjectPrefixes, displayPrefix = prevResolver, prevRoster, prevDisplay
	})
	SetKnownProjectPrefixes(roster)
	SetIssueOwnerPrefixResolver(func(id int) string { return owners[id] })
}

// TestFormatIDRendersTheOwningProjectsPrefix is half 1.
//
// The same `run report` row showed DOT-81 read from dotfiles.vorpal and ART-81
// read from artifacts.vorpal — one row, two names, neither flagged. Reproduced
// 4/4 across RUN-9/13/15/16 and again on RUN-17..20 and RUN-29.
func TestFormatIDRendersTheOwningProjectsPrefix(t *testing.T) {
	withOwners(t, map[int]string{81: "DOT", 267: "DKT", 268: "DOT"},
		[]string{"DKT", "DOT", "ART"})

	// The reader is ART. The rows are not.
	SetDisplayPrefix("ART")

	for id, want := range map[int]string{81: "DOT-81", 267: "DKT-267", 268: "DOT-268"} {
		if got := FormatID(id); got != want {
			t.Errorf("FormatID(%d) = %q, want %q — the prefix comes from the "+
				"row's own project, not the reader's", id, got, want)
		}
	}
}

// TestFormatIDFallsBackToTheReadersPrefix: an unresolvable owner renders as it
// always did.
//
// A row whose project cannot be resolved is a smaller problem than one that
// renders as `-42`, and the fallback is what keeps a library caller that never
// wires the resolver working unchanged.
func TestFormatIDFallsBackToTheReadersPrefix(t *testing.T) {
	withOwners(t, map[int]string{}, []string{"DKT", "DOT"})
	SetDisplayPrefix("DOT")

	if got := FormatID(42); got != "DOT-42" {
		t.Errorf("FormatID(42) with no resolvable owner = %q, want DOT-42", got)
	}

	// And with no resolver installed at all.
	SetIssueOwnerPrefixResolver(nil)
	if got := FormatID(42); got != "DOT-42" {
		t.Errorf("FormatID(42) with no resolver = %q, want DOT-42", got)
	}
}

// TestParseIDRefusesAPrefixTheIssueDoesNotBelongTo is half 2.
//
// Live on nightly-31-g8083dea, cwd = docket.git: `docket issue show DOT-20`
// printed `DKT-20 schema/workflow register writes land under project_id=1`.
// The lookup discarded the prefix, resolved 20 under the caller's project, and
// rendered the result under DKT with no warning — the reader asked about one
// issue and was shown another.
func TestParseIDRefusesAPrefixTheIssueDoesNotBelongTo(t *testing.T) {
	withOwners(t, map[int]string{20: "DKT", 213: "FLX"}, []string{"DKT", "DOT", "FLX"})
	SetDisplayPrefix("DKT")

	for _, input := range []string{"DOT-20", "dot-20", "DOT-213"} {
		if _, err := ParseID(input); err == nil {
			t.Errorf("ParseID(%q) resolved silently; a prefix that disagrees "+
				"with the row's owning project must fail loudly rather than "+
				"resolve a different issue wearing the requested number", input)
		}
	}

	// The refusal has to name BOTH projects, or the reader cannot tell whether
	// they typed the wrong prefix or the wrong number.
	_, err := ParseID("DOT-20")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, needle := range []string{"DOT", "DKT", "20"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("the refusal %q does not mention %q", err, needle)
		}
	}
}

// TestParseIDStillResolvesEveryLegitimateForm is the containment half.
//
// Cross-project reads stay legal: `issue list --project` prints other projects'
// ids precisely so they round-trip (DKT-72), and with FormatID rendering from
// the owner those ids are now correct. What half 2 forbids is only the third
// outcome — a DIFFERENT issue wearing the requested number.
func TestParseIDStillResolvesEveryLegitimateForm(t *testing.T) {
	withOwners(t, map[int]string{20: "DKT", 268: "DOT"}, []string{"DKT", "DOT"})
	SetDisplayPrefix("DKT")

	cases := map[string]int{
		"20":      20,  // bare: names no project, asserts nothing
		"DKT-20":  20,  // the owner's own prefix
		"dkt-20":  20,  // case-insensitively
		"DOT-268": 268, // ANOTHER project's issue, named correctly
	}
	for input, want := range cases {
		got, err := ParseID(input)
		if err != nil {
			t.Errorf("ParseID(%q) failed: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseID(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestParseIDIsSilentWithoutAResolver keeps every library caller working.
//
// A caller that never wires the resolver — every test in this repo that does
// not opt in, and any embedder — must see exactly the pre-DKT-256 behavior.
// A check that fired without the information to make it would refuse
// references it cannot judge.
func TestParseIDIsSilentWithoutAResolver(t *testing.T) {
	prevResolver, prevRoster := ownerPrefix, knownProjectPrefixes
	t.Cleanup(func() { ownerPrefix, knownProjectPrefixes = prevResolver, prevRoster })
	SetIssueOwnerPrefixResolver(nil)
	SetKnownProjectPrefixes([]string{"DKT", "DOT"})

	if got, err := ParseID("DOT-20"); err != nil || got != 20 {
		t.Errorf("ParseID(\"DOT-20\") = %d, %v with no resolver installed; "+
			"want 20 and no error — the pre-DKT-256 behavior", got, err)
	}
}

// TestUnresolvableOwnerDeclinesToJudge: a resolver that returns "" must not
// refuse.
//
// "I do not know who owns this" is not "you named the wrong project". Refusing
// there would break every reference to an id the resolver cannot see —
// including one to an issue that was just created in another process.
func TestUnresolvableOwnerDeclinesToJudge(t *testing.T) {
	withOwners(t, map[int]string{}, []string{"DKT", "DOT"})
	if got, err := ParseID("DOT-20"); err != nil || got != 20 {
		t.Errorf("ParseID(\"DOT-20\") = %d, %v with an unresolvable owner; "+
			"want 20 and no error", got, err)
	}
}
