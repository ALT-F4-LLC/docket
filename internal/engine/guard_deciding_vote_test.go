package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// openProposal creates a proposal that will stay open: the tally is not what
// these tests are about.
func openProposal(t *testing.T, conn *sql.DB, description string) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description:    description,
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	return id
}

// TestDecidingVoteAdmitsThePanelThatDecidesTheHold is DKT-236.
//
// Unacknowledged reaps hold headroom; the sanctioned way to decide whether to
// acknowledge them is a judge panel; spawning that panel was denied BY the
// hold — the exact state the panel exists to decide. Nothing could move, so
// what actually happened was a ~10h operator round trip followed by two
// self-passed `--ack-reap` calls with no panel at all: authorization creep
// arriving through the gate's own deadlock.
func TestDecidingVoteAdmitsThePanelThatDecidesTheHold(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, _ := reapOneWriter(t, conn, runID)

	// The deadlock, reproduced: the spawn is denied.
	denied, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: at})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if denied.Allowed {
		t.Fatal("premise: an unacknowledged reap must deny the spawn")
	}
	// And the denial NAMES the way out, so an operator reads the next command
	// out of the refusal rather than out of a document.
	if !strings.Contains(denied.Reason, "--deciding-vote") {
		t.Errorf("the denial does not name the carve-out: %q", denied.Reason)
	}

	// The conductor opens the question the panel will decide, and the panel's
	// own spawn is admitted.
	proposalID := openProposal(t, conn, "acknowledge the reaped judge lease?")
	allowed, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		DecidingVote: proposalID, NowMS: at,
	})
	testsupport.Must(t, err, "GuardSpawn --deciding-vote: %v", err)
	if !allowed.Allowed {
		t.Fatalf("the panel that exists to decide the hold was denied by it: %s",
			allowed.Reason)
	}
	// The verdict SAYS it used the carve-out, so a spawn let past a hold is
	// legible in the relay's own logs.
	if !strings.Contains(allowed.Reason, model.FormatProposalID(proposalID)) {
		t.Errorf("the allow does not name the proposal it was granted for: %q",
			allowed.Reason)
	}

	// The reap is STILL unacknowledged: the carve-out admits a spawn, it does
	// not decide the question. Acknowledging is still the panel's to do.
	if acks := openReapsOf(t, conn, runID); len(acks) == 0 {
		t.Error("the carve-out acknowledged the reap; it admits the panel " +
			"that will decide, and deciding is not its job")
	}

	// And it is EVENT-LOGGED. A spawn admitted over a hold must not read like
	// a spawn nothing was holding.
	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventSpawnAdmitted)
	if !ok {
		t.Fatalf("no %s event; a hold stepped past with no record is "+
			"indistinguishable from no hold at all", EventSpawnAdmitted)
	}
	for _, want := range []string{"deciding-vote", model.FormatProposalID(proposalID)} {
		if !strings.Contains(string(event.Data), want) {
			t.Errorf("the audit event does not carry %q: %s", want, event.Data)
		}
	}
}

// TestDecidingVoteIsNarrow pins every clause that keeps the carve-out from
// becoming a general bypass.
func TestDecidingVoteIsNarrow(t *testing.T) {
	t.Run("a proposal that does not exist", func(t *testing.T) {
		conn := mustDB(t)
		runID := serializedRun(t, conn)
		at, _ := reapOneWriter(t, conn, runID)

		_, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
			DecidingVote: 9999, NowMS: at,
		})
		if err == nil {
			t.Fatal("an id nobody created admitted the spawn; that is a bypass " +
				"with a plausible-looking flag")
		}
		if code, _ := CodeOf(err); code != CodeNotFound {
			t.Errorf("code = %v, want NOT_FOUND", code)
		}
	})

	t.Run("a proposal that is already decided", func(t *testing.T) {
		conn := mustDB(t)
		runID := serializedRun(t, conn)
		at, _ := reapOneWriter(t, conn, runID)

		id := openProposal(t, conn, "already settled")
		execSQL(t, conn, `UPDATE proposals SET status = ? WHERE id = ?`,
			string(model.ProposalStatusApproved), id)

		_, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
			DecidingVote: id, NowMS: at,
		})
		if err == nil {
			t.Fatal("a decided proposal admitted the spawn; it would then " +
				"authorize any spawn forever")
		}
		if code, _ := CodeOf(err); code != CodeConflict {
			t.Errorf("code = %v, want CONFLICT", code)
		}
	})

	t.Run("it does not relax the row comparison", func(t *testing.T) {
		conn := mustDB(t)
		runID := dispatchRun(t, conn)
		id := openProposal(t, conn, "unrelated")

		// Rows proposed against no open dispatch is G8's denial — a fact about
		// a relay spawning a batch the engine never issued, which no vote
		// makes acceptable.
		verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
			Rows:         []byte(`[]`),
			DecidingVote: id,
			NowMS:        nowMS,
		})
		testsupport.Must(t, err, "GuardSpawn: %v", err)
		if verdict.Allowed {
			t.Error("the carve-out relaxed the ROW half; row drift is a " +
				"different fact about a different actor")
		}
	})
}

// TestNoDecidingVoteWritesNoAuditEvent keeps the audit trail meaningful: an
// ordinary allowed spawn must not emit the carve-out event, or the event stops
// distinguishing anything.
func TestNoDecidingVoteWritesNoAuditEvent(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: nowMS})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if !verdict.Allowed {
		t.Fatalf("premise: a run with no hold must be allowed: %s", verdict.Reason)
	}

	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	for _, e := range page.Events {
		if e.Kind == EventSpawnAdmitted {
			t.Error("an ordinary allow emitted the carve-out event; the event " +
				"exists to mark a hold that was stepped past")
		}
	}
}
