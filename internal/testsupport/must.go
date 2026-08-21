// Package testsupport holds helpers shared across the repo's test packages.
// It depends on nothing else in the module, so no cycle can arise from
// importing it. This repo's own suite only exercises the internal-package
// import path (package foo) today; no package foo_test file exists in this
// tree to exercise the external-package import path. Nothing in the
// package's zero-dependency design blocks that import, but that is
// INFERRED, not observed here — the first downstream issue that adds a
// package foo_test file importing testsupport should confirm and report
// back if anything differs.
//
// testsupport is test-only and must never reach the shipped binary: it
// imports "testing", which registers command-line flags in init(), and
// nothing should carry that into production. TestAbsentFromProductionBinary
// (boundary_test.go) guards that this package — and "testing" itself —
// stays out of cmd/docket's dependency graph.
package testsupport

import "testing"

// Must fails the test immediately if err is non-nil, reporting msg (formatted
// with args, in the style of t.Fatalf) at the caller's line.
//
// Go cannot spread a multi-value call into Must alongside the message
// arguments (no legal generic-closure or spread rescues it — verified against
// go1.26.5), so a producer returning (T, error) is a two-line call:
//
//	v, err := f()
//	testsupport.Must(t, err, "f: %v", err)
//
// A single-value producer (a bare `err := f()`) is NOT bound by that
// compiler restriction — the spread problem only exists when there is a
// value beyond err to receive. Its two-line shape instead follows from
// this signature choice: Must does not format err itself, so a caller
// whose message references the error (the operator ruling on preserving
// each site's message text means most do) must bind err to a local so it
// can be passed twice — once as the abort condition, once as a %v operand.
// This is a property of Must's signature, not of the language, and the two
// references must be kept in sync at each call site.
func Must(t *testing.T, err error, msg string, args ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf(msg, args...)
	}
}
