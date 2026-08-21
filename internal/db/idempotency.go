package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Idempotency scopes. One per create verb, so the same key used on two
// different verbs does not collide.
const (
	ScopeIssueCreate  = "issue.create"
	ScopeDocCreate    = "doc.create"
	ScopeVoteCreate   = "vote.create"
	ScopeIssueComment = "issue.comment.add"
	ScopeDocComment   = "doc.comment.add"
	ScopeRunStart     = "run.start"
)

// LookupIdempotencyKey returns the entity id previously recorded for
// (scope, key), and whether such a record exists.
//
// A hit means the caller already performed this create — the correct response
// is to return the original entity with exit 0, not an error. A retried create
// after a dropped response must succeed, or the key is useless to the caller
// it exists for.
func LookupIdempotencyKey(db *sql.DB, scope, key string) (int, bool, error) {
	var entityID int
	err := db.QueryRow(
		`SELECT entity_id FROM idempotency_keys WHERE scope = ? AND key = ?`,
		scope, key,
	).Scan(&entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("looking up idempotency key: %w", err)
	}
	return entityID, true, nil
}

// LookupIdempotencyKeyTx is LookupIdempotencyKey inside a CALLER'S transaction.
//
// It exists for the same reason the plural form does: internal/db caps the pool
// at one connection, so a reader holding an open transaction cannot ask this
// question through the pool without deadlocking permanently. The single-key
// form is the one a caller wants when it knows exactly which key it is after —
// a prefix scan to find one row would read the whole family to discard it.
func LookupIdempotencyKeyTx(tx *sql.Tx, scope, key string) (int, bool, error) {
	var entityID int
	err := tx.QueryRow(
		`SELECT entity_id FROM idempotency_keys WHERE scope = ? AND key = ?`,
		scope, key,
	).Scan(&entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("looking up idempotency key: %w", err)
	}
	return entityID, true, nil
}

// LookupIdempotencyKeysTx returns every (key, entity id) in one scope whose key
// starts with prefix, inside a CALLER'S transaction.
//
// It exists for the reader that needs MANY of these at once and holds the
// single pooled connection while asking: one query over a family of keys rather
// than one per key, resolved where a pool read would deadlock rather than fail.
// What a prefix MEANS is the caller's business — this matches bytes.
//
// A `%` or `_` inside prefix is escaped rather than passed through to LIKE, so
// a caller cannot accidentally widen its own question with a key it built out
// of data.
func LookupIdempotencyKeysTx(tx *sql.Tx, scope, prefix string) (map[string]int, error) {
	rows, err := tx.Query(
		`SELECT key, entity_id FROM idempotency_keys
		  WHERE scope = ? AND key LIKE ? ESCAPE '\'`,
		scope, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("looking up idempotency keys: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			key      string
			entityID int
		)
		if err := rows.Scan(&key, &entityID); err != nil {
			return nil, fmt.Errorf("scanning an idempotency key: %w", err)
		}
		out[key] = entityID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("looking up idempotency keys: %w", err)
	}
	return out, nil
}

// escapeLike neutralizes LIKE's wildcards in a literal prefix.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// IdempotencyKeyOf is the REVERSE lookup: the key recorded for
// (scope, entityID), and whether one exists. It exists for the caller that
// holds an entity and needs the identity its create was keyed under — the
// engine recovering a vote-step proposal's run from the key OpenVoteProposal
// recorded — without a second, disagreeable copy of that link on the entity's
// own row. A create performed without a key (the historical non-idempotent
// wrappers) simply reports no row.
func IdempotencyKeyOf(db *sql.DB, scope string, entityID int) (string, bool, error) {
	var key string
	err := db.QueryRow(
		`SELECT key FROM idempotency_keys WHERE scope = ? AND entity_id = ? LIMIT 1`,
		scope, entityID,
	).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("looking up the idempotency key of %s %d: %w",
			scope, entityID, err)
	}
	return key, true, nil
}

// beginIdempotentCreate is the shared prologue of every CreateXIdempotent
// verb: check whether (scope, idempotencyKey) already recorded an entity, and
// if not, open the transaction the caller will insert into.
//
// hit reports a prior call: the caller must return existingID unchanged and
// must not touch tx (it is nil). Otherwise the caller owns tx — insert into
// it, call RecordIdempotencyKeyTx in the same transaction when idempotencyKey
// is non-empty, and Commit. The deferred Rollback is the caller's
// responsibility, matching every other transaction in this package.
//
// An empty idempotencyKey always skips the lookup, matching the historical
// behavior of the non-idempotent CreateX wrappers.
func beginIdempotentCreate(db *sql.DB, scope, idempotencyKey string) (existingID int, hit bool, tx *sql.Tx, err error) {
	if idempotencyKey != "" {
		existingID, hit, err = LookupIdempotencyKey(db, scope, idempotencyKey)
		if err != nil {
			return 0, false, nil, err
		}
		if hit {
			return existingID, true, nil, nil
		}
	}

	tx, err = db.Begin()
	if err != nil {
		return 0, false, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	return 0, false, tx, nil
}

// RecordIdempotencyKeyTx records (scope, key) -> entityID inside tx.
//
// It MUST be called in the same transaction as the insert it protects, so a
// crash between the two cannot orphan either. The (scope, key) primary key
// makes a concurrent duplicate a database constraint rather than an
// application-level check.
//
// created_at_ms and seq are millisecond-resolution and monotonic per
// engine-spec.md §5. They live here, in a table created at v5, and never on a
// pre-existing column — mutating an existing timestamp format would break
// byte-compatibility for every existing verb.
func RecordIdempotencyKeyTx(tx *sql.Tx, scope, key string, entityID int) error {
	nowMS := time.Now().UTC().UnixMilli()

	var nextSeq int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM idempotency_keys`,
	).Scan(&nextSeq); err != nil {
		return fmt.Errorf("allocating idempotency seq: %w", err)
	}

	_, err := tx.Exec(
		`INSERT INTO idempotency_keys (scope, key, entity_id, created_at_ms, seq)
		 VALUES (?, ?, ?, ?, ?)`,
		scope, key, entityID, nowMS, nextSeq,
	)
	if err != nil {
		return fmt.Errorf("recording idempotency key: %w", err)
	}
	return nil
}
