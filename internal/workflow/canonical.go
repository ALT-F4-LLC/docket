package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Canonical serializes a validated definition to canonical JSON — the form
// stored in `workflows.parsed`.
//
// `parsed` is stored, not just `body`, because activation and `next` read the
// parsed form on a hot path: re-parsing TOML per read is both slower and a
// correctness hazard, since a parser change would silently re-interpret a
// PINNED definition. The canonical JSON is the pinned interpretation; `body`
// is retained so `workflow show --source` and the content hash mean something
// auditable.
//
// Byte-stability is a requirement, not a nicety. Map iteration order must not
// leak into `parsed`, or the same file would serialize differently on
// re-register and an idempotent re-register would look like a CONFLICT.
// encoding/json sorts map keys, and every slice here preserves declaration
// order, so the output is a pure function of the definition.
func Canonical(def *Definition) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(def); err != nil {
		return nil, fmt.Errorf("serializing workflow definition: %w", err)
	}
	// Encode appends a newline; the canonical form has none, so the same bytes
	// round-trip through Canonical(Parse(...)) unchanged.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// FromCanonical restores a definition from its canonical JSON. Round-tripping
// through this is what makes `parsed` the authoritative read path: a reader
// never re-parses the TOML.
func FromCanonical(data []byte) (*Definition, error) {
	var def Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("reading stored workflow definition: %w", err)
	}
	// `after` is not optional in the parsed form — a root step stores `[]` —
	// so the hasAfter bit is recoverable and does not need storing.
	for _, step := range def.Steps {
		step.hasAfter = step.After != nil
	}
	return &def, nil
}

// SHA256 is the content hash of the registered bytes, hex-encoded. It is what
// `workflows.source_sha256` stores and what a re-register compares against:
// a registered name@version is frozen, and version pinning is worth nothing if
// the pinned bytes can be swapped underneath a run.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
