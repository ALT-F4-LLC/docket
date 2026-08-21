package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Lease refusal sentinels. Each maps to exactly one CLI error code, per the
// refusal matrix in docs/tdd/claims-leases.md §4 (engine-spec.md §9 item 3).
var (
	// ErrLeaseHeld means a live lease is held by someone else — the claim
	// race's loser. Surfaced as CONFLICT (exit 4).
	ErrLeaseHeld = errors.New("lease held")

	// ErrNotHolder means the caller presented no matching capability: either
	// the entity is unclaimed, or the token is wrong. Surfaced as AUTH_ERROR
	// (exit 5).
	//
	// The two cases are deliberately not distinguished. "Unclaimed" and "wrong
	// token" are the same answer to the caller — you do not hold this lease —
	// and separating them would leak whether a lease exists to a caller
	// holding no capability.
	ErrNotHolder = errors.New("not the lease holder")

	// ErrLeaseExpired means the token is RIGHT but the lease lapsed. Surfaced
	// as STALE_LEASE (exit 6), which is the entire value of a separate code: a
	// holder seeing it knows to re-claim (its work may be redone), while
	// AUTH_ERROR means it never held the lease at all.
	ErrLeaseExpired = errors.New("lease expired")
)

// claimPredicate is the CAS guard that produces mutual exclusion: a claim wins
// only against an unclaimed entity or one whose lease has lapsed.
//
// This is the load-bearing line of the stage. SQLite evaluates it inside the
// UPDATE's write transaction, so exactly one of N concurrent writers can
// satisfy it — the losers see the winner's committed row and match zero rows.
// There is no read-then-write window because there is no read.
//
// Breaking this predicate must fail the concurrency tests; a mutation test
// (TestClaimPredicateIsLoadBearing) proves it does.
const claimPredicate = `(owner IS NULL OR owner = '' OR expires_ms <= ?)`

// leaseTarget parameterizes the lease implementation over the TABLE it leases
// (TDD §6.6). v6 shipped these helpers against `issues`; v7 leases `steps` with
// the same names, types, and nullability (TDD §2's lease-field reuse contract),
// so the SQL is GENERALIZED rather than copied.
//
// The copy is what this type exists to prevent. A second implementation of
// claimPredicate against `steps` would be a second place for the refusal matrix
// to live, and the two would drift the first time one of them was fixed — which
// is precisely why §6.9's step-level matrix is the S2 matrix with verbs
// substituted rather than a matrix of its own. TestLeaseHelpersAreShared asserts
// both entities reach the same function.
//
// `versionColumn` is the one genuine difference between the two tables:
// `issues` carries the v5 CAS column as `version`, `steps` as `row_version`
// (reliability-delta §6.1's naming, which v7 adopted for every new entity).
// Nothing else differs, and nothing else may: a target needing a second
// exception would mean the reuse contract had been broken somewhere upstream.
type leaseTarget struct {
	table         string
	versionColumn string
	// notFound is the sentinel a missing row reports. `issues` has used
	// ErrNotFound since v1 and every caller matches on it; `steps` reports
	// ErrStepNotFound so a CLI verb can name the entity it could not find.
	notFound error
}

// The two leased entities. Adding a third means adding a value here, not a
// second implementation of anything below it.
var (
	leaseIssues = leaseTarget{table: "issues", versionColumn: "version", notFound: ErrNotFound}
	leaseSteps  = leaseTarget{table: "steps", versionColumn: "row_version", notFound: ErrStepNotFound}
)

