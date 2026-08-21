package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// THE WRITE-REAP ACKNOWLEDGMENT — §6's W1-W9 table, minus the two guard-spawn
// rows that group 3 adds (§11's group-2 row).
//
// W4 and W6 are adapted to `dispatch open --ack-reap`, which is THIS group's
// entry point and the one a NEW relay taking over from a crashed one uses. That
// is what makes the mechanism complete and usable without group 3: the ack has
// two entry points writing the same row, and the batch path ships here.

// writeLimitedSrc is W1's workflow: a bounded class with two steps in it.
//
// The class is called `write` because that is what the reference instance calls
// it — and the point of §6.5 is that CORE NEVER READS THE NAME. What makes these
// steps hold headroom is `max = 1`, and TestNoWriteClassLiteral asserts core
// contains no such literal.
const writeLimitedSrc = `
[pipeline]
name = "serialized"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = ["one"]

[[step]]
name = "read-a"
executor = "r"
class = "read"
emits = "out"
after = []

[[step]]
name = "read-b"
executor = "r"
class = "read"
emits = "out"
after = []
`

// serializedRun activates a run over writeLimitedSrc.
func serializedRun(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")
	issue := createIssue(t, conn, "serialized", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// openReaps reads a run's unacknowledged reaps through the same query the
// predicate uses.
func openReapsOf(t *testing.T, conn *sql.DB, runID int) []db.ReapAck {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	acks, err := db.UnacknowledgedReapsTx(tx, runID)
	testsupport.Must(t, err, "UnacknowledgedReapsTx: %v", err)
	return acks
}

// instancesIn collects the instances a `next` answer offered.
func instancesIn(answer *ReadySteps) []string {
	out := make([]string, 0, len(answer.Steps))
	for _, row := range answer.Steps {
		out = append(out, row.Instance)
	}
	return out
}

// reapOneWriter drives W2: claim the first write-class step, let its lease
// lapse, and run `next` to reap it. It returns the instant used and the reap's
// event seq.
func reapOneWriter(t *testing.T, conn *sql.DB, runID int) (at int64, seq int64) {
	t.Helper()

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if !contains(instancesIn(answer), "one@0") {
		t.Fatalf("premise: `one@0` must be ready, got %v", instancesIn(answer))
	}

	claim := claimInstance(t, conn, "one@0", nowMS)
	at = claim.LeaseExpiresMS + 1

	_, err = NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "`next` past the lease: %v", err)

	acks := openReapsOf(t, conn, runID)
	if len(acks) != 1 {
		t.Fatalf("W2: %d unacknowledged reaps after reaping a bounded class, want 1", len(acks))
	}
	return at, acks[0].ReapedSeq
}

// ---------------------------------------------------------------------------
// The run-status suspension, through the real entry points
// ---------------------------------------------------------------------------

// TestPausedRunReapsNothingThroughNext drives AC1 through `next`
// itself rather than the predicate, because the predicate passing proves
// nothing about the verb if the reap ever grows a second path.
//
// It also pins the consequence that makes the bug worth fixing: no reap means
// no `reap_acks` row, so an operator resolving the park does not additionally
// have to `--ack-reap` a writer that never died.
func TestPausedRunReapsNothingThroughNext(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if !contains(instancesIn(answer), "one@0") {
		t.Fatalf("premise: `one@0` must be ready, got %v", instancesIn(answer))
	}

	claim := claimInstance(t, conn, "one@0", nowMS)
	past := claim.LeaseExpiresMS + 1

	// The mid-wave human hold: the run parks while `one@0` is still claimed by
	// a worker that is alive and simply waiting.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunWaitingHuman), runID)

	_, err = NewEngine().NextSteps(conn, runID, 0, past)
	testsupport.Must(t, err, "`next` on a paused run past the lease: %v", err)

	if acks := openReapsOf(t, conn, runID); len(acks) != 0 {
		t.Errorf("`next` on a PAUSED run opened %d write-reap hold(s); the "+
			"lease lapsed only because the run was parked, and the operator "+
			"who resolves the park should not owe an --ack-reap for a writer "+
			"that never died (DKT-33)", len(acks))
	}
	if n := eventKindCount(t, conn, runID, EventLeaseReaped); n != 0 {
		t.Errorf("%d lease-reaped events on a paused run, want 0", n)
	}

	// Resuming restores the ordinary behaviour: the debt was suspended, not
	// forgiven, so the very next `next` reaps it.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunActive), runID)
	_, err = NewEngine().NextSteps(conn, runID, 0, past)
	testsupport.Must(t, err, "`next` after resuming: %v", err)
	if acks := openReapsOf(t, conn, runID); len(acks) != 1 {
		t.Errorf("%d unacknowledged reaps after the run resumed, want 1 — a "+
			"pause must suspend the TTL, not forgive it", len(acks))
	}
}

// TestD1ResolutionStillHoldsOnAnActiveRun is AC3, and the regression
// the run-status guard most plausibly causes.
//
// §5.8 D1 says a claimed-but-unrecorded step is resolved by lease expiry: "the
// step's TTL lapses, `next` reaps it, and the discrepancy dissolves". That
// resolution is only true because the reap at next.go runs BEFORE the refusal.
// If the guard had been written to consult run status in a way that made the
// reap conditional on something the refusal path changes, the ordinary path —
// core's default lease.ttl (15m) equal to dispatch.grace (15m) — would wedge
// every run behind a refusal naming a resolution that never happens.
//
// So: on an ACTIVE run, one `next` past the lease must both reap and answer.
func TestD1ResolutionStillHoldsOnAnActiveRun(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if !contains(instancesIn(answer), "one@0") {
		t.Fatalf("premise: `one@0` must be ready, got %v", instancesIn(answer))
	}

	claim := claimInstance(t, conn, "one@0", nowMS)

	// Past BOTH the lease and the dispatch grace, so the step is a live D1
	// discrepancy at the moment `next` is called.
	grace := int64(15 * 60 * 1000)
	past := claim.LeaseExpiresMS + grace + 1

	answer, err = NewEngine().NextSteps(conn, runID, 0, past)
	testsupport.Must(t, err, "D1's stated resolution did not happen: `next` past the lease "+
		"refused instead of reaping (%v). The reap must run before the "+
		"refusal, or the discrepancy is reported by the very invocation "+
		"whose reap clears it", err)

	// The reap really happened, and the step returned to the pool.
	if n := eventKindCount(t, conn, runID, EventLeaseReaped); n != 1 {
		t.Errorf("%d lease-reaped events, want 1", n)
	}
	if !contains(instancesIn(answer), "one@0") {
		t.Errorf("the reaped step was not re-offered (%v); W3 requires the step "+
			"the reap freed to be offered again", instancesIn(answer))
	}
}

// ---------------------------------------------------------------------------
// W1-W3, W5 — the hold
// ---------------------------------------------------------------------------

// TestReapedWriterGainsNoSuccessor is §9 item 10's second half, named as §6.6
// names it: "a reaped write-class step cannot gain a successor until the reap is
// acknowledged".
//
// It covers W1, W2, W3, and W5 in one scenario, because they are one scenario:
// the baseline, the reap, the hold, and the read fan-out that must survive it.
func TestReapedWriterGainsNoSuccessor(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)

	at, seq := reapOneWriter(t, conn, runID)
	if seq <= 0 {
		t.Fatalf("W2: the ack row names seq %d, which is not an event", seq)
	}

	// W2's other half: the reap is event-logged, and the ack row is created
	// UNACKNOWLEDGED (A19).
	if n := eventKindCount(t, conn, runID, EventLeaseReaped); n != 1 {
		t.Errorf("%d lease-reaped events, want 1", n)
	}
	acks := openReapsOf(t, conn, runID)
	if acks[0].AckedAtMS != nil {
		t.Error("A19: the ack row must be created unacknowledged")
	}
	if acks[0].Class != "write" {
		t.Errorf("the ack row names class %q, want the reaped step's class", acks[0].Class)
	}

	answer, err := NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "next after the reap: %v", err)
	offered := instancesIn(answer)

	// W3: the reaped step ITSELF is offered — it returned to the pool — but no
	// OTHER write-class step is, because the hold occupies the one slot.
	if !contains(offered, "one@0") {
		t.Errorf("W3: the reaped step is not offered (%v); it returned to the "+
			"pool and the hold is about its SUCCESSORS", offered)
	}

	// W5: read-class steps in the same run are STILL OFFERED (A6). The hold is
	// narrow — it holds write headroom, not the whole run — because read-only
	// fan-outs parallelize freely and a reaped writer must not stop them.
	for _, instance := range []string{"read-a@0", "read-b@0"} {
		if !contains(offered, instance) {
			t.Errorf("W5/A6: %s is not offered (%v); the hold must be NARROW, or "+
				"a reaped writer stops a run's read fan-out", instance, offered)
		}
	}

	// And the refusal EXPLAINS itself: a headroom denial with nothing running is
	// baffling unless the message names the reaps and the flag (§6.3).
	if answer.HeldReason == "" {
		t.Error("`next` reported no held reason while holding headroom")
	}
	for _, want := range []string{"one@0", "--ack-reap"} {
		if !strings.Contains(answer.HeldReason, want) {
			t.Errorf("the held reason %q does not name %q", answer.HeldReason, want)
		}
	}
}

