package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Input payloads reach the packet (docs/tdd/input-payloads.md).
//
// An artifact has two halves: `body` is prose, `payload` is the structured half
// validated at completion and stored in its own column. Both were recorded and
// resolved; only one reached the packet, because ContextInput had no payload
// field. A synthesize-class step whose contract requires clusters to carry
// their input severities unchanged received prose with the structure stripped,
// and the executor recovered it by opening a copy of the database directly —
// voiding the pinning, the reproducibility, and §6.6's no-live-state rule that
// the packet exists to provide.

// producedWithPayload records an artifact carrying both halves and marks its
// producer done, which is what makes it resolvable as an input.
func producedWithPayload(
	t *testing.T, conn *sql.DB, runID, stepID int, kind, body, payload string,
) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	_, err = db.InsertArtifactTx(tx, db.Artifact{
		RunID: runID, StepID: stepID, Kind: kind, Body: body, Payload: payload,
	}, nowMS)
	testsupport.Must(t, err, "inserting artifact: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "commit: %v", err)
	execSQL(t, conn, `UPDATE steps SET status = 'done' WHERE id = ?`, stepID)
}

// TestInputCarriesItsPayload is the headline: the structured half arrives, and
// arrives BYTE-IDENTICAL to what was recorded.
//
// Verbatim is the whole contract. Core validated the shape once, at completion;
// re-encoding or re-validating here would let assembly reject an artifact the
// engine already accepted.
func TestInputCarriesItsPayload(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	const payload = `[{"severity":"high","note":"a \"quoted\" finding"}]`
	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID,
		"change-summary", "the prose half", payload)

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	var found bool
	for _, in := range bundle.Inputs {
		if in.Kind != "change-summary" {
			continue
		}
		found = true
		if in.Payload != payload {
			t.Errorf("payload = %q, want %q byte-identical — core carries the "+
				"bytes and attaches no meaning to the keys", in.Payload, payload)
		}
		if in.Body != "the prose half" {
			t.Errorf("body = %q; the two halves must arrive SEPARATELY, not "+
				"concatenated", in.Body)
		}
	}
	if !found {
		t.Fatalf("no change-summary input resolved; inputs = %d",
			len(bundle.Inputs))
	}
}

// TestInputWithoutPayloadOmitsTheKey pins the compatibility half: an input with
// no payload serializes exactly as it did before this field existed, so every
// workflow that does not use payloads is untouched.
func TestInputWithoutPayloadOmitsTheKey(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "prose only", "")

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	encoded := jsonString(bundle)
	if strings.Contains(encoded, `"payload"`) {
		t.Errorf("a payload-less bundle serialized a `payload` key: %s — "+
			"omitempty is what keeps existing goldens byte-identical", encoded)
	}
}

// TestPayloadLessPacketIsUnchanged is the regression that protects every
// existing workflow: the RENDERED packet for a payload-less run must be
// byte-identical to what it was before payloads were carried.
//
// It renders the same step twice — once with a payload recorded on its input,
// once without — and requires the payload-less render to contain no trace of
// the payload machinery.
func TestPayloadLessPacketIsUnchanged(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "prose only", "")

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	packet, err := RenderStep(conn, reviewID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)

	// No empty section, no stray header, no blank line where a payload would
	// have gone. A conditional that renders its wrapper unconditionally is the
	// most likely way this breaks.
	if strings.Contains(packet.Packet, "PAYLOAD") {
		t.Errorf("a payload-less packet carries a PAYLOAD section:\n%s",
			packet.Packet)
	}
}

// TestPacketCarriesThePayload is the other side: when a payload exists, the
// default template renders it, and the executor never needs a second call.
func TestPacketCarriesThePayload(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	const payload = `[{"severity":"blocker"}]`
	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "prose", payload)

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	packet, err := RenderStep(conn, reviewID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if !strings.Contains(packet.Packet, payload) {
		t.Errorf("the rendered packet omits the payload:\n%s", packet.Packet)
	}
}

