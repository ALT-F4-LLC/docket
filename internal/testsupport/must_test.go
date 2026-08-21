package testsupport

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// mustFatalAttribution matches the Fatalf line's own text so the check
// requires the caller-line reference and the formatted message to appear on
// the SAME line, not merely somewhere in the output. A looser
// strings.Contains(out, "must_test.go:") check is satisfied by any
// must_test.go:-prefixed line at all — a stray t.Logf or an unrelated
// failing assertion in this file would pass it just as well, so it would
// stop discriminating dropped attribution from a coincidentally present
// file reference.
var mustFatalAttribution = regexp.MustCompile(`must_test\.go:\d+: boom: boom, ctx = ok`)

// TestMustPassesThroughOnNilError pins the non-aborting path: Must must not
// halt the test when err is nil.
func TestMustPassesThroughOnNilError(t *testing.T) {
	Must(t, nil, "unexpected failure")
}

// TestMustFatalsOnError proves the abort path fires, and does so at the
// caller's line rather than must.go's. Must's signature takes a concrete
// *testing.T (per this issue's design — no interface indirection), so its
// t.Fatalf call cannot be observed by substituting a fake T from within the
// same test binary; the only faithful way to see the real halt is to run it
// in a subprocess and check that subprocess failed. This is a large test
// (spawns a process) justified because it is pinning behavior a same-process
// assertion cannot reach: "aborts the test" and "attributes to the caller".
func TestMustFatalsOnError(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMustFatalsOnErrorChildHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "TESTSUPPORT_MUST_FATAL_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("subprocess succeeded, want failure from Must's t.Fatalf; output:\n%s", out)
	}
	if !mustFatalAttribution.Match(out) {
		t.Errorf("subprocess output does not show the formatted Fatalf message attributed "+
			"to the caller's line (t.Helper() dropped?); output:\n%s", out)
	}
	if strings.Contains(string(out), "unreachable") {
		t.Errorf("subprocess reached the post-Must sentinel, so Must did not abort; output:\n%s", out)
	}
}

// TestMustFatalsOnErrorChildHelper is the subprocess body for the case
// above. It is a no-op unless the parent set the environment, so it costs
// nothing in an ordinary run (prior art: internal/trust/add_test.go's
// TestTrustAddChildHelper). Splitting it into its own test, rather than
// branching inside TestMustFatalsOnError on the same env var, means a stray
// or inherited TESTSUPPORT_MUST_FATAL_CHILD can only ever select this
// dedicated function — it cannot silently redirect the parent test itself
// into the child's abort path.
func TestMustFatalsOnErrorChildHelper(t *testing.T) {
	if os.Getenv("TESTSUPPORT_MUST_FATAL_CHILD") == "" {
		t.Skip("not a child invocation")
	}
	Must(t, errors.New("boom"), "boom: %v, ctx = %s", "boom", "ok")
	t.Fatal("unreachable: Must should have halted the test above")
}
