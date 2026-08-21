package engine

import "testing"

// TestScopeIntersection is §6.5's S2 table, with the exact globs the TDD names
// as explicit cases plus the conservative direction stated as an assertion
// rather than a comment.
func TestScopeIntersection(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
		why  string
	}{
		{
			name: "a directory tree contains a file in it",
			a:    "internal/db/**", b: "internal/db/leases.go",
			want: true,
			why:  "the TDD names this pair as intersecting",
		},
		{
			name: "sibling trees do not intersect",
			a:    "internal/db/**", b: "internal/cli/**",
			want: false,
			why:  "the TDD names this pair as NOT intersecting",
		},
		{
			name: "a leading wildcard intersects everything",
			a:    "**/foo.go", b: "internal/db/leases.go",
			want: true,
			why: "its literal prefix is empty, and the empty string prefixes " +
				"every path — correct and conservative, not a bug",
		},
		{
			name: "a leading wildcard intersects another leading wildcard",
			a:    "**/foo.go", b: "**/bar.go",
			want: true,
			why:  "both literal prefixes are empty",
		},
		{
			name: "identical globs intersect",
			a:    "cmd/docket/main.go", b: "cmd/docket/main.go",
			want: true,
		},
		{
			name: "distinct literal paths do not intersect",
			a:    "cmd/docket/main.go", b: "cmd/docket/other.go",
			want: false,
		},
		{
			name: "a prefix that is not a path boundary still intersects",
			a:    "internal/db*", b: "internal/dbx/thing.go",
			want: true,
			why: "over-reporting delays a step; under-reporting corrupts a " +
				"working tree, so the ambiguous case resolves toward conflict",
		},
		{
			name: "a single-star tree intersects a file beneath it",
			a:    "docs/*", b: "docs/design/engine-spec.md",
			want: true,
		},
		{
			name: "character-class metacharacters begin the wildcard tail",
			a:    "internal/[dc]*", b: "internal/db/leases.go",
			want: true,
		},
		{
			name: "brace metacharacters begin the wildcard tail",
			a:    "internal/{db,cli}/**", b: "internal/db/leases.go",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Symmetric, per S4: exclusion is not directional.
			if got := globsIntersect(tc.a, tc.b); got != tc.want {
				t.Errorf("globsIntersect(%q, %q) = %v, want %v — %s",
					tc.a, tc.b, got, tc.want, tc.why)
			}
			if got := globsIntersect(tc.b, tc.a); got != tc.want {
				t.Errorf("globsIntersect(%q, %q) = %v, want %v (asymmetric!)",
					tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestScopelessNeverExcludes is S1, stated in both directions: an issue with no
// declared scope neither excludes nor is excluded (engine-core §5).
//
// This is the DORMANCY case as much as a scheduling one — every pre-existing
// issue carries NULL — so a regression here would make a workflow-free repo's
// steps mutually exclusive for no reason.
func TestScopelessNeverExcludes(t *testing.T) {
	held := []string{"internal/db/**"}

	cases := []struct {
		name string
		a, b []string
	}{
		{"nil against a real scope", nil, held},
		{"a real scope against nil", held, nil},
		{"empty slice against a real scope", []string{}, held},
		{"a real scope against empty slice", held, []string{}},
		{"both absent", nil, nil},
		{"both empty", []string{}, []string{}},
		{
			"even a universal glob does not exclude a scope-less issue",
			[]string{"**"}, nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ScopesIntersect(tc.a, tc.b) {
				t.Errorf("ScopesIntersect(%v, %v) = true, want false — "+
					"a scope-less issue never excludes and is never excluded (S1)",
					tc.a, tc.b)
			}
		})
	}
}

// TestScopeListsIntersectOnAnyPair pins that intersection is over the LISTS: a
// single overlapping glob anywhere in either list is a conflict, since a step
// holding any part of another's scope can corrupt it.
func TestScopeListsIntersectOnAnyPair(t *testing.T) {
	a := []string{"docs/**", "internal/cli/**"}
	b := []string{"cmd/**", "internal/cli/next.go"}

	if !ScopesIntersect(a, b) {
		t.Error("ScopesIntersect = false, want true: internal/cli/** contains internal/cli/next.go")
	}

	disjoint := []string{"cmd/**", "scripts/**"}
	if ScopesIntersect(a, disjoint) {
		t.Error("ScopesIntersect = true, want false: no pair of globs overlaps")
	}
}

// TestOnlyClaimedAndRunningExclude is S3, one case per PERSISTED status.
//
// Enumerating all nine rather than spot-checking two is deliberate: a status
// added later defaults to non-excluding, and the failure mode of getting it
// wrong is a silent double-claim on one working tree — the exact thing scope
// exists to prevent. `gated` is the interesting non-excluding case: its
// artifact has recorded and its token retired (§6.8), so the work against the
// tree is over even though the step is not terminal.
func TestOnlyClaimedAndRunningExclude(t *testing.T) {
	cases := []struct {
		status string
		want   bool
		why    string
	}{
		{"pending", false, "has not touched the tree; excluding would deadlock the pair"},
		{"claimed", true, "a holder may be writing"},
		{"running", true, "a holder is writing"},
		{"gated", false, "the artifact recorded and the token retired; the tree work is done"},
		{"done", false, "finished with the tree"},
		{"waiting-human", false, "parked; no holder"},
		{"skipped", false, "never ran"},
		{"superseded", false, "replaced; no holder"},
		{"failed-routed", false, "terminal record; no holder"},
	}

	if len(cases) != 9 {
		t.Fatalf("enumerated %d statuses, want the 9 persisted ones (§6.2)", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			if got := stepExcludesScope(tc.status); got != tc.want {
				t.Errorf("stepExcludesScope(%q) = %v, want %v — %s",
					tc.status, got, tc.want, tc.why)
			}
		})
	}
}

// TestLiteralPrefix pins the decomposition S2 rests on, including the decision
// NOT to trim back to a path separator.
func TestLiteralPrefix(t *testing.T) {
	cases := []struct{ glob, want string }{
		{"internal/db/**", "internal/db/"},
		{"internal/db/leases.go", "internal/db/leases.go"},
		{"**/foo.go", ""},
		{"docs/*", "docs/"},
		{"internal/[dc]*", "internal/"},
		{"internal/{db,cli}/**", "internal/"},
		{"a?c", "a"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := literalPrefix(tc.glob); got != tc.want {
			t.Errorf("literalPrefix(%q) = %q, want %q", tc.glob, got, tc.want)
		}
	}

	// The deliberate NON-trimming: `internal/db` keeps its full literal rather
	// than collapsing to `internal/`, so it does not over-report against
	// `internal/cli/**`.
	if globsIntersect("internal/db", "internal/cli/**") {
		t.Error("internal/db intersects internal/cli/** — the literal prefix " +
			"was trimmed back to a path separator, over-reporting well past " +
			"what conservatism requires")
	}
}