// TestPayloadSurvivesConcatenationHazards falsifies the rejected alternative
// (B) rather than only rejecting it in prose: a payload holding the very text
// that delimits a section must arrive intact and must not corrupt the body.
func TestPayloadSurvivesConcatenationHazards(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	const payload = "{\"note\":\"== INPUT change-summary\\nnot a real header\"}"
	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "real prose", payload)

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	for _, in := range bundle.Inputs {
		if in.Kind != "change-summary" {
			continue
		}
		if in.Payload != payload {
			t.Errorf("payload = %q, want %q", in.Payload, payload)
		}
		if in.Body != "real prose" {
			t.Errorf("body = %q, want %q — the halves stay separate", in.Body,
				"real prose")
		}
	}
}

// TestInputsBytesCountsPayloads pins §1.2: the context cap must see payload
// bytes, or a bundle could exceed its declared cap while reporting that it did
// not. The cap's honesty is the reason the field exists.
func TestInputsBytesCountsPayloads(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	payload := strings.Repeat("x", 512)
	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "prose", payload)

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	meta := bundle.Meta()
	if meta.InputsBytes < len(payload) {
		t.Errorf("inputs_bytes = %d, which does not account for a %d-byte "+
			"payload — the cap would be computed against a lie",
			meta.InputsBytes, len(payload))
	}
}

// TestPayloadTracksItsArtifactExactlyAsTheBodyDoes pins that the new field is
// governed by the SAME rule as the half beside it, which is the property that
// matters here.
//
// §6.6's "assembly reads no live state" is about ISSUE and TREE state — the
// title, body, labels, scope, and pinned files, all snapshotted at activation
// and covered by TestContextAssemblyReadsNoLiveState. Artifact rows are a
// different category: they are written once, at completion, and read back as
// the record of that event. `body` is re-read on every assembly, and `payload`
// must behave identically — an asymmetry between the two halves of one artifact
// would be the defect, not the re-read.
//
// So this asserts the invariant that is actually load-bearing: whatever the
// engine does with `body`, it does with `payload`.
func TestPayloadTracksItsArtifactExactlyAsTheBodyDoes(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	implID := stepIDByInstance(t, conn, "implement@0")
	producedWithPayload(t, conn, run.ID, implID, "change-summary", "prose",
		`[{"severity":"high"}]`)
	reviewID := stepIDByInstance(t, conn, "review@0#0")

	if !strings.Contains(bundleJSON(t, conn, reviewID), "severity") {
		t.Fatal("the bundle carries no payload to begin with")
	}

	// Edit BOTH halves and require the bundle to move on both or neither.
	execSQL(t, conn, `UPDATE artifacts SET body = ?, payload = ? WHERE step_id = ?`,
		"EDITED PROSE", `[{"severity":"EDITED"}]`, implID)

	after := bundleJSON(t, conn, reviewID)
	bodyMoved := strings.Contains(after, "EDITED PROSE")
	payloadMoved := strings.Contains(after, `severity\":\"EDITED`) ||
		strings.Contains(after, `"severity":"EDITED"`)

	if bodyMoved != payloadMoved {
		t.Errorf("the two halves of one artifact disagree: body moved = %v, "+
			"payload moved = %v — they must be governed by the same rule\n%s",
			bodyMoved, payloadMoved, after)
	}
}

// TestFanoutInputsEachCarryTheirOwnPayload is the case H-12 actually hit: a
// synthesize step consuming `review.*` gets every sibling's payload, each
// attributed to its own producer.
func TestFanoutInputsEachCarryTheirOwnPayload(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	// The fixture fans `review` out; give two siblings distinct payloads.
	first := stepIDByInstance(t, conn, "review@0#0")
	second := stepIDByInstance(t, conn, "review@0#1")
	producedWithPayload(t, conn, run.ID, first, "findings",
		"first prose", `[{"severity":"high"}]`)
	producedWithPayload(t, conn, run.ID, second, "findings",
		"second prose", `[{"severity":"low"}]`)

	synthID := stepIDByInstance(t, conn, "synthesize@0")
	bundle, err := ReadContext(conn, synthID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	seen := map[string]bool{}
	for _, in := range bundle.Inputs {
		if in.Payload != "" {
			seen[in.Payload] = true
		}
	}
	for _, want := range []string{`[{"severity":"high"}]`, `[{"severity":"low"}]`} {
		if !seen[want] {
			t.Errorf("the synthesize bundle is missing payload %s; it carries "+
				"%d input(s) — recovering these from the database is the path "+
				"this field exists to remove", want, len(bundle.Inputs))
		}
	}
}
