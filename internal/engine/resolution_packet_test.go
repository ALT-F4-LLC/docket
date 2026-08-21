package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// parkingWorkflow parks its one step at waiting-human on the first failure,
// which is the state `step resolve` applies to — the state an operator ruling
// is issued from.
const parkingWorkflow = `
[pipeline]
name = "parks"
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
`

// TestResolutionNoteReachesTheRetryPacket is DKT-247.
//
// A resolve note was audit-trail only. The packet rendered header, frozen
// body, inputs, pins, and output spec, and nothing else — so an operator's
// ruling issued BETWEEN rounds, the answer to the very question that parked
// the step, could not reach the retry it authorized. A conductor applied
// rulings as its own repo commits instead (agw: 3df53c4, b9182fa — both
// worked, neither sanctioned).
func TestResolutionNoteReachesTheRetryPacket(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, aerr := activate(conn, run.ID)
	testsupport.Must(t, aerr, "activate: %v", aerr)
	e := testEngine()
	stepID := stepIDByInstance(t, conn, "flaky@0")

	// First round: no ruling has been issued, so the packet must be exactly
	// what it always was.
	before, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if strings.Contains(before.Packet, "== RESOLUTION") {
		t.Fatalf("a first-round packet grew a resolution section:\n%s", before.Packet)
	}

	// The step fails and parks, and the operator rules on it with a note.
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	ferr := e.FailStep(conn, stepID, claim.Token, "the build did not compile", "", nowMS)
	testsupport.Must(t, ferr, "fail: %v", ferr)

	const ruling = "use the v2 API; the v1 shim was removed in 4a1c2f: see the note"
	rerr := e.ResolveStep(conn, stepID, ResolveRetry, ruling, nowMS)
	testsupport.Must(t, rerr, "resolve --as retry: %v", rerr)

	// The retry re-renders the SAME instance, and the ruling is in the packet.
	after, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep after resolve: %v", err)
	if !strings.Contains(after.Packet, "== RESOLUTION") {
		t.Fatalf("the retry packet carries no resolution section:\n%s", after.Packet)
	}
	if !strings.Contains(after.Packet, ruling) {
		t.Errorf("the retry packet does not carry the ruling %q:\n%s",
			ruling, after.Packet)
	}
	// The routing rides beside the note: "use the v2 API" reads very
	// differently under a retry than under an override.
	if !strings.Contains(after.Packet, ResolveRetry) {
		t.Errorf("the resolution section does not name its routing:\n%s", after.Packet)
	}

	// A note containing ": " survives whole — the routing half is a closed set
	// of hyphenated words, so splitting on the FIRST occurrence is exact.
	if strings.Contains(after.Packet, "see the note") == false {
		t.Errorf("the note was truncated at an internal colon:\n%s", after.Packet)
	}
}

// TestResolutionIsScopedToItsOwnStep is the other half of DKT-247's ask: a
// ruling on one step must not appear in another's packet. Instance labels
// repeat across issues in a run, so a bundle that reached for "the run's
// notes" would put one instance's ruling in another's hands.
func TestResolutionIsScopedToItsOwnStep(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, a, `["internal/a/**"]`), "scope A")
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, b, `["internal/b/**"]`), "scope B")
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	// Both issues carry a `flaky@0`; only issue A's is ruled on.
	var ruledID, otherID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = 'flaky@0'`,
		run.ID, a).Scan(&ruledID)
	testsupport.Must(t, err, "finding A's step: %v", err)
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = 'flaky@0'`,
		run.ID, b).Scan(&otherID)
	testsupport.Must(t, err, "finding B's step: %v", err)

	claim, err := ClaimStep(conn, ruledID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	ferr := e.FailStep(conn, ruledID, claim.Token, "boom", "", nowMS)
	testsupport.Must(t, ferr, "fail: %v", ferr)

	const ruling = "A-ONLY-RULING"
	rerr := e.ResolveStep(conn, ruledID, ResolveRetry, ruling, nowMS)
	testsupport.Must(t, rerr, "resolve: %v", rerr)

	ruled, err := RenderStep(conn, ruledID, "", nowMS)
	testsupport.Must(t, err, "RenderStep(ruled): %v", err)
	if !strings.Contains(ruled.Packet, ruling) {
		t.Fatalf("the ruled step's own packet lost the ruling:\n%s", ruled.Packet)
	}

	other, err := RenderStep(conn, otherID, "", nowMS)
	testsupport.Must(t, err, "RenderStep(other): %v", err)
	if strings.Contains(other.Packet, ruling) {
		t.Errorf("another issue's identically-named step received the "+
			"ruling; instance labels repeat across issues, so a bundle must "+
			"read its OWN row:\n%s", other.Packet)
	}
	if strings.Contains(other.Packet, "== RESOLUTION") {
		t.Errorf("an unruled step grew a resolution section:\n%s", other.Packet)
	}
}

// TestResolutionOfSplitsTheStoredRecord pins the parse itself, including the
// two shapes routingRecord can produce.
func TestResolutionOfSplitsTheStoredRecord(t *testing.T) {
	if got := resolutionOf(&db.Step{}); got != nil {
		t.Errorf("a step with no routing record reports %+v, want nil", got)
	}
	if got := resolutionOf(nil); got != nil {
		t.Errorf("a nil step reports %+v, want nil", got)
	}

	// routingRecord writes the routing alone when there is no note.
	bare := resolutionOf(&db.Step{Routing: "fix-loop"})
	if bare == nil || bare.Routing != "fix-loop" || bare.Note != "" {
		t.Errorf("a note-less record parsed to %+v, want routing fix-loop and "+
			"no note — the routing alone still says why the step is being "+
			"asked again", bare)
	}

	// And `<routing>: <note>` when there is one, with the note kept whole.
	withNote := resolutionOf(&db.Step{Routing: "retry: rerun it: the flake is real"})
	if withNote == nil || withNote.Routing != "retry" {
		t.Fatalf("parsed to %+v, want routing retry", withNote)
	}
	if withNote.Note != "rerun it: the flake is real" {
		t.Errorf("note = %q, want the whole note including its own colon",
			withNote.Note)
	}
}
