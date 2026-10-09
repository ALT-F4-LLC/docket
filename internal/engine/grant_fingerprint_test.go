package engine

import (
	"database/sql"
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

// parkOnPreV30Row parks implement@0 through a failing gate, then blanks the
// row's fingerprint the way migrateV29ToV30 leaves a row recorded before the
// upgrade: a step that was already parked when the operator upgraded.
func parkOnPreV30Row(t *testing.T) (conn *sql.DB, e *Engine, runID, stepID int) {
	t.Helper()
	conn = mustDB(t)
	runID = activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")
	e = testEngine()
	e.Gates = &exitGates{fail: true, exit: 1}
	stepID = stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, stepID)

	res, err := conn.Exec(
		`UPDATE gate_results SET fingerprint = '' WHERE step_id = ?`, stepID)
	testsupport.Must(t, err, "blanking the fingerprint: %v", err)
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("premise: the park recorded no gate row to blank")
	}
	return conn, e, runID, stepID
}

// TestBatchOverrideRefusesAnEmptyFingerprintRow: a --batch ruling on a row
// recorded before v30 would mint a grant grantMatches can never spend, a
// standing ruling in the ledger that covers nothing. The mint refuses instead,
// naming the cause and the resolve that still works, and records nothing.
func TestBatchOverrideRefusesAnEmptyFingerprintRow(t *testing.T) {
	conn, e, runID, implementID := parkOnPreV30Row(t)

	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact, not a code defect", nowMS+1)
	if err == nil {
		t.Fatal("--batch on a pre-v30 gate row was accepted")
	}
	assertCode(t, err, CodeValidation)
	msg := err.Error()
	for _, want := range []string{"build", "before the v30 fingerprint migration", "without --batch"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "no failed completion gate") {
		t.Errorf("refusal = %q reads as the no-failed-gate refusal; the "+
			"operator must learn the row predates fingerprints", msg)
	}

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 0 {
		t.Errorf("grants = %d, want 0 — a refused batch mints nothing", len(grants))
	}
	if got := eventKindCount(t, conn, runID, EventGateOverrideGranted); got != 0 {
		t.Errorf("%s events = %d, want 0", EventGateOverrideGranted, got)
	}
	if got := eventKindCount(t, conn, runID, EventStepResolved); got != 0 {
		t.Errorf("%s events = %d, want 0 — a refused batch resolves nothing",
			EventStepResolved, got)
	}
	step, err := db.GetStep(conn, implementID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Errorf("implement@0 = %q, want it still %q", step.Status, db.StepWaitingHuman)
	}
}

// TestBatchOverrideMintsOffFingerprintedRows: the refusal is keyed on the
// empty fingerprint, not on --batch. A park whose failing row carries one
// mints its grant and resolves exactly as before.
func TestBatchOverrideMintsOffFingerprintedRows(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")
	e := testEngine()
	e.Gates = &exitGates{fail: true, exit: 1}
	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact, not a code defect", nowMS+1)
	testsupport.Must(t, err, "resolve --as override-pass --batch: %v", err)

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 1 || grants[0].Fingerprint == "" {
		t.Errorf("grants = %+v, want one carrying a fingerprint", grants)
	}
	if got := eventKindCount(t, conn, runID, EventStepResolved); got != 1 {
		t.Errorf("%s events = %d, want 1", EventStepResolved, got)
	}
}

// TestSingleOverrideResolvesAPreV30Row: the refusal is --batch's alone. A
// single override-pass mints no grant, so a pre-v30 row costs it nothing, and
// it is the route the batch refusal points the operator to.
func TestSingleOverrideResolvesAPreV30Row(t *testing.T) {
	conn, e, runID, implementID := parkOnPreV30Row(t)

	err := e.ResolveStep(conn, implementID, ResolveOverridePass,
		"sandbox artifact, not a code defect", nowMS+1)
	testsupport.Must(t, err, "resolve --as override-pass: %v", err)

	if got := eventKindCount(t, conn, runID, EventStepResolved); got != 1 {
		t.Errorf("%s events = %d, want 1", EventStepResolved, got)
	}
	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 0 {
		t.Errorf("grants = %d, want 0 — a single resolve mints no grant", len(grants))
	}
}

func intPtr(v int) *int { return &v }
