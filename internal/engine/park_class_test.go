package engine

import (
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
// The REJECTION CARRIES AN OPERATOR NOTE, and that is deliberate: human.go
// composes the park's reason as `<note>; <bound reason>`, so the recorded text
// is the operator's words followed by the engine's. A classifier that read the
// reason would have to find the bound's phrasing inside a sentence an operator
// controls; this one reads LoopOutcome.Entered and never sees the string.
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
	// The operator note below is prepended to the bound's own sentence, so a
	// classifier keyed on the reason text reads an operator-controlled string.
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

		// The reason really does lead with the operator's words: a test that
		// passed by matching the engine's phrasing would have to reach past
		// them, and this pins that the two are not the same string.
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

// TestGateParkClassesComeFromTheGateRows pins the three gate classes at the
// reduction that separates them.
//
// A gate that RAN and failed, a gate whose trust entry did not match, and a
// gate skipped because the tree could not be bound all route the same way —
// `on_fail` — and all reduce to VerdictFail. What tells them apart is the
// recorded row's own verdict word, which is why verdictOverRows reports the
// skipped names and the unmatched flag beside the verdict rather than leaving
// the caller to re-reduce the same table.
//
// This is the seam the saga's switch reads. The end-to-end park is covered by
// TestParkClassIsDerivedFromEngineFacts; what needs pinning here is that the
// three inputs produce three distinguishable answers over identical routing.
func TestGateParkClassesComeFromTheGateRows(t *testing.T) {
	exit := 1
	cases := []struct {
		name          string
		rows          []db.GateResultRow
		wantSkipped   bool
		wantUnmatched bool
		wantClass     db.ParkClass
	}{{
		name: string(db.ParkClassGateFailed),
		rows: []db.GateResultRow{
			{Gate: "tests", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: &exit},
		},
		wantClass: db.ParkClassGateFailed,
	}, {
		name: string(db.ParkClassGateUnmatched),
		rows: []db.GateResultRow{
			{Gate: "build", Ordinal: 0, Verdict: db.GateVerdictUnmatched},
		},
		wantUnmatched: true,
		wantClass:     db.ParkClassGateUnmatched,
	}, {
		name: string(db.ParkClassGateSkipped),
		rows: []db.GateResultRow{
			{Gate: "coverage", Ordinal: 0, Verdict: db.GateVerdictSkipped},
		},
		wantSkipped: true,
		wantClass:   db.ParkClassGateSkipped,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, unmeasured, unmatched := verdictOverRows(tc.rows)

			// All three are not-pass; the routing they produce is identical.
			if verdict != VerdictFail {
				t.Fatalf("verdict = %q, want %q — every case here is a not-pass",
					verdict, VerdictFail)
			}
			if got := len(unmeasured) > 0; got != tc.wantSkipped {
				t.Errorf("skipped-gates reported = %v, want %v", got, tc.wantSkipped)
			}
			if unmatched != tc.wantUnmatched {
				t.Errorf("unmatched = %v, want %v", unmatched, tc.wantUnmatched)
			}

			// The saga's own precedence over those facts: a gate that could not
			// RUN outranks one that ran and failed, because the operator's next
			// move differs — restore the entry or rebind the tree, versus read
			// the failure. Mirrors saga.go's switch, which decides `skipped`
			// before the fail case and `unmatched` inside it.
			var class db.ParkClass
			switch {
			case len(unmeasured) > 0:
				class = db.ParkClassGateSkipped
			case unmatched:
				class = db.ParkClassGateUnmatched
			default:
				class = db.ParkClassGateFailed
			}
			if class != tc.wantClass {
				t.Errorf("park class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}
