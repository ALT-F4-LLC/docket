package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-725: an operator's steering had no channel that reliably reached a NEW
// round's rendered packet. Observed on RUN-51/AGT-643: after a 3/3 vote
// rejection the operator authorized another fix round and recorded the
// judge-converged remedy as an issue COMMENT; the resulting fix@9 packet
// (STEP-2489) carried zero bytes of it. Comments are not a context source
// (§6.6's five-source rule), a mid-run description edit never renders either
// (`body_snapshot` froze at activation, §9 item 5), and the `fix-round` note
// itself died on the SUPERSEDED park's row — DKT-247's == RESOLUTION section
// is scoped to a step's own routing record, and the new round's rows had none.
//
// The fix: an authorized entry stamps its note onto the round's freshly
// instantiated pending rows as their entering routing record
// (stampEntryRouting), so the standing own-row rendering carries it into
// every packet of the round the operator paid for.

// TestFixRoundNoteReachesTheNewRoundPackets drives the dkt587 threshold
// fixture to its bound, authorizes one more round with a steering note, and
// asserts the note renders in the NEW round's packets — the fix body's and
// the re-instantiated routing step's.
func TestFixRoundNoteReachesTheNewRoundPackets(t *testing.T) {
	conn := mustDB(t)
	_, _ = activateInterposed(t, conn, dkt587ThresholdSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	// Parked at the bound; the operator authorizes one more round WITH the
	// converged remedy as the note.
	if got := stepStatus(t, conn, "check@2"); got != db.StepWaitingHuman {
		t.Fatalf("check@2 = %q, want %q before the grant", got, db.StepWaitingHuman)
	}
	const remedy = "route the validator through the deny-by-default table; " +
		"the panel converged on closing the fail-open path structurally"
	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "check@2"),
		ResolveFixRound, remedy, nowMS)
	testsupport.Must(t, err, "resolving --as fix-round: %v", err)

	// The note is VERIFIED IN THE RENDERED PACKET, not inferred from rows:
	// reading the packet directly is how the original gap was confirmed.
	for _, instance := range []string{"fix@3", "check@3"} {
		result, err := RenderStep(
			conn, stepIDByInstance(t, conn, instance), "", nowMS)
		testsupport.Must(t, err, "rendering %s: %v", instance, err)
		if !strings.Contains(result.Packet, remedy) {
			t.Errorf("%s's packet does not carry the fix-round note:\n%s",
				instance, result.Packet)
		}
		if !strings.Contains(result.Packet, "== RESOLUTION fix-loop") {
			t.Errorf("%s's packet carries no == RESOLUTION fix-loop section:\n%s",
				instance, result.Packet)
		}
	}

	// The stamp changes routing ONLY — the rows stay pending and claimable,
	// exactly as a DKT-247 retry's ruled-on pending row does.
	if got := stepStatus(t, conn, "fix@3"); got != db.StepPending {
		t.Errorf("fix@3 = %q after the stamp, want %q", got, db.StepPending)
	}

	// And the round proceeds normally: the stamped body claims, completes,
	// and its OWN verdict overwrites the entering record at routing time.
	driveFixtureRound(t, 3)
	claimAndComplete(t, conn, e, "fix@3", "the third fix", "")
	raw := stepRoutingRaw(t, conn, "fix@3")
	if strings.Contains(raw, remedy) {
		t.Errorf("fix@3 routing = %q after completion; its own verdict must "+
			"replace the entering stamp", raw)
	}
}

// TestFixRoundWithoutNoteLeavesTheNewRoundUnchanged pins the other half: an
// authorization carrying no note stamps nothing, so a note-less fix-round's
// packets are byte-identical to what they always were.
func TestFixRoundWithoutNoteLeavesTheNewRoundUnchanged(t *testing.T) {
	conn := mustDB(t)
	_, _ = activateInterposed(t, conn, dkt587ThresholdSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "check@2"),
		ResolveFixRound, "", nowMS)
	testsupport.Must(t, err, "resolving --as fix-round without a note: %v", err)

	if raw := stepRoutingRaw(t, conn, "fix@3"); raw != "" {
		t.Errorf("fix@3 routing = %q after a note-less fix-round, want empty", raw)
	}
	result, err := RenderStep(conn, stepIDByInstance(t, conn, "fix@3"), "", nowMS)
	testsupport.Must(t, err, "rendering fix@3: %v", err)
	if strings.Contains(result.Packet, "== RESOLUTION") {
		t.Errorf("fix@3's packet grew a == RESOLUTION section with no note:\n%s",
			result.Packet)
	}
}
