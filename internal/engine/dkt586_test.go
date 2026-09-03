package engine

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-586 — RUN-30's spurious resume, pinned end to end.
//
// RUN-30's trail: seq 3053 an operator `run pause` (active -> waiting-human);
// seq 3057 a `run-resumed` with EMPTY data, one millisecond after synthesize@1
// recorded (seq 3055/3056), with no operator verb in between; seq 3083 a second
// operator pause whose reason complains the run "read back as active even after
// the dispatch (DISPATCH-129) was reconciled and closed"; seq 3124 the real
// resume, carrying {from, to, reason}.
//
// The defect was reconcileRun's default branch: a run-level pause parks no
// step, so the rollup read `parked == 0` and flipped the run back to `active`
// the moment an in-flight step's record routed through it. That is DKT-305's
// defect exactly (RUN-31 was the other victim), fixed by the `pause_origin`
// guard, and DKT-304 additionally made every rollup-written lifecycle event
// self-identifying ({from, to, reason: "rollup"}) so an empty-data resume can
// no longer be produced by ANY path.
//
// This test drives RUN-30's full shape — both incidents in one run — against
// the current code as a regression pin:
//
//  1. step claimed under a dispatch, operator pauses, the step RECORDS: the
//     run must stay `waiting-human` and no `run-resumed` may appear (the
//     acceptance criterion's first clause).
//  2. the dispatch is then reconciled and closed (backfill + verify + close,
//     DISPATCH-129's pipeline): the run must STILL read `waiting-human` —
//     nothing in the dispatch pipeline touches the run row, so the pause
//     stands across the close.
//  3. the only way back to `active` is the resume transition — `run resume`
//     via MoveRun — and its event carries {from, to, reason} (the criterion's
//     second clause, and seq 3124's shape).
func TestRun30PauseSurvivesStepRecordAndDispatchClose(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	run, _ := activatedRun(t, conn)

	// The wave: a manifest is open and its step is claimed — the state RUN-30
	// was in when the operator typed the pause. Limit 1 so the manifest holds
	// only the claimed step: RUN-30's wave had recorded every manifest row
	// before the reconcile ran, and VerifyDispatch (correctly) reports a
	// mismatch for a manifest whose UNRECORDED rows are absent from a paused
	// run's ready set — R1 empties it — which is a different fact than the one
	// this test pins.
	openDispatch(t, conn, run.ID, 1, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement@0: %v", err)

	// seq 3053: the operator pauses. Run-level — no step parks.
	paused, _, err := MoveRun(conn, run.ID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator paused mid-wave", nowMS+1)
	testsupport.Must(t, err, "MoveRun pause: %v", err)
	if paused.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run is %s after the pause, want %s",
			paused.Status, model.RunWaitingHuman)
	}

	// seq 3055/3056: the in-flight step RECORDS, one tick later. Its routing
	// transaction runs reconcileRun with parked == 0 (no step is
	// waiting-human) and unfinished > 0 (the review fan-out is pending) — the
	// exact branch that resumed RUN-30 at seq 3057.
	err = e.CompleteStep(conn, implID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "recording the in-flight step: %v", err)

	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if after.Status != model.RunWaitingHuman {
		t.Fatalf("run is %s after a step recorded into the pause, want %s — "+
			"the step-record path wrote the run row back toward active (DKT-586)",
			after.Status, model.RunWaitingHuman)
	}
	if n := countRunEvents(t, conn, run.ID, EventRunResumed); n != 0 {
		t.Fatalf("%d run-resumed event(s) after a step recorded into the pause, "+
			"want 0 — seq 3057's spurious resume", n)
	}

	// DISPATCH-129's pipeline: backfill, verify, close — while the run is
	// paused, exactly as RUN-30's wave was reconciled. None of the three
	// stages may move the run row.
	_, err = e.ReconcileDispatch(conn, run.ID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "wave-journal:wf-30", "", false, "", nowMS+3)
	testsupport.Must(t, err, "reconciling the dispatch: %v", err)
	if status, _ := dispatchStatus(t, conn, run.ID); status != db.DispatchClosed {
		t.Fatalf("premise: dispatch is %s after the reconcile, want %s",
			status, db.DispatchClosed)
	}

	closed, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun after the close: %v", err)
	if closed.Status != model.RunWaitingHuman {
		t.Fatalf("run reads %s after the dispatch was reconciled and closed, "+
			"want %s — RUN-30's second pause complained of exactly this",
			closed.Status, model.RunWaitingHuman)
	}
	if n := countRunEvents(t, conn, run.ID, EventRunResumed); n != 0 {
		t.Fatalf("%d run-resumed event(s) after the dispatch closed, want 0", n)
	}

	// seq 3124: the legitimate resume, through the resume transition and
	// nothing else. It moves the run and its event carries {from, to, reason}.
	resumed, _, err := MoveRun(conn, run.ID, "resume", model.RunActive,
		[]model.RunStatus{model.RunWaitingHuman}, "operator carried on", nowMS+4)
	testsupport.Must(t, err, "MoveRun resume: %v", err)
	if resumed.Status != model.RunActive {
		t.Fatalf("run is %s after `run resume`, want %s", resumed.Status, model.RunActive)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var resumes []Event
	for _, ev := range page.Events {
		if ev.Kind == EventRunResumed {
			resumes = append(resumes, ev)
		}
	}
	if len(resumes) != 1 {
		t.Fatalf("%d run-resumed event(s) in the feed, want exactly 1 — the "+
			"operator's own", len(resumes))
	}
	var data lifecycleData
	testsupport.Must(t, json.Unmarshal(resumes[0].Data, &data),
		"decoding the resume's data: %v", err)
	if data.From != string(model.RunWaitingHuman) || data.To != string(model.RunActive) {
		t.Errorf("resume data = %s -> %s, want waiting-human -> active", data.From, data.To)
	}
	if data.Reason != "operator carried on" {
		t.Errorf("resume reason = %q, want the operator's — an empty or absent "+
			"reason is seq 3057's unattributable shape", data.Reason)
	}
}

// TestNoRunResumedEventEverCarriesEmptyData pins DKT-304's half of the fix
// from the ledger side: the ROLLUP's own resume — the legitimate, step-park
// kind — identifies itself. RUN-30's seq 3057 was diagnosable only because its
// data was empty; with every writer of `run-resumed` now naming {from, to,
// reason}, an empty-data resume cannot be produced by any path, and this test
// fails if one ever is.
func TestNoRunResumedEventEverCarriesEmptyData(t *testing.T) {
	const src = `
[pipeline]
name = "parks586"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"

[[step]]
name = "wrap"
after = ["flaky"]
executor = "w"
emits = "out2"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "parks586.toml")
	issue := createIssue(t, conn, "parked then resumed", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

	// Park the run at the STEP level, then resolve the park: the rollup's
	// automatic resume — the one `run-resumed` no operator types.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.FailStep(conn, id, claim.Token, "gave up", "", nowMS),
		"fail: %v", err)
	testsupport.Must(t, e.ResolveStep(conn, id, ResolveSkip, "not worth it", nowMS+1),
		"resolve: %v", err)

	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if after.Status != model.RunActive {
		t.Fatalf("premise: run is %s after its only park resolved, want %s "+
			"(the rollup's automatic resume)", after.Status, model.RunActive)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	seen := 0
	for _, ev := range page.Events {
		if ev.Kind != EventRunResumed {
			continue
		}
		seen++
		var data lifecycleData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("run-resumed data %q does not decode: %v", ev.Data, err)
		}
		if data.From == "" || data.To == "" || data.Reason == "" {
			t.Errorf("run-resumed carries empty data %s — RUN-30's seq 3057 "+
				"shape; every resume must name {from, to, reason}", ev.Data)
		}
		if data.Reason != runRollupReason {
			t.Errorf("the rollup's resume reason = %q, want %q — the value no "+
				"operator verb can produce", data.Reason, runRollupReason)
		}
	}
	if seen != 1 {
		t.Fatalf("%d run-resumed event(s), want exactly 1 (the rollup's)", seen)
	}
}
