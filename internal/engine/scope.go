package engine

import "strings"

// Scope non-overlap — R4 of the readiness predicate (TDD §6.5, engine-core §5's
// mutual exclusion).
//
// Scope is a list of path GLOBS an issue declares (`issues.scope_globs`). Two
// steps conflict iff their issues' scope globs intersect, and a step whose
// scope conflicts with a `claimed` or `running` step is not ready.

// ScopesIntersect reports whether two scope-glob LISTS conflict.
//
// S1: an empty or absent list never excludes and is never excluded
// (engine-core §5: "Scope-less issues declare `scope = []` and never exclude").
// This is the dormancy case as well as the common one — every pre-existing
// issue carries NULL — so it is the first branch rather than a special case
// buried in the loop.
func ScopesIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, ga := range a {
		for _, gb := range b {
			if globsIntersect(ga, gb) {
				return true
			}
		}
	}
	return false
}

// globsIntersect reports whether two path globs could both match some path.
//
// S2: THE CONSERVATISM IS THE DESIGN DECISION, and its direction is deliberate.
// Exact glob intersection is undecidable in general for arbitrary patterns, so
// this decomposes each glob into its literal prefix (everything before the
// first wildcard metacharacter) and reports intersection when either prefix is
// a prefix of the other.
//
// The error direction is chosen and is not symmetric in cost:
//
//	over-reporting  ("they intersect" when they do not) delays a step
//	under-reporting ("they don't" when they do)         corrupts a working tree
//
// So this over-reports. `internal/db/**` and `internal/db/leases.go` intersect;
// `internal/db/**` and `internal/cli/**` do not; `**/foo.go` intersects
// EVERYTHING, because its literal prefix is empty and the empty string prefixes
// every path — which is correct and conservative, not a bug to fix later.
func globsIntersect(a, b string) bool {
	pa, pb := literalPrefix(a), literalPrefix(b)
	return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
}

// ScopeWithinDomain reports whether EVERY glob of a scope lies inside a
// workflow's declared `[match] domain_paths` (DKT-1182).
//
// It is deliberately NOT ScopesIntersect, in two ways:
//
//   - It is DIRECTIONAL. "This issue's work belongs to that domain" is a
//     containment question, and intersection answers a different one:
//     `internal/**` intersects `internal/tui/**` while saying nothing about
//     whether the issue is TUI work.
//   - It errs toward SILENCE, where ScopesIntersect errs toward reporting. The
//     costs are reversed: a missed exclusion corrupts a working tree, while a
//     spurious binding warning trains an operator to skim past the ones that
//     are real. So EVERY glob must be inside — an issue scoped half to a
//     domain and half elsewhere is genuinely cross-cutting, and which pipeline
//     should own it is a judgment this lint has no basis to make.
//
// An empty scope or an empty domain is never inside anything: "no scope
// declared" is lintUnscopedHolders' subject, and a workflow that declares no
// domain has opted out of this lint entirely.
func ScopeWithinDomain(scope, domain []string) bool {
	if len(scope) == 0 || len(domain) == 0 {
		return false
	}
	for _, s := range scope {
		inside := false
		for _, d := range domain {
			if globWithin(s, d) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

// globWithin reports whether every path `inner` can match lies under `outer`.
//
// Both are reduced to their literal prefixes, as everywhere else scope is
// compared, and then the test is prefix containment AT A PATH BOUNDARY:
// `internal/tui/` contains `internal/tui/screens/x.go` and does NOT contain
// `internal/tuix/x.go`, which a bare strings.HasPrefix would have accepted.
//
// A domain whose literal prefix is EMPTY — `**/*_test.go`, or a bare `**` —
// contains everything and is therefore refused rather than honored. Under the
// silence-preferring rule above, a domain that matches every issue in the run
// would make the lint fire on all of them and mean nothing; an author who wants
// that is better served by naming the directories.
func globWithin(inner, outer string) bool {
	in, out := literalPrefix(inner), literalPrefix(outer)
	if out == "" {
		return false
	}
	out = strings.TrimSuffix(out, "/")
	if in == out {
		return true
	}
	return strings.HasPrefix(in, out+"/")
}

// literalPrefix returns the leading portion of a glob that contains no wildcard
// metacharacter — the longest path fragment every match is guaranteed to begin
// with.
//
// It stops at the first `*`, `?`, `[`, or `{`. It does NOT trim back to a path
// separator: `internal/db` and `internal/dbx` would then both yield
// `internal/` and be reported as intersecting, which is over-reporting well
// past what correctness requires. Prefix comparison on the raw literal is
// already conservative in the safe direction.
func literalPrefix(glob string) string {
	i := strings.IndexAny(glob, "*?[{")
	if i < 0 {
		return glob
	}
	return glob[:i]
}

// stepExcludesScope reports whether a step in this status excludes another
// step's scope.
//
// S3: exclusion is against `claimed` and `running` steps ONLY. A `done` step is
// finished with the tree; a `pending` one has not touched it. Neither excludes,
// and both would deadlock the schedule if they did — a `pending` step excluding
// on scope would make two steps in one scope permanently unschedulable, since
// each would be waiting on the other's status to change.
//
// A `gated` step is deliberately NOT here: by §6.8 its artifact has recorded
// and its token has retired, so the work against the tree is done and only the
// engine-owned saga remains.
func stepExcludesScope(status string) bool {
	return status == "claimed" || status == "running"
}
