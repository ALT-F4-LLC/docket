package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// park names a waiting-human step and the run it parked in, so every case
// below is asserted the same way on every surface.
type park struct {
	runID  int
	stepID int
}

// assertParkClass is the shared assertion: the class the routing transaction
// wrote reaches BOTH read surfaces that can carry it, with the same value.
//
// `step show` reads LoadStepView and `step list` reads RunStepList, so these
// two calls are the verbs themselves, not a stand-in for them. `docket next`
// is deliberately absent: NextSteps offers READY rows and stepRow stamps
// db.StepReady on every one (next.go, `NextSteps` and `stepRow`), so a
// waiting-human row never appears in an offer and there is nothing there to
// assert against.
func assertParkClass(t *testing.T, conn *sql.DB, p park, want db.ParkClass) {
	t.Helper()

	step, err := db.GetStep(conn, p.stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Fatalf("step %s is %q, want %q — the fixture must park for the class "+
			"to mean anything", step.Instance, step.Status, db.StepWaitingHuman)
	}

	view, err := LoadStepView(conn, p.stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.ParkClass != want {
		t.Errorf("`step show` park_class = %q, want %q", view.ParkClass, want)
	}

	entries, err := RunStepList(conn, p.runID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	found := false
	for _, e := range entries {
		if e.Step != model.FormatStepID(p.stepID) {
			continue
		}
		found = true
		if e.ParkClass != want {
			t.Errorf("`step list` park_class = %q, want %q", e.ParkClass, want)
		}
	}
	if !found {
		t.Errorf("step %s is absent from `step list --run`; a parked row is "+
			"inventory a conductor must still see", model.FormatStepID(p.stepID))
	}
}

// exhaustedSrc parks its only step by spending its single attempt.
const exhaustedSrc = `
[pipeline]
name = "park-class-exhausted"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"
`

// gapOnlySrc is the ordinary fixture's shape: one executor step whose
// gap-only completion parks before any gate verdict is consulted.
const gapOnlySrc = `
[pipeline]
name = "park-class-gap-only"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"
`

// voteRejectedSrc parks a vote gate on a rejected tally.
const voteRejectedSrc = `
[pipeline]
name = "park-class-vote"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"
`

// loopBoundSrc rejects through a human gate whose `on_fail` is `fix-loop` at a
// bound of zero, so the entry is refused and the gate parks instead.
//
// The REJECTION CARRIES AN OPERATOR NOTE, and that is deliberate: the note
// lands in the routing record as `waiting-human: <note>; <bound reason>`, so a
// classifier that read the routing string would have to find the bound's
// phrasing inside a sentence an operator controls; this one reads
// LoopOutcome.Entered and never sees the string. The park's own reason is the
// engine's, apart from the note.
const loopBoundSrc = `
[pipeline]
name = "park-class-loop-bound"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"

[[step]]
name = "gate"
after = ["work"]
type = "human"
on_fail = "fix-loop"
max_fix_loops = 1

[[step]]
name = "fix"
executor = "fixer"
emits = "out"
loop = true
after_loop = "gate"
`

// gatedSrc is one gated executor step that parks on its gate's verdict, so the
// three gate classes can be driven end to end through the real routing stage.
const gatedSrc = `
[pipeline]
name = "park-class-gated"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"
gates = ["build"]
on_fail = "waiting-human"
`

// thresholdSrc parks on its own declared threshold rather than on a gate, so
// `threshold-routed` is produced by the routing it names.
const thresholdSrc = `
[pipeline]
name = "park-class-threshold"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "findings"
payload = "findings@1"
threshold = { "waiting-human" = "any(severity >= blocker)" }
`

// verdictGates returns one fixed verdict for every gate, so a test can name the
// recorded row's verdict word — which is the fact the classifier reads.
type verdictGates struct{ verdict string }

func (g verdictGates) Run(
	_ context.Context, spec GateSpec, _ StepContext,
) (GateResult, error) {
	exit := 0
	if g.verdict != VerdictPass {
		exit = 1
	}
	return GateResult{Gate: spec.Name, Exit: exit, Verdict: g.verdict}, nil
}

// activateSrc registers one TOML fixture and activates a run over it.
func activateSrc(t *testing.T, conn *sql.DB, src, path string) int {
	t.Helper()
	registerSource(t, conn, []byte(src), path)
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// TestParkClassIsDerivedFromEngineFacts is DKT-1900.
//
// A waiting-human row said WHY it parked only in prose, so a conductor deciding
// what to do with it — route it itself, or seat a panel — had to read the
// engine's sentence and guess. Each case below drives one real routing decision
// to its park and asserts the enum value on the surfaces that carry it.
//
// Every value is assigned INSIDE the routing transaction from that decision's
// own facts: a gate row's verdict, a tally, LoopOutcome.Entered. No case
// asserts anything about the reason text, and none can pass by reading it —
// which is the property the loop-bound case below is built to pin.
func TestParkClassIsDerivedFromEngineFacts(t *testing.T) {
	t.Run(string(db.ParkClassAttemptsExhausted), func(t *testing.T) {
		conn := mustDB(t)
		runID := activateSrc(t, conn, exhaustedSrc, "exhausted.toml")
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "work@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		testsupport.Must(t,
			e.FailStep(conn, stepID, claim.Token, "gave up", "", nowMS), "fail: %v", err)

		assertParkClass(t, conn, park{runID, stepID}, db.ParkClassAttemptsExhausted)
	})

	// The three gate classes, end to end through the real routing stage. All
	// three route identically — per `on_fail` — and reduce to a not-pass
	// verdict; the recorded row's verdict WORD is what tells them apart.
	for _, tc := range []struct {
		verdict string
		want    db.ParkClass
	}{
		{VerdictFail, db.ParkClassGateFailed},
		{VerdictUnmatched, db.ParkClassGateUnmatched},
		{VerdictSkipped, db.ParkClassGateSkipped},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			conn := mustDB(t)
			runID := activateSrc(t, conn, gatedSrc, "gated-"+tc.verdict+".toml")
			e := testEngine()
			e.Gates = verdictGates{verdict: tc.verdict}

			stepID := stepIDByInstance(t, conn, "work@0")
			claim, err := ClaimStep(conn, stepID,
				ClaimOptions{Owner: "w", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)
			testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
				Token: claim.Token, Artifact: []byte("the work"), NowMS: nowMS,
			}), "complete: %v", nil)

			assertParkClass(t, conn, park{runID, stepID}, tc.want)
		})
	}

	t.Run(string(db.ParkClassThresholdRouted), func(t *testing.T) {
		conn := mustDB(t)
		registerFixtureSchema(t, conn)
		runID := activateSrc(t, conn, thresholdSrc, "threshold.toml")
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "work@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
			Token:    claim.Token,
			Artifact: []byte("the findings"),
			Payload:  []byte(`[{"severity":"blocker"}]`),
			NowMS:    nowMS,
		}), "complete: %v", nil)

		assertParkClass(t, conn, park{runID, stepID}, db.ParkClassThresholdRouted)
	})

	// A rejected hold parks the ROUTING step, never the materialized one: the
	// held step ends `done` on both answers (H14) because it recorded a
	// decision, and the consequence lands on the step whose routing was
	// deferred. The class is read from that decision, not from the note.
	t.Run(string(db.ParkClassHeldRejected), func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)
		e := testEngine()

		driveToReconcile(t, conn, e, clusteredPayload)

		heldID := stepIDByInstance(t, conn, "reconcile-held@0#0")
		testsupport.Must(t,
			e.DecideStepValue(conn, heldID, false, "not a real cluster", "", nowMS),
			"rejecting the held cluster: %v", nil)

		assertParkClass(t, conn,
			park{run.ID, stepIDByInstance(t, conn, "reconcile@0")},
			db.ParkClassHeldRejected)
	})

	t.Run(string(db.ParkClassGapOnly), func(t *testing.T) {
		conn := mustDB(t)
		runID := activateSrc(t, conn, gapOnlySrc, "gap-only.toml")
		e := testEngine()

		stepID := stepIDByInstance(t, conn, "work@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		var gapIssues []string
		err = e.CompleteStep(conn, stepID, CompleteOptions{
			Token:    claim.Token,
			Artifact: []byte("   \n"),
			Gaps: [][]byte{[]byte(
				"# The fix belongs in another repository\n\nNothing here to change.")},
			GapIssues: &gapIssues,
			NowMS:     nowMS,
		})
		testsupport.Must(t, err, "complete gap-only: %v", err)

		assertParkClass(t, conn, park{runID, stepID}, db.ParkClassGapOnly)
	})

	t.Run(string(db.ParkClassVoteRejected), func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		runID := activateSrc(t, conn, voteRejectedSrc, "vote.toml")
		e := testEngine()

		proposalID := openGateProposal(t, conn, e, runID)
		for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
			castSeat(t, conn, proposalID, seat, model.VerdictReject, "no")
		}
		testsupport.Must(t, e.DriveRunLifecycles(conn, runID, nowMS),
			"driving the tally: %v", nil)

		assertParkClass(t, conn,
			park{runID, stepIDByInstance(t, conn, "gate@0")}, db.ParkClassVoteRejected)
	})

	// The mutant clause: the class comes from the ORDINAL against the pinned
	// `max_fix_loops`, via LoopOutcome.Entered, not from the reason's wording.
	// The operator note below lands in the routing record ahead of the bound's
	// own sentence, so a classifier keyed on that text reads an
	// operator-controlled string.
	t.Run(string(db.ParkClassLoopBound), func(t *testing.T) {
		conn := mustDB(t)
		runID := activateSrc(t, conn, loopBoundSrc, "loop-bound.toml")
		e := testEngine()

		claimAndComplete(t, conn, e, "work@0", "the work", "")

		// Round 1 is within the bound: this rejection ENTERS the loop, mints
		// `fix@1` and re-instantiates the gate, and parks nothing.
		testsupport.Must(t,
			e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false,
				"first pass needs work", nowMS),
			"first rejection: %v", nil)
		claimAndComplete(t, conn, e, "fix@1", "the fix", "")

		// Round 2 would exceed `max_fix_loops = 1`, so the entry is refused and
		// the gate parks on the bound instead.
		const note = "rejected: the approach is wrong, not the execution"
		gateID := stepIDByInstance(t, conn, "gate@1")
		testsupport.Must(t, e.DecideStep(conn, gateID, false, note, nowMS),
			"second rejection: %v", nil)

		assertParkClass(t, conn, park{runID, gateID}, db.ParkClassLoopBound)

		// The reason is the engine's sentence about the bound, not the bare
		// class word: this pins that the two are not the same string.
		view, err := LoadStepView(conn, gateID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if view.ParkReason == "" {
			t.Fatal("the park recorded no reason; the class must not be the only record")
		}
		if view.ParkReason == string(view.ParkClass) {
			t.Errorf("park_reason = %q is just the class; the two are separate "+
				"facts and the reason must carry the operator's own words",
				view.ParkReason)
		}
	})
}

