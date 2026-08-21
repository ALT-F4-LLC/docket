package engine

import (
	"database/sql"
	"fmt"
	"strconv"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Terminal transitions close what they orphan (DKT-262).
//
// An open proposal is NOT INERT. `vote list` shows it to an operator as
// outstanding work, and since DKT-236 it is also what a spawn-guard carve-out
// points at — so a stale entry makes two surfaces lie, one of them a guard.
//
// Four proposals of the current epoch stood open forever because the thing they
// were attached to ended without closing them: DKT-V11 was orphaned when RUN-4
// was abandoned, and DKT-V21 and DKT-V46 were ack-reap ballots the operator
// satisfied directly. DKT-114 added the close verb; nothing called it
// automatically, which is what left them standing.
//
// EVERY CLOSE HERE RIDES INSIDE THE TRANSITION'S OWN TRANSACTION. A close
// committed separately can be lost while the transition stands, which puts the
// row back in exactly the state this file exists to prevent — and the window is
// widest at abandonment, which is where an interrupted session is most likely.

// ReapAckProposalKey is the idempotency key an ack-reap ballot is created
// under, so the acknowledgment that satisfies it can find it.
//
// THE ENGINE OWNS THIS CONVENTION EVEN THOUGH THE ENGINE DOES NOT CREATE THE
// BALLOT. A conductor routing a reap to a panel opens the proposal; the engine
// applies the acknowledgment. Only one of the two can be the definition, and it
// has to be the side that must find the row later — a convention living in the
// creator would leave the finder guessing, which is how these ballots came to
// be uncloseable in the first place.
//
// A conductor that does not use it loses nothing it had: its proposal simply is
// not auto-closed, exactly as today.
func ReapAckProposalKey(runID int, seq int64) string {
	return "reap-ack:" + strconv.Itoa(runID) + ":" + strconv.FormatInt(seq, 10)
}

// closeRunProposalsTx closes every OPEN proposal this run's vote steps opened,
// naming the transition that orphaned them.
//
// It resolves them through the SAME prefix scan the report and the scheduler
// use, so a proposal is findable here exactly when it is findable there. A
// second way to enumerate a run's proposals is a second thing to keep in
// agreement with the key writer.
//
// Ad-hoc proposals — an operator's own, bound to no step — are NOT touched.
// They are outside the run's machinery and their author decides when they end;
// closing them because a run happened to finish would be the engine disposing
// of a question nobody asked it to hold.
func closeRunProposalsTx(tx *sql.Tx, runID int, reason string) (int, error) {
	keys, err := db.LookupIdempotencyKeysTx(
		tx, db.ScopeVoteCreate, voteIdempotencyPrefix(runID))
	if err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}
	ids := make([]int, 0, len(keys))
	for _, id := range keys {
		ids = append(ids, id)
	}
	return db.CloseOpenProposalsTx(tx, ids, reason)
}

// closeReapAckProposalTx closes the ballot an acknowledged reap was convened to
// decide.
//
// The acknowledgment IS the answer. A panel was asked whether to accept a
// reap; an operator acknowledged it directly, so the question is settled and
// the ballot has nothing left to decide. Leaving it open reports a settled
// question as pending work — and, since DKT-236, offers a spawn-guard carve-out
// pointing at a decision that was already made elsewhere.
//
// A missing ballot is NOT an error and not a warning. Most reaps are
// acknowledged with no panel ever convened, so "no proposal under this key" is
// the ordinary case rather than a problem to report.
func closeReapAckProposalTx(tx *sql.Tx, runID int, seq int64) error {
	id, found, err := db.LookupIdempotencyKeyTx(
		tx, db.ScopeVoteCreate, ReapAckProposalKey(runID, seq))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = db.CloseOpenProposalsTx(tx, []int{id}, fmt.Sprintf(
		"the reap at seq %d was acknowledged directly by an operator, so this "+
			"ballot's question was settled out of band; it was never decided",
		seq))
	return err
}

// closeStepProposalTx closes the ballot ONE step opened, when it has one.
//
// The supersession case (DKT-V3, "superseded by a later proposal, never
// closed"): a loop's next ordinal sweeps the previous one's steps, and a vote
// step swept that way has been asked a question the loop has moved past. The
// ballot it opened is about an ordinal that no longer exists.
//
// A step with no proposal is the ordinary case and reports nothing — the sweep
// covers every kind of step, and only vote steps ever open one.
func closeStepProposalTx(
	tx *sql.Tx, runID, issueID int, instance, reason string,
) error {
	id, found, err := db.LookupIdempotencyKeyTx(
		tx, db.ScopeVoteCreate, voteIdempotencyKey(runID, issueID, instance))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = db.CloseOpenProposalsTx(tx, []int{id}, reason)
	return err
}

// abandonedProposalReason is what a run-abandonment writes into the proposals
// it closes.
//
// It says THE RUN ENDED, never that the vote was decided — the issue's own
// wording, and the distinction is the whole point. These ballots reached no
// verdict, and a `final_outcome` that read like one would replace an honest
// "open forever" with a dishonest "decided", which is worse: a reader can spot
// a stale open row, and cannot spot an invented decision.
func abandonedProposalReason(runID int, reason string) string {
	out := fmt.Sprintf(
		"%s was abandoned before this vote was decided; the ballot is closed "+
			"because its run ended, not because a verdict was reached",
		model.FormatRunID(runID))
	if reason != "" {
		out += " (run abandoned: " + reason + ")"
	}
	return out
}

// compile-time note: closeRunProposalsTx takes *sql.Tx rather than *sql.DB
// because internal/db caps the pool at ONE connection, and both callers hold an
// open transaction when they reach it. A pool read from there deadlocks
// permanently rather than failing.
var _ = (*sql.DB)(nil)
