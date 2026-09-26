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

// ---------------------------------------------------------------------------
// `--active --deciding-vote`: the proposal resolves its own run
// ---------------------------------------------------------------------------

// activeRunOver activates one run over the already-registered writeLimitedSrc
// and returns it with its issue, for the cases that need several runs.
func activeRunOver(t *testing.T, conn *sql.DB, title string) (runID, issueID int) {
	t.Helper()
	issueID = createIssue(t, conn, title, "body", "task", nil)
	run := startRun(t, conn, issueID)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate %s: %v", title, err)
	return run.ID, issueID
}

// reapWriterOf is reapOneWriter for ONE NAMED RUN: claimInstance resolves an
// instance to the lowest step id, so with several runs it always claims the
// oldest run's writer, and a hold has to land on a specific run here.
func reapWriterOf(t *testing.T, conn *sql.DB, runID int) (at int64) {
	t.Helper()
	var stepID int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND instance = 'one@0'`, runID).Scan(&stepID)
	testsupport.Must(t, err, "finding one@0 of %s: %v", model.FormatRunID(runID), err)
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim one@0 of %s: %v", model.FormatRunID(runID), err)
	at = claim.LeaseExpiresMS + 1
	_, err = NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "`next` past the lease: %v", err)
	if acks := openReapsOf(t, conn, runID); len(acks) != 1 {
		t.Fatalf("%d unacknowledged reaps on %s, want 1", len(acks), model.FormatRunID(runID))
	}
	return at
}

func linkProposal(t *testing.T, conn *sql.DB, proposalID, issueID int) {
	t.Helper()
	err := db.LinkProposalIssue(conn, proposalID, issueID)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)
}

// carveOutEvents counts a run's spawn-admitted audit events.
func carveOutEvents(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	n := 0
	for _, e := range page.Events {
		if e.Kind == EventSpawnAdmitted {
			n++
		}
	}
	return n
}

// TestActiveDecidingVoteAdmitsTheRunTheProposalServes: `--active
// --deciding-vote` resolves the run from the proposal's linked issue, so a hook
// launching a panel asks once — instead of asking `--active`, parsing the run
// off the denial, and asking again with `--run`.
func TestActiveDecidingVoteAdmitsTheRunTheProposalServes(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	held, heldIssue := activeRunOver(t, conn, "held")
	other, _ := activeRunOver(t, conn, "other")
	at := reapWriterOf(t, conn, held)

	// Premise: the plain question denies, naming the held run.
	denied, err := GuardSpawnActive(conn, 0, 0, at)
	testsupport.Must(t, err, "GuardSpawnActive: %v", err)
	if denied.Allowed || !strings.Contains(denied.Reason, model.FormatRunID(held)) {
		t.Fatalf("premise: the hold on %s must deny --active; allowed=%v reason=%q",
			model.FormatRunID(held), denied.Allowed, denied.Reason)
	}

	proposalID := openProposal(t, conn, "acknowledge the reaped judge lease?")
	linkProposal(t, conn, proposalID, heldIssue)

	allowed, err := GuardSpawnActive(conn, 0, proposalID, at)
	testsupport.Must(t, err, "GuardSpawnActive --deciding-vote: %v", err)
	if !allowed.Allowed {
		t.Fatalf("the panel that exists to decide %s's hold was denied by it: %s",
			model.FormatRunID(held), allowed.Reason)
	}
	for _, want := range []string{model.FormatProposalID(proposalID), model.FormatRunID(held)} {
		if !strings.Contains(allowed.Reason, want) {
			t.Errorf("the allow does not name %s: %q", want, allowed.Reason)
		}
	}

	// The carve-out admits a spawn; deciding is still the panel's to do.
	if len(openReapsOf(t, conn, held)) == 0 {
		t.Error("the carve-out acknowledged the reap; it admits the panel that will decide")
	}
	// Audited on the run it admitted, and only there.
	if n := carveOutEvents(t, conn, held); n != 1 {
		t.Errorf("%d spawn-admitted events on the served run, want 1", n)
	}
	if n := carveOutEvents(t, conn, other); n != 0 {
		t.Errorf("%d spawn-admitted events on the unheld run, want 0", n)
	}
}

// TestActiveDecidingVoteServingNoActiveRunDenies: a proposal linked to an
// issue whose only run has ended serves no active run, so a hold elsewhere
// denies plainly — no carve-out, the denial naming the held run and saying
// why the vote did not apply.
func TestActiveDecidingVoteServingNoActiveRunDenies(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	held, _ := activeRunOver(t, conn, "held")
	ended, endedIssue := activeRunOver(t, conn, "ended")
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, ended)
	at := reapWriterOf(t, conn, held)

	proposalID := openProposal(t, conn, "about a run that is over")
	linkProposal(t, conn, proposalID, endedIssue)

	verdict, err := GuardSpawnActive(conn, 0, proposalID, at)
	testsupport.Must(t, err, "GuardSpawnActive --deciding-vote: %v", err)
	if verdict.Allowed {
		t.Fatal("a proposal serving no active run admitted a spawn past another run's hold")
	}
	for _, want := range []string{
		model.FormatRunID(held), model.FormatProposalID(proposalID), "does not apply",
	} {
		if !strings.Contains(verdict.Reason, want) {
			t.Errorf("the denial does not carry %q: %q", want, verdict.Reason)
		}
	}
	if n := carveOutEvents(t, conn, held); n != 0 {
		t.Errorf("%d spawn-admitted events on a denied spawn, want 0", n)
	}
}

