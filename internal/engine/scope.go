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