// TestReapHoldBlocksTheSuccessor is W3's negative half, isolated.
//
// `two@0` declares `after = ["one"]`, so it is not ready until `one` is done —
// which means the scenario above cannot show the hold blocking it. This one
// drives `one@0` to `done` AFTER the reap, so the successor's only remaining
// blocker is the hold.
func TestReapHoldBlocksTheSuccessor(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	// `one@0` completes, WITH its usage recorded — the ordinary path, which is
	// what `complete --usage` writes. Without the flag the step would be a
	// legitimate D2 discrepancy once this run opens a dispatch to acknowledge
	// through, and `next` would refuse for a reason this test is not about.
	execSQL(t, conn,
		`UPDATE steps SET status = ?, usage_recorded = 1 WHERE run_id = ? AND instance = ?`,
		db.StepDone, runID, "one@0")

	answer, err := NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "next: %v", err)
	if contains(instancesIn(answer), "two@0") {
		t.Fatalf("W3: the successor was offered while a reap is unacknowledged "+
			"(%v). The DB fence is not a tree fence — a wedged writer must be "+
			"confirmed gone before a successor writes", instancesIn(answer))
	}

	// W7: acknowledge, and write-class steps flow again.
	ackVia(t, conn, runID, seq, at)

	answer, err = NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "next after the ack: %v", err)
	if !contains(instancesIn(answer), "two@0") {
		t.Errorf("W7: the successor is still withheld after the ack (%v)",
			instancesIn(answer))
	}
}