// TestActiveDecidingVoteDoesNotReachAnotherRunsHold: the proposal serves one
// run, and a hold on a run it does not serve denies the plain question exactly
// as before — the carve-out is the served run's alone.
func TestActiveDecidingVoteDoesNotReachAnotherRunsHold(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	served, servedIssue := activeRunOver(t, conn, "served")
	held, _ := activeRunOver(t, conn, "held")
	at := reapWriterOf(t, conn, held)

	proposalID := openProposal(t, conn, "about the other run")
	linkProposal(t, conn, proposalID, servedIssue)

	verdict, err := GuardSpawnActive(conn, 0, proposalID, at)
	testsupport.Must(t, err, "GuardSpawnActive --deciding-vote: %v", err)
	if verdict.Allowed {
		t.Fatal("a vote serving one run admitted a spawn past ANOTHER run's hold")
	}
	if !strings.Contains(verdict.Reason, model.FormatRunID(held)) {
		t.Errorf("the denial %q does not name the held run %s",
			verdict.Reason, model.FormatRunID(held))
	}
	if strings.Contains(verdict.Reason, model.FormatRunID(served)) {
		t.Errorf("the denial %q names the served, unheld run %s",
			verdict.Reason, model.FormatRunID(served))
	}
	for _, id := range []int{served, held} {
		if n := carveOutEvents(t, conn, id); n != 0 {
			t.Errorf("%d spawn-admitted events on %s, want 0", n, model.FormatRunID(id))
		}
	}
}

// TestActiveDecidingVoteRecordsNoCarveOutOnADenial: with the served run AND an
// unserved run both held, the answer is a denial — and the served run's
// carve-out, which alone would have admitted, is not audited, because no spawn
// was admitted. An event written per run would claim otherwise.
func TestActiveDecidingVoteRecordsNoCarveOutOnADenial(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	served, servedIssue := activeRunOver(t, conn, "served")
	other, _ := activeRunOver(t, conn, "other")
	at := reapWriterOf(t, conn, served)
	reapWriterOf(t, conn, other)

	proposalID := openProposal(t, conn, "acknowledge the served run's reap?")
	linkProposal(t, conn, proposalID, servedIssue)

	verdict, err := GuardSpawnActive(conn, 0, proposalID, at)
	testsupport.Must(t, err, "GuardSpawnActive --deciding-vote: %v", err)
	if verdict.Allowed {
		t.Fatal("a hold on an unserved run did not deny")
	}
	if !strings.Contains(verdict.Reason, model.FormatRunID(other)) {
		t.Errorf("the denial %q does not name the unserved run %s",
			verdict.Reason, model.FormatRunID(other))
	}
	if n := carveOutEvents(t, conn, served); n != 0 {
		t.Errorf("%d spawn-admitted events on the served run after a denial, want 0", n)
	}
}

// TestActiveDecidingVoteIsNarrow: the `--active` spelling keeps the carve-out's
// clauses. An id nobody created and a decided proposal are refused with the
// codes the `--run` path uses, whether or not the held run is served.
func TestActiveDecidingVoteIsNarrow(t *testing.T) {
	t.Run("a proposal that does not exist", func(t *testing.T) {
		conn := mustDB(t)
		runID := serializedRun(t, conn)
		at := reapWriterOf(t, conn, runID)

		_, err := GuardSpawnActive(conn, 0, 9999, at)
		if err == nil {
			t.Fatal("an id nobody created was answered as a proposal serving no run")
		}
		if code, _ := CodeOf(err); code != CodeNotFound {
			t.Errorf("code = %v, want NOT_FOUND", code)
		}
	})

	t.Run("a decided proposal serving the held run", func(t *testing.T) {
		conn := mustDB(t)
		registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
		runID, issueID := activeRunOver(t, conn, "held")
		at := reapWriterOf(t, conn, runID)

		id := openProposal(t, conn, "already settled")
		linkProposal(t, conn, id, issueID)
		execSQL(t, conn, `UPDATE proposals SET status = ? WHERE id = ?`,
			string(model.ProposalStatusApproved), id)

		_, err := GuardSpawnActive(conn, 0, id, at)
		if err == nil {
			t.Fatal("a decided proposal admitted the spawn")
		}
		if code, _ := CodeOf(err); code != CodeConflict {
			t.Errorf("code = %v, want CONFLICT", code)
		}
		if n := carveOutEvents(t, conn, runID); n != 0 {
			t.Errorf("%d spawn-admitted events after a refusal, want 0", n)
		}
	})
}