// TestUnmatchedGateIsReportedApartFromAFailure pins the fact the gate classes
// are derived from, at the reduction that produces it.
//
// `unmatched` is not-pass, so it already folded into VerdictFail and routed
// identically — and that identity is exactly why a caller could not tell "the
// gate ran and failed" from "the gate could not be invoked". verdictOverRows
// reports the distinction beside the verdict so the classifier reads one
// reduction rather than re-reducing the same table, which is how a report comes
// to contradict the routing beside it (DKT-982).
func TestUnmatchedGateIsReportedApartFromAFailure(t *testing.T) {
	exit := 1
	cases := []struct {
		name          string
		rows          []db.GateResultRow
		wantUnmatched bool
	}{{
		name: "a gate that ran and failed",
		rows: []db.GateResultRow{
			{Gate: "tests", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: &exit},
		},
	}, {
		name: "a gate whose entry did not match",
		rows: []db.GateResultRow{
			{Gate: "build", Ordinal: 0, Verdict: db.GateVerdictUnmatched},
		},
		wantUnmatched: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, _, unmatched := verdictOverRows(tc.rows)

			// The routing they produce is identical; only the flag differs.
			if verdict != VerdictFail {
				t.Fatalf("verdict = %q, want %q — both cases are a not-pass",
					verdict, VerdictFail)
			}
			if unmatched != tc.wantUnmatched {
				t.Errorf("unmatched = %v, want %v — a gate that never ran must "+
					"not reach the operator wearing a failed gate's word",
					unmatched, tc.wantUnmatched)
			}
		})
	}
}