// ackVia acknowledges through THIS GROUP's entry point, `dispatch open
// --ack-reap` (W4/W6 as §11's group-2 row adapts them).
//
// The dispatch it opens is abandoned immediately: this helper is about the ack,
// and leaving a manifest open would make every subsequent `next` refuse for a
// reason the caller is not testing.
func ackVia(t *testing.T, conn *sql.DB, runID int, seq int64, at int64) {
	t.Helper()
	_, err := NewEngine().OpenDispatch(conn, runID, 0, []int64{seq}, at)
	testsupport.Must(t, err, "dispatch open --ack-reap %d: %v", seq, err)
	_, err = NewEngine().AbandonDispatch(conn, runID, "", at)
	testsupport.Must(t, err, "abandoning the ack's dispatch: %v", err)
}

// ---------------------------------------------------------------------------
// W6, W8, W9 — the ack's semantics
// ---------------------------------------------------------------------------

// TestAckRecordsWhoAcknowledged is W6 and A8: the row is acknowledged and
// `acked_by` records the VERB.
//
// A8 is explicit that `acked_by` is never a user identity, because core has no
// identity model. The assertion is therefore that the value is one of the two
// verb names, not that it identifies anybody.
func TestAckRecordsWhoAcknowledged(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	ackVia(t, conn, runID, seq, at)

	if acks := openReapsOf(t, conn, runID); len(acks) != 0 {
		t.Fatalf("%d reaps still unacknowledged after the ack", len(acks))
	}

	var ackedBy string
	var ackedAt sql.NullInt64
	err := conn.QueryRow(
		`SELECT COALESCE(acked_by, ''), acked_at_ms FROM reap_acks WHERE reaped_seq = ?`,
		seq).Scan(&ackedBy, &ackedAt)
	testsupport.Must(t, err, "reading the acknowledged row: %v", err)
	if ackedBy != db.AckByDispatchOpen {
		t.Errorf("acked_by = %q, want %q", ackedBy, db.AckByDispatchOpen)
	}
	if !ackedAt.Valid {
		t.Error("acked_at_ms is NULL on an acknowledged row")
	}

	// A3: the acknowledgment is ATTRIBUTABLE — releasing write headroom is a
	// transition, and §9 item 2 requires every transition to be traceable.
	if n := eventKindCount(t, conn, runID, EventReapAcknowledged); n != 1 {
		t.Errorf("%d reap-acknowledged events, want 1", n)
	}
}

