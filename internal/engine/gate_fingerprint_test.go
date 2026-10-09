package engine

import (
	"strings"
	"testing"
)

// ptyFailure renders the RUN-95 `pty.Open` capture with the two things that
// vary between two runs of ONE unchanged environmental failure: the worktree
// the gate ran in, and how long it took.
func ptyFailure(worktree, duration string) string {
	return "--- FAIL: TestTUIAttaches (" + duration + ")\n" +
		"    " + worktree + "/internal/tui/attach_test.go:41: pty.Open: operation not permitted\n" +
		"FAIL\tgithub.com/ALT-F4-LLC/docket/internal/tui\t" + duration + "\n"
}

// wholeSuiteCapture renders one `go test ./...` run as the tests gate records
// it: the same failing internal/tui block surrounded by passing-package summary
// lines. appResult is the internal/app summary's result field, which is a
// duration on a fresh run and `(cached)` on a test-cache hit; appSuffix is any
// text `go test` prints after it, such as a coverage figure.
func wholeSuiteCapture(appResult, appSuffix string) string {
	return "?   \tgithub.com/ALT-F4-LLC/docket/cmd/docket\t[no test files]\n" +
		"ok  \tgithub.com/ALT-F4-LLC/docket/internal/app\t" + appResult + appSuffix + "\n" +
		"ok  \tgithub.com/ALT-F4-LLC/docket/internal/engine\t4.102s\n" +
		"--- FAIL: TestTUIAttaches (0.31s)\n" +
		"    attach_test.go:41: pty.Open: operation not permitted\n" +
		"FAIL\n" +
		"FAIL\tgithub.com/ALT-F4-LLC/docket/internal/tui\t0.874s\n" +
		"FAIL\n"
}

// TestGateResultFingerprint: the fingerprint is stable across run-varying text
// and discriminating on failure content. Both directions matter — a fingerprint
// that never repeats restores the per-step toil DKT-546 removed, and one that
// collapses distinct failures restores the gate-wide waiver DKT-1796 removes.
func TestGateResultFingerprint(t *testing.T) {
	t.Run("stable across duration and worktree", func(t *testing.T) {
		a := GateFingerprint(ptyFailure(
			"/private/tmp/claude-501/wf_703267b4-f30-2", "0.31s"))
		b := GateFingerprint(ptyFailure(
			"/Users/o/repo/.claude/worktrees/wf_aaaa-1", "12.874s"))
		if a != b {
			t.Errorf("fingerprints differ across duration and worktree:\n"+
				" a = %s\n b = %s\nthe same environmental failure must keep "+
				"one signature or no grant ever matches it twice", a, b)
		}
	})

	t.Run("stable across ANSI colour and CRLF", func(t *testing.T) {
		plain := "FAIL\tinternal/app\t1.0s\n"
		decorated := "\x1b[31mFAIL\x1b[0m\tinternal/app\t2.5s\r\n"
		if a, b := GateFingerprint(plain), GateFingerprint(decorated); a != b {
			t.Errorf("colour/CRLF changed the fingerprint:\n a = %s\n b = %s", a, b)
		}
	})

	t.Run("stable across timestamps", func(t *testing.T) {
		a := GateFingerprint("2026-09-15T10:04:01Z build failed\n")
		b := GateFingerprint("2026-09-14T23:59:59Z build failed\n")
		if a != b {
			t.Errorf("timestamps changed the fingerprint:\n a = %s\n b = %s", a, b)
		}
	})

	t.Run("stable across a cached passing package in a whole-suite run", func(t *testing.T) {
		for _, suffix := range []string{"", "\tcoverage: 50.0% of statements"} {
			cached := GateFingerprint(wholeSuiteCapture("(cached)", suffix))
			fresh := GateFingerprint(wholeSuiteCapture("2.874s", suffix))
			if cached != fresh {
				t.Errorf("suffix %q: an unrelated package being a test-cache "+
					"hit changed the fingerprint:\n cached = %s\n fresh  = %s\n"+
					"the same failure must keep one signature across whole-suite runs",
					suffix, cached, fresh)
			}
		}
	})

	t.Run("cached text inside a failing block is kept", func(t *testing.T) {
		block := func(assertion string) string {
			return "--- FAIL: TestReplayTiming (0.31s)\n" +
				"    replay_test.go:41: " + assertion + "\n" +
				"FAIL\tgithub.com/ALT-F4-LLC/docket/internal/app\t(cached)\n"
		}
		got := normalizeGateOutput(block("got (cached), want 0.5s"))
		for _, kept := range []string{
			"replay_test.go:41: got (cached), want <dur>",
			"/internal/app\t(cached)",
		} {
			if !strings.Contains(got, kept) {
				t.Errorf("normalization rewrote failing-block text; want %q in:\n%s",
					kept, got)
			}
		}
		a := GateFingerprint(block("got (cached), want 0.5s"))
		b := GateFingerprint(block("got 0.5s, want (cached)"))
		if a == b {
			t.Errorf("assertions differing only in where (cached) appears "+
				"collapsed to one fingerprint (%s)", a)
		}
	})

	t.Run("different test name yields a different fingerprint", func(t *testing.T) {
		pty := GateFingerprint(ptyFailure("/w/a", "0.31s"))
		other := GateFingerprint(
			"--- FAIL: TestIssueCloseRejectsOpenChild (0.31s)\n" +
				"    /w/a/internal/app/issue_test.go:41: want CONFLICT, got nil\n" +
				"FAIL\tgithub.com/ALT-F4-LLC/docket/internal/app\t0.31s\n")
		if pty == other {
			t.Errorf("a real regression hashed identically to the "+
				"environmental failure (%s); the grant would auto-pass it", pty)
		}
	})

	t.Run("different relative path yields a different fingerprint", func(t *testing.T) {
		a := GateFingerprint("/root/one/internal/app/run.go:12: boom\n")
		b := GateFingerprint("/root/one/internal/tui/run.go:12: boom\n")
		if a == b {
			t.Errorf("paths differing only in their package collapsed to one "+
				"fingerprint (%s); normalization must keep the path tail", a)
		}
	})

	t.Run("reordered lines yield a different fingerprint", func(t *testing.T) {
		a := GateFingerprint("first failure\nsecond failure\n")
		b := GateFingerprint("second failure\nfirst failure\n")
		if a == b {
			t.Errorf("line order was normalized away (%s); a different "+
				"failure set must not ride one grant", a)
		}
	})
}
