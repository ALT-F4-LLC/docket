package schema

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
)

// AggregateName and AggregateVersion identify the one schema Docket ships
// (§7.6). It is the ONLY builtin: everything else in the registry got there
// because a user registered it.
const (
	AggregateName    = "aggregate"
	AggregateVersion = 1
)

// aggregateBody is the shipped `aggregate@1` document.
//
// It lives under internal/ because the Vorpal build's include list requires it
// — the same binding repo fact that puts the workflow templates there — and it
// is embedded rather than read from disk so a run's validation cannot depend on
// a file an operator can edit.
//
// It NAMES NO INSTANCE TOKEN (E2). It cannot: the reduced value's key is the
// author's field, which is exactly why `additionalProperties` is true. The
// genericity gate scans it, per §1.1.1, as the first embedded `*.json` core
// surface — every byte of a shipped schema is read by the first user who runs
// `docket schema show aggregate@1`.
//
//go:embed schemas/aggregate@1.json
var aggregateBody []byte

// AggregateBody returns the shipped document's bytes, verbatim.
//
// A copy is returned because these bytes are seeded into every database and
// hashed into every pin: a caller that mutated the package's own slice would
// change what `schema show` prints and what a re-seed compares against, with no
// diff anywhere to explain it.
func AggregateBody() []byte {
	out := make([]byte, len(aggregateBody))
	copy(out, aggregateBody)
	return out
}

// AggregateSHA256 is the hex content hash of the shipped document — the value
// seeded into `schemas.source_sha256` and recorded as a run's pin.
func AggregateSHA256() string {
	sum := sha256.Sum256(aggregateBody)
	return hex.EncodeToString(sum[:])
}

// AggregateRef renders `aggregate@1`.
func AggregateRef() string {
	return fmt.Sprintf("%s@%d", AggregateName, AggregateVersion)
}

// Aggregate compiles the shipped document.
//
// A failure here is a BUILD defect, not a runtime condition — the bytes are
// embedded and TestEmbeddedAggregateSchemaMatchesItsGolden pins them — so the
// error is returned rather than swallowed, and the one caller treats it as
// fatal.
func Aggregate() (*Registered, error) {
	return Compile(AggregateName, AggregateVersion, AggregateBody())
}
