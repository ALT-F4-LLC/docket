package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestGrantRequiresFingerprintMatch is DKT-1796's whole contract at the seam
// the authority is spent through.
//
// Before it, `grantMatches` compared gate, exit and reason — and `reason` is
// empty for an ordinary failure — so one ruling on a sandbox artifact covered
// every later failure of that gate with that exit, a real regression included.
// The subtests below are the adversarial and the benign case together: the
// control has to stop the regression WITHOUT costing DKT-546 the repeat cover
// it exists to provide.
func TestGrantRequiresFingerprintMatch(t *testing.T) {
	t.Run("a different failure on the same gate and exit parks", func(t *testing.T) {
		conn := mustDB(t)
		runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

		gates := &exitGates{fail: true, exit: 1, output: environmentalFailure}
		e := testEngine()
		e.Gates = gates

		implementID := stepIDInRun(t, conn, runID, "implement@0")
		parkThroughFailingGate(t, conn, e, implementID)
		testsupport.Must(t, e.ResolveStepBatch(conn, implementID,
			ResolveOverridePass, "sandbox artifact, not a code defect", nowMS+1),
			"batch resolve")

		grants, err := db.GateOverrideGrantsForRun(conn, runID)
		testsupport.Must(t, err, "reading grants: %v", err)
		if len(grants) != 1 {
			t.Fatalf("grants = %d, want 1", len(grants))
		}
		if grants[0].Fingerprint == "" {
			t.Fatal("the grant recorded no fingerprint; a ruling with no " +
				"signature is the gate-wide waiver DKT-1796 removes")
		}

		// The SAME gate fails with the SAME exit 1 and an empty reason — the
		// whole pre-fix signature — but the content is a real Go test failure
		// in a package the operator never looked at.
		gates.mu.Lock()
		gates.output = "--- FAIL: TestIssueCloseRejectsOpenChild (0.02s)\n" +
			"    /w/internal/app/issue_test.go:88: want CONFLICT, got nil\n" +
			"FAIL\tgithub.com/ALT-F4-LLC/docket/internal/app\t0.02s\n"
		gates.mu.Unlock()

		packageID := stepIDInRun(t, conn, runID, "package@0")
		claim, err := ClaimStep(conn, packageID,
			ClaimOptions{Owner: "w2", NowMS: nowMS + 2})
		testsupport.Must(t, err, "claim package: %v", err)
		testsupport.Must(t, e.CompleteStep(conn, packageID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("the package record"),
			NowMS: nowMS + 2,
		}), "complete package")

		step, err := db.GetStep(conn, packageID)
		testsupport.Must(t, err, "GetStep package: %v", err)
		if step.Status != db.StepWaitingHuman {
			t.Fatalf("package@0 = %q, want %q — a failure whose CONTENT the "+
				"operator never read must park, not ride the grant",
				step.Status, db.StepWaitingHuman)
		}
		if got := eventKindCount(t, conn, runID, EventStepBatchOverridden); got != 0 {
			t.Errorf("%s events = %d, want 0 — no authority may be spent on "+
				"an unread failure", EventStepBatchOverridden, got)
		}
	})

	t.Run("the same failure content is still covered", func(t *testing.T) {
		conn := mustDB(t)
		runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

		gates := &exitGates{fail: true, exit: 1, output: environmentalFailure}
		e := testEngine()
		e.Gates = gates

		implementID := stepIDInRun(t, conn, runID, "implement@0")
		parkThroughFailingGate(t, conn, e, implementID)
		testsupport.Must(t, e.ResolveStepBatch(conn, implementID,
			ResolveOverridePass, "sandbox artifact, not a code defect", nowMS+1),
			"batch resolve")

		packageID := stepIDInRun(t, conn, runID, "package@0")
		claim, err := ClaimStep(conn, packageID,
			ClaimOptions{Owner: "w2", NowMS: nowMS + 2})
		testsupport.Must(t, err, "claim package: %v", err)
		testsupport.Must(t, e.CompleteStep(conn, packageID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("the package record"),
			NowMS: nowMS + 2,
		}), "complete package")

		step, err := db.GetStep(conn, packageID)
		testsupport.Must(t, err, "GetStep package: %v", err)
		if step.Status != db.StepDone {
			t.Fatalf("package@0 = %q, want %q — an identical repeat of the "+
				"ruled-on failure must still be covered, or DKT-546's toil "+
				"reduction is gone", step.Status, db.StepDone)
		}

		// AC3: both edges of the ledger name the signature, so a status report
		// can say WHICH failure the authority was minted on and spent on.
		grants, err := db.GateOverrideGrantsForRun(conn, runID)
		testsupport.Must(t, err, "reading grants: %v", err)
		fp := shortFingerprint(grants[0].Fingerprint)

		granted := eventDetailsOfKind(t, conn, runID, EventGateOverrideGranted)
		if len(granted) != 1 || !strings.Contains(granted[0], "fp="+fp) {
			t.Errorf("%s data = %v, want one naming fp=%s",
				EventGateOverrideGranted, granted, fp)
		}
		spent := eventDetailsOfKind(t, conn, runID, EventStepBatchOverridden)
		if len(spent) != 1 || !strings.Contains(spent[0], "fp="+fp) {
			t.Errorf("%s data = %v, want one naming fp=%s",
				EventStepBatchOverridden, spent, fp)
		}
	})

	t.Run("a silent gate still carries a signature", func(t *testing.T) {
		// An `unmatched` row's process never ran, so its capture is empty by
		// nature — and v24 built the NULL-exit path precisely so such a park
		// could be batch-ruled. Recording a BLANK fingerprint for it would
		// mint a grant that can never be spent, a silent no-op worse than a
		// refusal. The empty capture is hashed instead, so empty means
		// pre-v30 and nothing else.
		silent := GateFingerprint("")
		if silent == "" {
			t.Fatal("a gate that printed nothing recorded no fingerprint; " +
				"its grant could never match")
		}
		row := db.GateResultRow{
			Gate: "build", Verdict: db.GateVerdictUnmatched,
			Fingerprint: silent,
		}
		grant := db.GateOverrideGrant{Gate: "build", Fingerprint: silent}
		if !grantMatches(grant, row) {
			t.Error("a grant minted on a silent failing gate did not cover " +
				"an identical later one; DKT-546's cover must survive")
		}
	})

	t.Run("a grant with no recorded fingerprint covers nothing", func(t *testing.T) {
		// A grant minted before v30 back-fills to the empty string. It vouches
		// for no content, so it must match NOTHING — a blank read as a
		// wildcard would preserve the gate-wide waiver for exactly the runs
		// that were in flight across the upgrade.
		row := db.GateResultRow{
			Gate: "build", Exit: intPtr(1), Fingerprint: "abc123",
		}
		legacy := db.GateOverrideGrant{Gate: "build", Exit: intPtr(1)}
		if grantMatches(legacy, row) {
			t.Error("a fingerprint-less grant covered a failing row")
		}
		blankRow := db.GateResultRow{Gate: "build", Exit: intPtr(1)}
		if grantMatches(legacy, blankRow) {
			t.Error("a fingerprint-less grant covered a fingerprint-less row; " +
				"two blanks are not a matching signature")
		}
	})
}

func intPtr(v int) *int { return &v }
