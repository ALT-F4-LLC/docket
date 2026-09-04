package engine

import (
	"encoding/json"
	"fmt"
)

// COMPLETION METADATA (docs/tdd/completion-metadata.md).
//
// `step complete --metadata` was accepted, reported success, and dropped. Every
// other part of the path already existed and was correct: the flag parsed, the
// option field was declared and documented as "opaque KV merged onto the step's
// own", the column existed, and the R7 rollup read it correctly and
// generically. What was absent was the one write connecting them — nothing read
// `opts.Metadata`, so zero steps in any database carried metadata and the
// rollup was starved rather than wrong.
//
// The failure was SILENT, which is the part worth fixing carefully: an instance
// reporting its bag got exit 0 and an empty rollup, and reasonably concluded its
// own emission was at fault.
//
// THE MERGE READS NO KEY. It overlays an opaque bag onto another opaque bag and
// never names, branches on, or interprets a key — the same promise
// db.MetadataRollup makes at the read end, asserted mechanically by
// TestMergeMetadataReadsNoKey. A merge that special-cased one key would be core
// having an opinion about what a workflow author's bag of strings means.

// MetadataMaxBytes caps one `--metadata` bag, on every verb that takes one —
// `claim`, `complete`, `fail`, `annotate`.
//
// The bag is opaque, so nothing else bounds it. Without a cap a worker can push
// an artifact-sized body into a column the R7 rollup groups BY DISTINCT VALUE,
// turning a report into a wall of unique multi-kilobyte strings — a slow,
// silent degradation of the one read surface this path exists to feed.
//
// 16 KiB is roughly three orders of magnitude above the motivating use (a
// handful of short routing keys, ~120 bytes): generous enough that no
// legitimate KV bag meets it, small enough that the rollup stays legible.
//
// It is a CONSTANT, not config, following db.ArtifactMaxBytes. A new engine
// config key is a surface addition needing its own spec line, and no evidence
// exists that any instance needs a different value.
const MetadataMaxBytes = 16 << 10

// mergeMetadata overlays a completing worker's opaque KV bag onto the step's
// own, returning the JSON text to store.
//
// LAST-WRITE-WINS PER TOP-LEVEL KEY, and the merge is SHALLOW — one level. A
// nested object is a VALUE, not a subtree to descend into. Deep-merging would
// require core to have an opinion about the STRUCTURE of what an instance put
// in its bag, which is the genericity line; shallow also keeps the result
// dependent only on which keys were written, never on how deeply an instance
// happened to nest them.
//
// It is a PURE FUNCTION, deliberately: it knows nothing about sagas,
// transactions, or which verb called it. `stageZero` calls it today; the
// `fail --metadata` parity calls the same function and the same writer, which
// is what makes that a matter of adding a flag rather than a mechanism.
//
// Determinism: `encoding/json` marshals map keys in sorted order, so the stored
// text is stable regardless of the order keys arrived in and R9's goldens do
// not flap on Go map iteration order.
func mergeMetadata(definition, completion string) (string, error) {
	// Nothing to merge: the step keeps its definition bag, byte-identical. This
	// is the path every existing caller takes, none of which passes the flag.
	if completion == "" {
		return definition, nil
	}

	incoming, err := DecodeMetadataBag(completion, "metadata")
	if err != nil {
		return "", err
	}

	// A definition bag that does not parse is the step's own recorded state, so
	// the error names it as such rather than blaming the caller's input.
	base, err := DecodeMetadataBag(definition, "the step's recorded metadata")
	if err != nil {
		return "", err
	}
	if base == nil {
		base = make(map[string]any, len(incoming))
	}

	for key, value := range incoming {
		base[key] = value
	}

	if len(base) == 0 {
		return "", nil
	}
	out, err := json.Marshal(base)
	if err != nil {
		return "", fmt.Errorf("serializing step metadata: %w", err)
	}
	return string(out), nil
}