// claim is the shared CAS claim. See ClaimIssue for what every line of it is
// for; this is that function with the table name lifted out.
func (t leaseTarget) claim(
	db *sql.DB, id int, owner string, ttlMS, nowMS int64,
) (token string, lease *model.Lease, err error) {
	if owner == "" {
		return "", nil, fmt.Errorf("claiming %s: owner must not be empty", t.table)
	}

	token, hash, err := model.MintToken()
	if err != nil {
		return "", nil, err
	}
	expiresMS := nowMS + ttlMS

	tx, err := db.Begin()
	if err != nil {
		return "", nil, fmt.Errorf("beginning claim transaction: %w", err)
	}
	defer tx.Rollback()

	lease, err = t.claimTx(tx, id, owner, hash, expiresMS, nowMS)
	if err != nil {
		return "", nil, err
	}

	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("committing claim: %w", err)
	}
	return token, lease, nil
}

// claimTx is the claim's transaction body, separated so a caller that needs
// the claim and something else to commit together — `step claim`, which mints
// the token and assembles the context bundle in ONE transaction (§6.6) — shares
// this exact SQL rather than restating it.
func (t leaseTarget) claimTx(
	tx *sql.Tx, id int, owner, hash string, expiresMS, nowMS int64,
) (*model.Lease, error) {
	// The CAS claim, the version bump, and the attempt increment are one
	// statement, so no concurrent writer can interleave between them.
	stmt := `UPDATE ` + t.table + `
	            SET owner = ?, token_hash = ?, expires_ms = ?,
	                attempt = attempt + 1, ` + t.versionColumn + ` = ` + t.versionColumn + ` + 1
	          WHERE id = ? AND ` + claimPredicate
	res, err := tx.Exec(stmt, owner, hash, expiresMS, id, nowMS)
	if err != nil {
		return nil, fmt.Errorf("claiming %s: %w", t.table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("checking rows affected: %w", err)
	}

	if n == 0 {
		// Zero rows: distinguish "gone" from "held by someone else", exactly
		// as CheckAndBumpVersion does. Collapsing the two would report a live
		// conflict as a missing entity.
		var exists bool
		if err := tx.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM `+t.table+` WHERE id = ?)`, id,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("probing %s existence: %w", t.table, err)
		}
		if !exists {
			return nil, t.notFound
		}
		return nil, ErrLeaseHeld
	}

	var attempt int
	if err := tx.QueryRow(
		`SELECT attempt FROM `+t.table+` WHERE id = ?`, id,
	).Scan(&attempt); err != nil {
		return nil, fmt.Errorf("reading attempt: %w", err)
	}

	return &model.Lease{
		Owner: owner, TokenHash: hash, ExpiresMS: expiresMS, Attempt: attempt,
	}, nil
}

// getLease reads a lease without writing anything. Reads never write
// (engine-spec.md §6); liveness is computed by the caller via Lease.Live.
func (t leaseTarget) getLease(db *sql.DB, id int) (*model.Lease, error) {
	return t.scan(db.QueryRow(
		`SELECT owner, token_hash, expires_ms, attempt FROM `+t.table+` WHERE id = ?`, id,
	))
}

// getLeaseTx is getLease inside a transaction, so a refusal check and the
// mutation it guards commit or roll back together.
func (t leaseTarget) getLeaseTx(tx *sql.Tx, id int) (*model.Lease, error) {
	return t.scan(tx.QueryRow(
		`SELECT owner, token_hash, expires_ms, attempt FROM `+t.table+` WHERE id = ?`, id,
	))
}

// scan reads a lease row, mapping SQL NULLs to the unclaimed zero value.
func (t leaseTarget) scan(row *sql.Row) (*model.Lease, error) {
	var (
		owner     sql.NullString
		tokenHash sql.NullString
		expiresMS sql.NullInt64
		attempt   int
	)
	err := row.Scan(&owner, &tokenHash, &expiresMS, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, t.notFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading lease: %w", err)
	}
	return &model.Lease{
		Owner:     owner.String,
		TokenHash: tokenHash.String,
		ExpiresMS: expiresMS.Int64,
		Attempt:   attempt,
	}, nil
}

// authorize verifies that token holds a LIVE lease, inside tx. It is the single
// gate every lease-mediated mutating verb passes through, for BOTH entities —
// which is why §6.9's step matrix cannot drift from §4's issue matrix.
func (t leaseTarget) authorize(tx *sql.Tx, id int, token string, nowMS int64) (*model.Lease, error) {
	lease, err := t.getLeaseTx(tx, id)
	if err != nil {
		return nil, err
	}
	if err := authorizeLease(lease, token, nowMS); err != nil {
		return nil, err
	}
	return lease, nil
}

// authorizeRead is authorize over a lease read OUTSIDE any transaction.
//
// It exists for one caller: a refusal that must precede a write transaction and
// must still be an AUTH_ERROR rather than a content error (payloads-thresholds
// §4.8 C6 — "authorization precedes content, always, or an unauthenticated
// caller learns the schema by probing it"). It is ADVISORY: the authoritative
// check is still the in-transaction one, which is what a concurrent release or
// expiry races against.
func (t leaseTarget) authorizeRead(db *sql.DB, id int, token string, nowMS int64) error {
	lease, err := t.getLease(db, id)
	if err != nil {
		return err
	}
	return authorizeLease(lease, token, nowMS)
}

// authorizeLease is THE predicate, shared by both readers. Two copies of it
// would be two refusal matrices, and the whole point of the shared gate is that
// there is one.
func authorizeLease(lease *model.Lease, token string, nowMS int64) error {
	if !lease.Held() || !model.TokenMatches(token, lease.TokenHash) {
		return ErrNotHolder
	}
	if !lease.Live(nowMS) {
		return ErrLeaseExpired
	}
	return nil
}

// heartbeat extends a live lease held by token and returns the new expiry.
// attempt and owner are untouched: a heartbeat is not a new claim.
func (t leaseTarget) heartbeat(
	db *sql.DB, id int, token string, ttlMS, nowMS int64,
) (*model.Lease, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning heartbeat transaction: %w", err)
	}
	defer tx.Rollback()

	lease, err := t.authorize(tx, id, token, nowMS)
	if err != nil {
		return nil, err
	}

	expiresMS := nowMS + ttlMS
	if _, err := tx.Exec(
		`UPDATE `+t.table+` SET expires_ms = ?, `+
			t.versionColumn+` = `+t.versionColumn+` + 1 WHERE id = ?`,
		expiresMS, id,
	); err != nil {
		return nil, fmt.Errorf("extending lease: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing heartbeat: %w", err)
	}

	lease.ExpiresMS = expiresMS
	return lease, nil
}

// release ends a live lease held by token. attempt survives: it counts claims
// for all time, so releasing does not erase the trail of what has been tried.
func (t leaseTarget) release(db *sql.DB, id int, token string, nowMS int64) (*model.Lease, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning release transaction: %w", err)
	}
	defer tx.Rollback()

	lease, err := t.authorize(tx, id, token, nowMS)
	if err != nil {
		return nil, err
	}
	if err := t.clearLeaseTx(tx, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing release: %w", err)
	}
	return &model.Lease{Attempt: lease.Attempt}, nil
}

// clearLeaseTx drops the lease fields, leaving attempt intact.
//
// This is also the STEP saga's token retirement (§6.8 stage 1): the token
// retires when the artifact records, which is this operation, in the artifact's
// own transaction. Retirement and release are the same state change — no
// owner, no hash, no expiry — reached by two different authorities, so they are
// one function rather than two that must agree.
func (t leaseTarget) clearLeaseTx(tx *sql.Tx, id int) error {
	_, err := tx.Exec(
		`UPDATE `+t.table+`
		    SET owner = NULL, token_hash = NULL, expires_ms = NULL,
		        `+t.versionColumn+` = `+t.versionColumn+` + 1
		  WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("clearing lease: %w", err)
	}
	return nil
}

