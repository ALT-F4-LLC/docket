package engine

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `report executors` — the cross-run ledger (DKT-2453): per executor hint and
// per voter name, across every run in a window, read-only.

// ledgerRow finds one hint's row, failing the test when it is absent.
func ledgerRow(t *testing.T, ledger *ExecutorLedger, hint string) ExecutorLedgerRow {
	t.Helper()
	for _, row := range ledger.Executors {
		if row.Executor == hint {
			return row
		}
	}
	t.Fatalf("no ledger row for executor %q among %+v", hint, ledger.Executors)
	return ExecutorLedgerRow{}
}

// voterRow finds one voter name's row, failing the test when it is absent.
func voterRow(t *testing.T, ledger *ExecutorLedger, name string) VoterLedgerRow {
	t.Helper()
	for _, row := range ledger.Voters {
		if row.Voter == name {
			return row
		}
	}
	t.Fatalf("no ledger row for voter %q among %+v", name, ledger.Voters)
	return VoterLedgerRow{}
}

// recordRuling writes one attributed ruling event on a step, the shape the
// operator verbs leave (DKT-2450), without driving the verb's own
// preconditions: the ledger reads the event log, and the event is the fact.
func recordRuling(t *testing.T, conn *sql.DB, step *db.Step, kind, data string) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	err = recordEvent(tx, eventRecord{
		Kind: kind, RunID: step.RunID, Instance: step.Instance, IssueID: step.IssueID,
		Data: data, AtMS: nowMS,
	})
	testsupport.Must(t, err, "recording %s: %v", kind, err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
}

// TestExecutorLedgerGroupsRunsUnderOneHint is the issue's acceptance
// criterion: two runs whose steps declare the same executor hint land under
// ONE row, with both runs' steps, routings, rulings, and reaps summed there —
// and the seat that cast on both runs' panels lands under one voter row the
// same way.
func TestExecutorLedgerGroupsRunsUnderOneHint(t *testing.T) {
	conn := mustDB(t)
	firstRun, firstProposal, _ := evidenceRun(t, conn)
	secondRun, secondProposal, _ := secondEvidenceRun(t, conn)

	// The seat sits on both panels, with the panels' fates differing: the
	// first run's gate is resolved override-pass, the second's routes into a
	// fix loop.
	castSeat(t, conn, firstProposal, "alice", model.VerdictApprove, "")
	castSeat(t, conn, secondProposal, "alice", model.VerdictReject, "")
	castSeat(t, conn, secondProposal, "bob", model.VerdictApproveWithConcerns, "")

	firstGate := stepInRun(t, conn, firstRun, "gate@0")
	recordRuling(t, conn, firstGate, EventStepResolved,
		`{"detail":"override-pass","actor":"Erik","cwd":"/work"}`)
	execSQL(t, conn, `UPDATE steps SET routing = 'fix-loop: rejected' WHERE id = ?`,
		stepInRun(t, conn, secondRun, "gate@0").ID)

	// The hinted step of the first run was reaped twice: once by expiry, once
	// by a relay.
	firstSeed := stepInRun(t, conn, firstRun, "seed@0")
	recordRuling(t, conn, firstSeed, EventLeaseReaped, `{"reason":"lease expired"}`)
	recordRuling(t, conn, firstSeed, EventLeaseReaped,
		`{"forced":true,"reason":"spawn died","actor":"relay","cwd":"/work"}`)
	// ...and the second run's hinted step was resolved override-pass and
	// routed fix-loop, so the hint's own columns have something to sum.
	secondSeed := stepInRun(t, conn, secondRun, "seed@0")
	recordRuling(t, conn, secondSeed, EventStepResolved,
		`{"detail":"override-pass","actor":"Erik","cwd":"/work"}`)
	execSQL(t, conn, `UPDATE steps SET routing = 'fix-loop' WHERE id = ?`, secondSeed.ID)

	ledger, err := LoadExecutorLedger(conn, ExecutorLedgerOptions{ProjectID: 1})
	testsupport.Must(t, err, "LoadExecutorLedger: %v", err)

	if ledger.Runs != 2 {
		t.Errorf("runs = %d, want 2", ledger.Runs)
	}
	if len(ledger.Executors) != 1 {
		t.Fatalf("executors = %+v, want exactly one row: both runs' seed steps "+
			"declare the hint %q", ledger.Executors, "x")
	}
	x := ledgerRow(t, ledger, "x")
	want := ExecutorLedgerRow{
		Executor: "x", Runs: 2, Steps: 2, FixLoopRoutes: 1, OverridePasses: 1,
		Reaps: 2, ForcedReaps: 1,
	}
	if x != want {
		t.Errorf("row for x = %+v, want %+v", x, want)
	}

	alice := voterRow(t, ledger, "alice")
	wantAlice := VoterLedgerRow{
		Voter: "alice", Runs: 2, Casts: 2, Approve: 1, Reject: 1,
		FixLoopRoutes: 1, OverridePasses: 1,
	}
	if alice != wantAlice {
		t.Errorf("row for alice = %+v, want %+v", alice, wantAlice)
	}
	bob := voterRow(t, ledger, "bob")
	if bob.Runs != 1 || bob.Casts != 1 || bob.ApproveWithConcerns != 1 || bob.FixLoopRoutes != 1 {
		t.Errorf("row for bob = %+v, want one concerned cast on the fix-looped panel", bob)
	}
	if ledger.Voters[0].Voter != "alice" || ledger.Voters[1].Voter != "bob" {
		t.Errorf("voters ordered %s, %s; want ascending by name",
			ledger.Voters[0].Voter, ledger.Voters[1].Voter)
	}
}