// TestSecondAckIsASuccessThatChangesNothing is W8 and A10.
//
// Idempotency is what lets a relay retry its hook without failing. The test
// asserts the SECOND ack writes no second event as well as not failing: a
// success that recorded a duplicate transition would make the feed claim the
// headroom was released twice.
func TestSecondAckIsASuccessThatChangesNothing(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	ackVia(t, conn, runID, seq, at)
	before := eventKindCount(t, conn, runID, EventReapAcknowledged)

	// The second ack, through the same entry point.
	ackVia(t, conn, runID, seq, at)

	if after := eventKindCount(t, conn, runID, EventReapAcknowledged); after != before {
		t.Errorf("a second ack wrote %d more events; A10 makes it a success that "+
			"CHANGES NOTHING", after-before)
	}
}

// TestAckOfABogusSeqIsRefused is W9 and A9 — the FORGERY POINT.
//
// An ack must name a real reap. The run scope is half of the same clause: an ack
// naming another run's reap is a forgery in exactly the way naming a non-reap
// is, and both are covered here.
func TestAckOfABogusSeqIsRefused(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, realSeq := reapOneWriter(t, conn, runID)

	// A seq that is not a reap at all.
	_, err := NewEngine().OpenDispatch(conn, runID, 0, []int64{999_999}, at)
	if err == nil {
		t.Fatal("W9: an ack of a seq that names no reap succeeded")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("the refusal has code %q, want VALIDATION_ERROR", code)
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("the refusal %q does not name the seq it refused", err)
	}

	// A seq that IS a reap, but of another run. The other run gets its own
	// workflow registration through a second issue on the same definition.
	otherIssue := createIssue(t, conn, "other", "body", "task", nil)
	otherRun := startRun(t, conn, otherIssue)
	_, err = activate(conn, otherRun.ID)
	testsupport.Must(t, err, "activating the second run: %v", err)
	_, err = NewEngine().OpenDispatch(conn, otherRun.ID, 0, []int64{realSeq}, at)
	if err == nil {
		t.Fatal("A9: a run acknowledged ANOTHER run's reap — the run scope is " +
			"what stops a relay clearing a hold on a run it is not driving")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("the cross-run refusal has code %q, want VALIDATION_ERROR", code)
	}

	// And the real reap is still unacknowledged: a refused ack acknowledges
	// nothing.
	if acks := openReapsOf(t, conn, runID); len(acks) != 1 {
		t.Errorf("%d unacknowledged reaps after two refused acks, want 1", len(acks))
	}
}

// ---------------------------------------------------------------------------
// A12-A15 — the hold IS headroom
// ---------------------------------------------------------------------------