// ClaimIssue takes a lease on an issue and returns the minted capability
// token. It is one CAS transaction: exactly one of N concurrent claimants
// wins, and the losers get ErrLeaseHeld (engine-core.md §5).
//
// Expiry is reaped lazily, here and only here (engine-spec.md §6: "lazy lease
// reaping confined to next/claim; reads never write"). The `expires_ms <= now`
// disjunct IS the reaping — there is no reaper, no background pass, and no
// write from any read path. An expired lease is therefore re-claimable with no
// operator action beyond the claim itself, which is the liveness mechanism
// engine-spec.md §9 item 4 requires.
//
// attempt increments on every winning claim, including the one that replaces a
// holder that died mid-work. That is the complete attempt trail: it counts
// claims for all time and is never decremented or reset.
func ClaimIssue(db *sql.DB, id int, owner string, ttlMS int64, nowMS int64) (token string, lease *model.Lease, err error) {
	return leaseIssues.claim(db, id, owner, ttlMS, nowMS)
}

// GetIssueLease reads an issue's lease without writing anything.
//
// Reads never write (engine-spec.md §6). Liveness is computed from ExpiresMS by
// the caller via Lease.Live; nothing here reaps.
func GetIssueLease(db *sql.DB, id int) (*model.Lease, error) {
	return leaseIssues.getLease(db, id)
}