// ledgerClusterPayload is sourcePayload plus a HELD cluster: C-3's spread
// reaches the fixture's hold_spread, and its two members come from the second
// review seat — so that seat's executor is credited a corroborated cluster
// that was also held.
const ledgerClusterPayload = `[
  {"id":"C-1","severity":"low","member_ids":["a-1"],"member_sources":["review@0#0"]},
  {"id":"C-2","severity":["medium","high"],"member_ids":["a-2","b-1"],"member_sources":["review@0#0","review@0#1"]},
  {"id":"C-3","severity":["low","blocker"],"member_ids":["b-2","b-3"],"member_sources":["review@0#1"]}
]`

// TestExecutorLedgerAttributesClustersToExecutors: over a round whose
// aggregate step declares `source_field`, each hint is credited the clusters
// its steps contributed to, split unique / corroborated / held — the same
// attribution the run report makes per run (DKT-2462), summed across runs.
func TestExecutorLedgerAttributesClustersToExecutors(t *testing.T) {
	conn := mustDB(t)
	registerFixtureSchema(t, conn)
	registerSource(t, conn, sourceFieldFixture(t), fixturePath)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	driveToReconcile(t, conn, testEngine(), ledgerClusterPayload)

	ledger, err := LoadExecutorLedger(conn, ExecutorLedgerOptions{ProjectID: 1})
	testsupport.Must(t, err, "LoadExecutorLedger: %v", err)

	correctness := ledgerRow(t, ledger, "judge-correctness")
	if correctness.UniqueClusters != 1 || correctness.CorroboratedClusters != 1 || correctness.HeldClusters != 0 {
		t.Errorf("judge-correctness clusters = unique %d / corroborated %d / held %d, want 1 / 1 / 0 (C-1 alone, C-2 with another seat)",
			correctness.UniqueClusters, correctness.CorroboratedClusters, correctness.HeldClusters)
	}
	architecture := ledgerRow(t, ledger, "judge-architecture")
	if architecture.UniqueClusters != 0 || architecture.CorroboratedClusters != 2 || architecture.HeldClusters != 1 {
		t.Errorf("judge-architecture clusters = unique %d / corroborated %d / held %d, want 0 / 2 / 1 (C-2 and the held C-3)",
			architecture.UniqueClusters, architecture.CorroboratedClusters, architecture.HeldClusters)
	}
	// A seat that contributed to no cluster still appears, with zeros: it ran
	// steps in the window, and a hint absent from the page reads as a hint
	// that never ran.
	simplicity := ledgerRow(t, ledger, "judge-simplicity")
	if simplicity.Steps != 1 || simplicity.UniqueClusters+simplicity.CorroboratedClusters != 0 {
		t.Errorf("judge-simplicity = %+v, want one step and no clusters", simplicity)
	}
}

