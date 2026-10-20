package pgstore

// v1 and v7 already create issues.size and steps.park_reason, park_class and
// the loop columns, as SQLite's own DDL does, so v32-v35's ADD COLUMN IF NOT
// EXISTS is then a no-op, as SQLite's column probe makes it there.

// v31DDL ports migrateV30ToV31.
const v31DDL = `
ALTER TABLE dispatches ADD COLUMN IF NOT EXISTS extended_seq BIGINT NOT NULL DEFAULT 0;
`

// v32DDL ports migrateV31ToV32.
const v32DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS park_reason TEXT NOT NULL DEFAULT '';
`

// v33DDL ports migrateV32ToV33.
const v33DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS park_class TEXT NOT NULL DEFAULT '';
`

// v34DDL ports migrateV33ToV34.
const v34DDL = `
ALTER TABLE issues ADD COLUMN IF NOT EXISTS size TEXT NOT NULL DEFAULT '';
`

// v35DDL ports migrateV34ToV35.
const v35DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS loop_rounds_run BIGINT NOT NULL DEFAULT 0;
ALTER TABLE steps ADD COLUMN IF NOT EXISTS loop_trigger_step TEXT NOT NULL DEFAULT '';
ALTER TABLE steps ADD COLUMN IF NOT EXISTS loop_latest_verdict TEXT NOT NULL DEFAULT '';
`

// v36DDL ports migrateV35ToV36. NULL means the schema version is in service.
const v36DDL = `
ALTER TABLE schemas ADD COLUMN IF NOT EXISTS deprecated_at_ms BIGINT;
`

// v37DDL ports migrateV36ToV37.
const v37DDL = `
ALTER TABLE gate_results ADD COLUMN IF NOT EXISTS target_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE proposals ADD COLUMN IF NOT EXISTS hold_on_dissent BIGINT NOT NULL DEFAULT 0;
`

// v38DDL ports migrateV37ToV38. argv stays TEXT: its default, the legacy
// marker db.grantArgvLegacy, is deliberately not JSON.
const v38DDL = `
ALTER TABLE gate_override_grants ADD COLUMN IF NOT EXISTS argv TEXT NOT NULL DEFAULT 'legacy';
`

// v39DDL ports migrateV38ToV39, including its back-fill of terminal steps.
// The literals are the db.StepDone, StepSkipped, StepSuperseded and
// StepFailedRouted values the SQLite migration binds. The update touches only
// rows still at zero, so a rerun changes nothing.
const v39DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS recorded_at_ms BIGINT NOT NULL DEFAULT 0;

UPDATE steps SET recorded_at_ms = updated_at_ms
 WHERE recorded_at_ms = 0 AND status IN ('done', 'skipped', 'superseded', 'failed-routed');
`

// v40DDL ports migrateV39ToV40.
const v40DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS authority TEXT NOT NULL DEFAULT '';
ALTER TABLE steps ADD COLUMN IF NOT EXISTS authority_ref TEXT NOT NULL DEFAULT '';
`

// v41DDL ports migrateV40ToV41.
const v41DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS claim_phase TEXT NOT NULL DEFAULT '';
`

// v42DDL ports migrateV41ToV42.
const v42DDL = `
ALTER TABLE run_notes ADD COLUMN IF NOT EXISTS authority TEXT NOT NULL DEFAULT '';
`

// v43DDL ports migrateV42ToV43. NULL means the claim supplied no recovery
// secret.
const v43DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS recovery_hash TEXT;
`

// v44DDL ports migrateV43ToV44.
const v44DDL = `
ALTER TABLE steps ADD COLUMN IF NOT EXISTS loop_cap BIGINT NOT NULL DEFAULT 0;
`

// v45DDL ports migrateV44ToV45. NULL means the pin was recorded without its
// bytes.
const v45DDL = `
ALTER TABLE pins ADD COLUMN IF NOT EXISTS content BYTEA;
`

// v46DDL ports migrateV45ToV46: the one-row event low watermark, seeded so
// every seq below the lowest surviving event reads as pruned.
const v46DDL = `
CREATE TABLE IF NOT EXISTS event_low_watermark (
	id  BIGINT PRIMARY KEY CHECK (id = 1),
	seq BIGINT NOT NULL
);

INSERT INTO event_low_watermark (id, seq)
	SELECT 1, COALESCE(MIN(seq) - 1, 0) FROM events
ON CONFLICT DO NOTHING;
`

// v47DDL ports migrateV46ToV47. NULL means the key was recorded without a
// request fingerprint.
const v47DDL = `
ALTER TABLE idempotency_keys ADD COLUMN IF NOT EXISTS fingerprint TEXT;
`