// DecodeMetadataBag parses one opaque KV bag, refusing anything that is not a
// JSON OBJECT.
//
// The object requirement is the one shape core insists on, and it is not core
// reading the bag: a KV bag with no keys has nothing to merge and nothing for
// the rollup to group by. db.MetadataRollup already SKIPS a non-object row at
// read time, and that tolerance exists for rows written before any writer
// validated them (R10: a read verb that refused because one row held odd bytes
// would be useless during exactly the run an operator wants to inspect). It is
// not a licence to write such rows now — refusing at the write end is what
// keeps the tolerance from becoming the norm.
//
// Exported so internal/cli's `vote cast --metadata` (vote_cast.go) can decode
// through the SAME "is this a JSON object" rule rather than a second copy of
// it — the two differ only in what wraps this decode (the vote's own size cap
// against `db.VoteMetadataMaxBytes`, checked by the caller on the returned
// bag).
func DecodeMetadataBag(raw, label string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}

	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, validationErr("%s is not valid JSON: %v", label, err)
	}

	// A JSON `null` decodes without error and is not an object. It is called
	// out here rather than falling through the type switch so the message names
	// the actual shape instead of "<nil>".
	if probe == nil {
		return nil, validationErr(
			"%s must be a JSON object of keys to values, not null", label)
	}

	bag, ok := probe.(map[string]any)
	if !ok {
		return nil, validationErr(
			"%s must be a JSON object of keys to values, got %s", label, jsonShape(probe))
	}
	return bag, nil
}

// jsonShape names a decoded JSON value's shape for an error message. It reports
// the SHAPE and never the content: a refusal should not echo a bag back to a
// worker that may have put something large or sensitive in it.
func jsonShape(v any) string {
	switch v.(type) {
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	default:
		return "a non-object value"
	}
}

// validateMetadataSize is `complete`'s cap check (MetadataMaxBytes), measured
// on the RAW INCOMING TEXT — before parsing and before the merge.
//
// The bytes the caller supplied are the number the caller can act on. Measuring
// the MERGED result would make a refusal depend on what the definition bag
// already held, so an unchanged command could start failing because a workflow
// author edited an unrelated file.
//
// The message names BOTH numbers and a real remedy, in db.ArtifactMaxBytes's
// phrasing style: a worker with genuinely large output has two purpose-built
// channels, and the error points at them rather than merely refusing the work.
func validateMetadataSize(raw string) error {
	return validateMetadataSizeWithRemedy(raw, "the artifact or the payload")
}

// validateFailMetadataSize is `fail`'s cap check, the same MetadataMaxBytes
// measured the same way — `fail` shares the bag's size limit, not just its
// shape. The remedy differs because the channels differ: `fail`
// offers no `--artifact-file` or `--payload-file`, so pointing at either
// would send an operator looking for a flag `step fail` does not have. The
// note is the one bulk-detail channel this verb actually offers.
func validateFailMetadataSize(raw string) error {
	return validateMetadataSizeWithRemedy(raw, "the note")
}

// validateClaimMetadataSize is `claim`'s cap check, the same MetadataMaxBytes
// measured the same way (DKT-592). The remedy differs again because the
// channels do: a claim records nothing but the bag, so the only place bulk
// detail can go is the completion this claim is the start of — and naming
// `--artifact-file` outright would send a dispatcher looking for a flag `step
// claim` does not have.
func validateClaimMetadataSize(raw string) error {
	return validateMetadataSizeWithRemedy(raw, "the artifact or the payload at completion")
}

// validateMetadataSizeWithRemedy is the shared cap check the verbs' size
// validators call — one size limit, one message shape, remedy text supplied
// by the caller because only the caller knows which channels its verb offers.
func validateMetadataSizeWithRemedy(raw, remedy string) error {
	if len(raw) > MetadataMaxBytes {
		return validationErr(
			"metadata is %d bytes, over the %d-byte cap; record the detail in %s instead",
			len(raw), MetadataMaxBytes, remedy)
	}
	return nil
}