// TestExecutorLedgerWindow: --since by run id and by instant, and the
// project scope, each bound the runs read.
func TestExecutorLedgerWindow(t *testing.T) {
	conn := mustDB(t)
	firstRun, _, _ := evidenceRun(t, conn)
	secondRun, _, _ := secondEvidenceRun(t, conn)
	execSQL(t, conn, `UPDATE runs SET created_at_ms = ? WHERE id = ?`, nowMS+60_000, secondRun)

	for _, tc := range []struct {
		name     string
		opts     ExecutorLedgerOptions
		wantRuns int
	}{
		{"unbounded", ExecutorLedgerOptions{ProjectID: 1}, 2},
		{"since the second run's id", ExecutorLedgerOptions{ProjectID: 1, SinceRun: secondRun}, 1},
		{"since an id past every run", ExecutorLedgerOptions{ProjectID: 1, SinceRun: secondRun + 1}, 0},
		{"since the second run's instant", ExecutorLedgerOptions{ProjectID: 1, SinceMS: nowMS + 60_000}, 1},
		{"since after both", ExecutorLedgerOptions{ProjectID: 1, SinceMS: nowMS + 120_000}, 0},
		{"another project", ExecutorLedgerOptions{ProjectID: 2}, 0},
		{"the whole store", ExecutorLedgerOptions{}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger, err := LoadExecutorLedger(conn, tc.opts)
			testsupport.Must(t, err, "LoadExecutorLedger: %v", err)
			if ledger.Runs != tc.wantRuns {
				t.Errorf("runs = %d, want %d (first %s, second %s)",
					ledger.Runs, tc.wantRuns, model.FormatRunID(firstRun), model.FormatRunID(secondRun))
			}
			if tc.wantRuns == 0 && (len(ledger.Executors) != 0 || len(ledger.Voters) != 0) {
				t.Errorf("an empty window still carries rows: %+v / %+v",
					ledger.Executors, ledger.Voters)
			}
			if tc.wantRuns == 0 && (ledger.Executors == nil || ledger.Voters == nil) {
				t.Error("an empty window must still carry empty arrays, not nulls")
			}
		})
	}
}

// TestExecutorLedgerWritesNothing is R8 for this verb: the database is
// byte-identical after the read, the same page-level check `run report`
// keeps — a ledger that reaped a lease on its way past would make "I only
// looked at it" untrue.
func TestExecutorLedgerWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.db")
	conn, err := sql.Open("sqlite", path)
	testsupport.Must(t, err, "opening: %v", err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	testsupport.Must(t, db.Initialize(conn), "Initialize")
	testsupport.Must(t, db.Migrate(conn), "Migrate")

	_, proposalID, _ := evidenceRun(t, conn)
	castSeat(t, conn, proposalID, "alice", model.VerdictApprove, "")
	// A lapsed lease on a second run's claimable step: the write a read verb
	// is most likely to make by accident.
	issue := createIssue(t, conn, "second subject", "body", "task", nil)
	second := startRun(t, conn, issue)
	_, err = activate(conn, second.ID)
	testsupport.Must(t, err, "activate the second run: %v", err)
	seed := stepInRun(t, conn, second.ID, "seed@0")
	_, err = ClaimStep(conn, seed.ID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim the second seed: %v", err)
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE id = ?`, seed.ID)

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing: %v", err)
	before := hashFile(t, path)

	ledger, err := LoadExecutorLedger(conn, ExecutorLedgerOptions{ProjectID: 1})
	testsupport.Must(t, err, "LoadExecutorLedger: %v", err)
	if len(ledger.Executors) == 0 || len(ledger.Voters) == 0 {
		t.Fatal("the ledger read nothing, so a zero-write result proves nothing")
	}

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing after the read: %v", err)
	if after := hashFile(t, path); after != before {
		t.Error("`report executors` changed the database; it must write nothing")
	}
}
