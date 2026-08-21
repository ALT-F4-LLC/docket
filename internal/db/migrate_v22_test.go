package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// v22 — the pause-origin column (DKT-305). Three obligations: the column
// arrives on a migrated store, the rewind guard converges a store stamped 22
// without it, and the BACK-FILL marks the runs whose standing park the rows
// already prove. The behavior the column exists for — the rollup declining to
// resume a run-level park — is the engine's to prove.

func TestMigrateToV22(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
	for _, col := range v22ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV22RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 22 — the mid-change-binary database v13's guard comment describes —
// and asserts Migrate converges it.
//
// The column form matters here: v22 adds no table and no index, so every v21
// sentinel is present on such a store and a table probe would never fire.
func TestV22RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE runs DROP COLUMN pause_origin`)
	if exists, _ := hasColumnDB(db, "runs", "pause_origin"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	exists, err := hasColumnDB(db, "runs", "pause_origin")
	if err != nil || !exists {
		t.Fatalf("the rewind guard did not converge runs.pause_origin back (err %v)", err)
	}
}

// TestV22BackfillsStandingRunLevelPauses is the part of v22 that is NOT the
// usual additive column, and it is deliberate: a run parked at the moment of
// upgrade would otherwise come up unmarked, and the next step to route would
// resume it — the exact DKT-305 reproduction, caused by the fix for it.
//
// Nothing is invented. A live `breach_reason` on a parked run is what
// `BreachRunBudgetTx` writes and nothing else does; a parked run with NO parked
// step can only have been parked by a run-level verb, because the rollup parks
// a run exclusively when it counts one. Every other row keeps the empty
// default: a step-level park un-parks itself, and a run that is not parked has
// no origin to record.
func TestV22BackfillsStandingRunLevelPauses(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	res, err := db.Exec(
		`INSERT INTO workflows (name, version, source_path, source_sha256, body, parsed,
		                        created_at_ms, row_version)
		 VALUES ('w', 1, 'w.toml', 'sha', 'bytes', '{}', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a workflow: %v", err)
	}
	wfID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO issues (title, status, created_at, updated_at)
		 VALUES ('mid-flight', 'todo', '2026-08-19', '2026-08-19')`)
	if err != nil {
		t.Fatalf("seeding an issue: %v", err)
	}
	issueID, _ := res.LastInsertId()

	// Four runs, one per shape the back-fill must tell apart.
	seedRun := func(status, breach string) int64 {
		t.Helper()
		res, err := db.Exec(
			`INSERT INTO runs (request, status, breach_reason, created_at_ms, updated_at_ms)
			 VALUES ('', ?, ?, 1, 1)`, status, breach)
		if err != nil {
			t.Fatalf("seeding a %s run: %v", status, err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	breached := seedRun(string(model.RunWaitingHuman), "budget: spend 9 of cap 8 reached")
	operatorPaused := seedRun(string(model.RunWaitingHuman), "")
	stepParked := seedRun(string(model.RunWaitingHuman), "")
	running := seedRun(string(model.RunActive), "")

	seedStep := func(runID int64, instance, status string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, instance, kind,
			                    status, created_at_ms, updated_at_ms)
			 VALUES (?, ?, ?, 'a', ?, 'executor', ?, 1, 1)`,
			runID, issueID, wfID, instance, status); err != nil {
			t.Fatalf("seeding a step: %v", err)
		}
	}
	// The operator-paused run has work in flight but nothing PARKED — the
	// shape that makes the origin underivable from the step tables alone, and
	// the reason the column exists.
	seedStep(operatorPaused, "a@0", StepClaimed)
	seedStep(stepParked, "a@0", StepWaitingHuman)
	seedStep(running, "a@0", StepPending)

	// Rewind to v21: drop the column and the stamp, then migrate forward over
	// rows that predate it.
	mustExec(t, db, `ALTER TABLE runs DROP COLUMN pause_origin`)
	mustExec(t, db, `UPDATE meta SET value = '21' WHERE key = 'schema_version'`)
	if err := Migrate(db); err != nil {
		t.Fatalf("migrating v21 -> v22 over standing runs: %v", err)
	}

	originOf := func(runID int64) model.RunPauseOrigin {
		t.Helper()
		var origin string
		if err := db.QueryRow(
			`SELECT pause_origin FROM runs WHERE id = ?`, runID).Scan(&origin); err != nil {
			t.Fatalf("reading pause_origin of run %d: %v", runID, err)
		}
		return model.RunPauseOrigin(origin)
	}

	for _, c := range []struct {
		name  string
		runID int64
		want  model.RunPauseOrigin
		why   string
	}{
		{"breached", breached, model.RunPauseOriginBudget,
			"a parked run with a live breach_reason was parked by the budget (DKT-68)"},
		{"operator-paused", operatorPaused, model.RunPauseOriginOperator,
			"a parked run with no parked step was parked by a run-level verb"},
		{"step-parked", stepParked, model.RunPauseOriginNone,
			"the rollup parked this run and the rollup un-parks it"},
		{"active", running, model.RunPauseOriginNone,
			"a run that is not parked has no origin to record"},
	} {
		if got := originOf(c.runID); got != c.want {
			t.Errorf("%s run: pause_origin = %q, want %q — %s",
				c.name, got, c.want, c.why)
		}
	}
}