// TestReapHoldIsHeadroom is §6.3's named assertion, and it proves the FOLD
// rather than the block.
//
// A class with `max = 2` and ONE unacknowledged reap still admits ONE step. A
// separate `ReadyCondition` — the design this rejects — would block the class
// entirely, so this is the test that distinguishes "the hold occupies a slot"
// from "the hold stops the class".
func TestReapHoldIsHeadroom(t *testing.T) {
	const src = `
[pipeline]
name = "two-writers"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 2 }

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "three"
executor = "w"
class = "write"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "two-writers.toml")
	issue := createIssue(t, conn, "two-writers", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claim := claimInstance(t, conn, "one@0", nowMS)
	at := claim.LeaseExpiresMS + 1

	// The reap creates ONE hold in a class bounded at TWO.
	_, err = NewEngine().NextSteps(conn, run.ID, 0, at)
	testsupport.Must(t, err, "next: %v", err)
	if acks := openReapsOf(t, conn, run.ID); len(acks) != 1 {
		t.Fatalf("premise: want exactly 1 unacknowledged reap, got %d", len(acks))
	}

	answer, err := NewEngine().NextSteps(conn, run.ID, 0, at)
	testsupport.Must(t, err, "next: %v", err)
	offered := instancesIn(answer)

	// inFlight = 1 (the hold), max = 2, so 1 < 2 and the class still admits
	// steps. If the hold were a blocking condition rather than a fold, NOTHING
	// in this class would be offered.
	if len(offered) == 0 {
		t.Fatalf("a class with max=2 and ONE unacknowledged reap offered nothing; "+
			"the hold must OCCUPY A SLOT, not stop the class (%v)", offered)
	}

	// And claiming one more fills the class: 1 hold + 1 claimed = 2, so the
	// third step is not ready. That is the arithmetic, asserted from the other
	// side.
	claimInstance(t, conn, offered[0], at)
	answer, err = NewEngine().NextSteps(conn, run.ID, 0, at)
	testsupport.Must(t, err, "next: %v", err)
	for _, instance := range instancesIn(answer) {
		if strings.HasPrefix(instance, "one@") || strings.HasPrefix(instance, "two@") ||
			strings.HasPrefix(instance, "three@") {
			t.Errorf("%s is offered with 1 hold + 1 claim against max=2; the fold "+
				"must count the hold toward the bound", instance)
		}
	}
}

// TestUnboundedClassGetsNoAckRowAndNoHold is A18 and A21 — D3's dormancy.
//
// A class with no declared `max` is a class the author declared safe to
// parallelize. It gets NO ack row and NO hold, and `next` behaves exactly as it
// did at S5. That is what makes a repo with no `[limits]` see none of this
// mechanism at all.
func TestUnboundedClassGetsNoAckRowAndNoHold(t *testing.T) {
	const src = `
[pipeline]
name = "unbounded"
version = 1

[match]
kind = ["task"]

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "unbounded.toml")
	issue := createIssue(t, conn, "unbounded", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claim := claimInstance(t, conn, "one@0", nowMS)
	at := claim.LeaseExpiresMS + 1

	answer, err := NewEngine().NextSteps(conn, run.ID, 0, at)
	testsupport.Must(t, err, "next: %v", err)
	if len(answer.Reaped) == 0 {
		t.Fatal("premise: the lapsed lease must have been reaped")
	}

	// A18: NO ROW. The reap happened; the ledger stayed empty.
	var n int
	err = conn.QueryRow(`SELECT COUNT(*) FROM reap_acks`).Scan(&n)
	testsupport.Must(t, err, "counting reap_acks: %v", err)
	if n != 0 {
		t.Errorf("%d reap_acks rows for an UNBOUNDED class; A18 inserts nothing, "+
			"which is what makes a repo with no [limits] see none of this", n)
	}
	// A21: and nothing is held — both steps are offered.
	if answer.HeldReason != "" {
		t.Errorf("an unbounded class reported a hold: %q", answer.HeldReason)
	}
	if !contains(instancesIn(answer), "two@0") {
		t.Errorf("A21: the sibling is withheld in an unbounded class (%v)",
			instancesIn(answer))
	}
}

// ---------------------------------------------------------------------------
// A22 — the genericity guard
// ---------------------------------------------------------------------------

// TestNoWriteClassLiteral is §6.5 A22 and §10.3's standing assertion.
//
// Core never reads the string `"write"`. The mechanism is keyed on the DECLARED
// BOUND, because engine-spec §2 says the class name is instance policy — so a
// core mechanism keyed on the name would be unimplementable, and one that
// happened to work for a class called `write` would be instance policy living in
// core.
//
// It greps the IMPLEMENTATION in the shape of the existing genericity guards:
// the non-test sources of this package, where a literal would actually change
// behavior. Comments are stripped first, because the word appears throughout
// them — quoting engine-core §5's "write-class lease" is exactly how the
// argument gets recorded, and a guard that forbade discussing the spec would be
// a guard nobody could keep.
func TestNoWriteClassLiteral(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		testsupport.Must(t, err, "reading %s: %v", name, err)
		for i, line := range strings.Split(stripComments(string(src)), "\n") {
			if strings.Contains(line, `"write"`) {
				t.Errorf(`%s:%d contains the literal "write". Core ships no class `+
					`by that name: engine-spec §2 makes the class name INSTANCE `+
					`POLICY, so the write-reap hold is keyed on a finite [limits] `+
					`max (§6.5 A20-A22) and never on a string. Line: %s`,
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// stripComments removes Go line and block comments, so the literal guard scans
// CODE rather than the argument recorded beside it.
//
// It is a lexical strip rather than a parse: it does not need to be perfect, it
// needs to not report the word `write` inside a paragraph explaining why core
// never writes it. A string containing `//` would be over-stripped, and the
// consequence — a missed literal — is covered by the fact that the guard also
// runs over every other line of the same file.
func stripComments(src string) string {
	var b strings.Builder
	for {
		block := strings.Index(src, "/*")
		line := strings.Index(src, "//")
		switch {
		case block < 0 && line < 0:
			b.WriteString(src)
			return b.String()
		case block >= 0 && (line < 0 || block < line):
			b.WriteString(src[:block])
			end := strings.Index(src[block:], "*/")
			if end < 0 {
				return b.String()
			}
			src = src[block+end+2:]
		default:
			b.WriteString(src[:line])
			end := strings.Index(src[line:], "\n")
			if end < 0 {
				return b.String()
			}
			b.WriteString("\n")
			src = src[line+end+1:]
		}
	}
}
