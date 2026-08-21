package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestVerifyReportsEveryRowNotJustTheFirst is DKT-243.
//
// The comparison stopped at the FIRST shifted row and hard-errored, so a
// dispatch where several steps had moved mid-flight reported one of them and
// hid the rest — ~16 occurrences across three sessions, each costing a manual
// per-step confirm round before a `close` that reconciles the same state
// without complaint. The verb still FAILS when anything is off; what changed
// is that one invocation now says everything it knows.
func TestVerifyReportsEveryRowNotJustTheFirst(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	// Two issues with disjoint scopes, so both lead rows are ready and the
	// manifest carries more than one thing that can move.
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, a, `["internal/a/**"]`), "scope A")
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, b, `["internal/b/**"]`), "scope B")
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	ready := readyOffered(manifest.Rows)
	if len(ready) != 2 {
		t.Fatalf("premise: the fixture must lead with 2 ready rows, got %+v",
			manifest.Rows)
	}

	// BOTH lead rows move: each is claimed, so neither is offerable any more.
	for _, row := range ready {
		id, err := model.ParseStepID(row.Step)
		testsupport.Must(t, err, "parsing %q: %v", row.Step, err)
		_, err = ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claim %s: %v", row.Step, err)
	}

	result, mismatch, err := NewEngine().VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch == nil {
		t.Fatal("a manifest whose ready rows were both claimed verified as equal")
	}
	if result.Verified {
		t.Error("verified = true alongside a mismatch")
	}

	// The refusal still names the FIRST offending row — the message an
	// operator has read for two releases is unchanged.
	if mismatch.Position != 0 {
		t.Errorf("mismatch at position %d, want the first differing row", mismatch.Position)
	}

	// And every stored row now has a verdict, not just the ones up to the
	// first problem.
	if len(result.Rows) != len(manifest.Rows) {
		t.Fatalf("reported %d row verdicts for a %d-row manifest — a sweep "+
			"that stops early is the whole of DKT-243",
			len(result.Rows), len(manifest.Rows))
	}
	for i, r := range result.Rows {
		if r.Position != i {
			t.Errorf("verdict %d reports position %d; positions index the "+
				"STORED manifest in order", i, r.Position)
		}
		if r.Verdict == "" {
			t.Errorf("row %d (%s) has no verdict", i, r.Instance)
		}
		if r.Step == "" || r.Instance == "" {
			t.Errorf("row %d names no step/instance: %+v", i, r)
		}
	}

	// Both claimed rows are reported, not one: that is the count that used to
	// require a second invocation per row.
	var offending int
	for _, r := range result.Rows {
		if r.Verdict == RowMissing || r.Verdict == RowShifted {
			offending++
			if r.Stored == "" {
				t.Errorf("%s reports %s with no stored bytes; the evidence is "+
					"the point", r.Instance, r.Verdict)
			}
		}
	}
	if offending < 2 {
		t.Errorf("reported %d offending rows, want both claimed ones — "+
			"reporting one and hiding the rest is what cost the per-step "+
			"confirm rounds", offending)
	}
}

// TestVerifyEqualStillReportsEveryRowMatched keeps the happy path honest: a
// verified manifest says so per row too, which is what makes the tally on the
// success line meaningful rather than decorative.
func TestVerifyEqualStillReportsEveryRowMatched(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil || !result.Verified {
		t.Fatalf("an untouched manifest failed verify: %+v", mismatch)
	}
	if len(result.Rows) != len(manifest.Rows) {
		t.Fatalf("verified manifest reports %d verdicts for %d rows",
			len(result.Rows), len(manifest.Rows))
	}
	for _, r := range result.Rows {
		if r.Verdict != RowMatched {
			t.Errorf("%s verdict = %q on a verified manifest", r.Instance, r.Verdict)
		}
		if r.Stored != "" || r.Computed != "" {
			t.Errorf("%s carries bytes on a matched row; there is nothing to "+
				"show and a manifest is mostly matched rows", r.Instance)
		}
	}
}

// TestVerifyMarksRecordedRowsAsRecorded keeps DKT-10/DKT-65's skip visible
// rather than silent: a row whose step recorded is fine, and saying so is what
// stops an operator from re-checking it by hand.
func TestVerifyMarksRecordedRowsAsRecorded(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	first := readyOffered(manifest.Rows)[0]
	id, err := model.ParseStepID(first.Step)
	testsupport.Must(t, err, "parsing %q: %v", first.Step, err)
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, testEngine().CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	}), "complete: %v", err)

	result, _, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)

	var found bool
	for _, r := range result.Rows {
		if r.Step == first.Step {
			found = true
			if r.Verdict != RowRecorded {
				t.Errorf("a recorded step's row reports %q, want %q — its "+
					"absence from the recomputation is the dispatch working",
					r.Verdict, RowRecorded)
			}
		}
	}
	if !found {
		t.Errorf("the recorded step's row is missing from the verdicts entirely")
	}
}