// getIssueLeaseTx is GetIssueLease inside a transaction, so a refusal check and
// the mutation it guards commit or roll back together.
func getIssueLeaseTx(tx *sql.Tx, id int) (*model.Lease, error) {
	return leaseIssues.getLeaseTx(tx, id)
}

// HeartbeatIssue extends a live lease held by token, and returns the new
// expiry. Any tool activity in the holder's session can drive this, so a
// working holder keeps its lease and a wedged or dead one lets it lapse
// (engine-core.md §5 "Leases").
//
// attempt and owner are untouched: a heartbeat is not a new claim.
func HeartbeatIssue(db *sql.DB, id int, token string, ttlMS int64, nowMS int64) (*model.Lease, error) {
	return leaseIssues.heartbeat(db, id, token, ttlMS, nowMS)
}

// ReleaseIssue ends a live lease held by token.
//
// attempt survives: it counts claims for all time, so releasing does not erase
// the trail of what has already been tried.
func ReleaseIssue(db *sql.DB, id int, token string, nowMS int64) (*model.Lease, error) {
	return leaseIssues.release(db, id, token, nowMS)
}

// clearLeaseTx drops an issue's lease fields, leaving attempt intact.
func clearLeaseTx(tx *sql.Tx, id int) error {
	return leaseIssues.clearLeaseTx(tx, id)
}

// AuthorizeLeaseMutation verifies that a caller may mutate an issue whose lease
// may or may not be live, and ends the lease when the caller is the holder.
//
// This is the guard for terminal verbs (`issue close`), which end a lease as a
// side effect the way a step's token retires when its artifact records
// (engine-spec.md §2).
//
// The dormancy rule is here, in the first branch: an issue with no LIVE lease
// is outside the mechanism entirely. No token is required, nothing is refused,
// and behavior is exactly what it was at v5. The token check fires only when a
// live lease exists — which is what makes engine-spec.md §9 item 8 hold for a
// repo that never claims.
func AuthorizeLeaseMutation(tx *sql.Tx, id int, token string, nowMS int64) error {
	lease, err := getIssueLeaseTx(tx, id)
	if err != nil {
		return err
	}
	if !lease.Live(nowMS) {
		// Unclaimed, or the lease already lapsed: not lease-mediated.
		// A lapsed lease is cleared here rather than left to mislead a later
		// reader, but this is a write on a MUTATING path only — no read verb
		// reaches it.
		if lease.Held() {
			return clearLeaseTx(tx, id)
		}
		return nil
	}
	if !model.TokenMatches(token, lease.TokenHash) {
		return ErrNotHolder
	}
	return clearLeaseTx(tx, id)
}
