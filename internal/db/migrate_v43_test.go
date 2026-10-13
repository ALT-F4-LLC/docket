package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// v43 — `steps.recovery_hash`, the SHA-256 of the recovery secret a claim was
// minted with. Only the hash is stored; no secret means NULL.

func TestMigrateToV43StoresOnlyTheRecoverySecretHash(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v42 store: the column absent, the stamp one back, and a step written
	// in the v42 shape.
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN recovery_hash`)
	mustExec(t, db, `UPDATE meta SET value = '42' WHERE key = 'schema_version'`)
	_, stepID := seedRunAndStep(t, db)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v42: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v42 = %d, want %d",
			v, currentSchemaVersion)
	}
	if got := storedRecoveryHash(t, db, stepID); got.Valid {
		t.Errorf("a pre-v43 step reads recovery_hash %q, want NULL", got.String)
	}

	secret := mintRecoverySecret(t)
	claimWithRecovery(t, db, stepID, "wave:a", secret, 1000)

	if got := storedRecoveryHash(t, db, stepID); !got.Valid || got.String != model.HashToken(secret) {
		t.Errorf("recovery_hash = %v, want HashToken(secret)", got)
	}
	assertNoColumnHoldsSecret(t, db, stepID, secret)
}

// TestEveryStepClaimRewritesTheRecoveryHash pins that a claim without a secret
// stores NULL, never HashToken(""), and that a later claim replaces an earlier
// lease's hash rather than inheriting it.
func TestEveryStepClaimRewritesTheRecoveryHash(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	_, stepID := seedRunAndStep(t, db)
	first, second := mintRecoverySecret(t), mintRecoverySecret(t)

	claimWithRecovery(t, db, stepID, "wave:1", first, 1000)
	if got := storedRecoveryHash(t, db, stepID); got.String != model.HashToken(first) {
		t.Fatalf("first claim stored %v, want HashToken(first)", got)
	}

	// Each later claim lands after the previous lease expired (TTL 10).
	claimWithRecovery(t, db, stepID, "wave:2", "", 2000)
	if got := storedRecoveryHash(t, db, stepID); got.Valid {
		t.Errorf("a claim without a secret left recovery_hash %q, want NULL", got.String)
	}

	claimWithRecovery(t, db, stepID, "wave:3", second, 3000)
	if got := storedRecoveryHash(t, db, stepID); got.String != model.HashToken(second) {
		t.Errorf("third claim stored %v, want HashToken(second)", got)
	}
}

// TestV43RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 43 and asserts Migrate converges it. v43 adds no table and no
// index, so only a column probe can notice.
func TestV43RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN recovery_hash`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "steps", "recovery_hash"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge steps.recovery_hash back (err %v)", err)
	}
}

// mintRecoverySecret returns a fresh secret in the shape a claimant supplies:
// 64 hex characters, the same length as its hash, so only an exact comparison
// tells a stored secret from a stored hash.
func mintRecoverySecret(t *testing.T) string {
	t.Helper()
	secret, _, err := model.MintToken()
	if err != nil {
		t.Fatalf("minting a recovery secret: %v", err)
	}
	return secret
}

func claimWithRecovery(t *testing.T, db *sql.DB, stepID int, owner, secret string, nowMS int64) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if _, _, err := ClaimStepWithRecoveryTx(tx, stepID, owner, secret, 10, nowMS); err != nil {
		t.Fatalf("ClaimStepWithRecoveryTx(%s): %v", owner, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func storedRecoveryHash(t *testing.T, db *sql.DB, stepID int) sql.NullString {
	t.Helper()
	var hash sql.NullString
	if err := db.QueryRow(`SELECT recovery_hash FROM steps WHERE id = ?`,
		stepID).Scan(&hash); err != nil {
		t.Fatalf("reading recovery_hash: %v", err)
	}
	return hash
}

// assertNoColumnHoldsSecret scans every column of the step row, so a secret
// written to any column, metadata included, is caught.
func assertNoColumnHoldsSecret(t *testing.T, db *sql.DB, stepID int, secret string) {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM steps WHERE id = ?`, stepID)
	if err != nil {
		t.Fatalf("selecting the step row: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("reading columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("step %d not found", stepID)
	}
	values := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatalf("scanning the step row: %v", err)
	}
	for i, v := range values {
		if strings.Contains(string(v), secret) {
			t.Errorf("column %s holds the raw recovery secret", cols[i])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating the step row: %v", err)
	}
}
