package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-491 — a rendered step packet inlined the same input artifact once per
// declared edge, so a six-lane review brief carried six byte-identical copies
// of one document.

// multiLaneDocSrc is the incident's shape, minimized: SIX when-gated authoring
// lanes that all emit one kind, a consuming step that declares one input entry
// per lane (it cannot know which lane an issue takes), and a fix-loop whose
// body re-emits that same kind.
//
// At ordinal 0 the six entries are honest — five lanes are `skipped` and
// resolve to nothing. At ordinal 1 loopProducerRedirect rebinds each stale lane
// to the loop body's emit of the SAME KIND (DKT-12), which is the one thing all
// six lanes share, so every entry lands on the ONE revised doc.
const multiLaneDocSrc = `
[pipeline]
name = "multi-lane-doc"
version = 1

[match]
kind = ["task"]

[[step]]
name = "author-a"
after = []
executor = "author-a"
when = "labels contains lane:a"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "author-b"
after = []
executor = "author-b"
when = "labels contains lane:b"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "author-c"
after = []
executor = "author-c"
when = "labels contains lane:c"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "author-d"
after = []
executor = "author-d"
when = "labels contains lane:d"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "author-e"
after = []
executor = "author-e"
when = "labels contains lane:e"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "author-f"
after = []
executor = "author-f"
when = "labels contains lane:f"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "review"
after = ["author-a", "author-b", "author-c", "author-d", "author-e", "author-f"]
fanout = ["judge-one", "judge-two"]
emits = "findings"
inputs = ["author-a.doc", "author-b.doc", "author-c.doc", "author-d.doc", "author-e.doc", "author-f.doc", "issue.body"]

[[step]]
name = "synthesize"
after = ["review"]
executor = "synthesize-findings"
emits = "findings"
inputs = ["review.*"]

[[step]]
name = "reconcile"
after = ["synthesize"]
action = "aggregate"
params = { field = "severity", method = "max", hold_spread = 2, output = "findings" }
inputs = ["synthesize.findings"]
payload = "findings@1"
threshold = { "fix-loop" = "any(severity >= blocker)" }
max_fix_loops = 2

[[step]]
name = "revise-a"
executor = "author-a"
emits = "doc"
loop = true
inputs = ["reconcile.findings", "author-a.doc"]
after_loop = "review"
`

// The two documents the lane records. Each carries a marker no other section of
// the packet can produce, so an assertion counts THE ARTIFACT rather than a
// header that happens to read alike.
const (
	authoredDoc = "AUTHORED-DOC-MARKER: the first draft of the document."
	revisedDoc  = "REVISED-DOC-MARKER: the revised draft, one blocker addressed."
)

// blockerPayload routes `fix-loop` through the fixture schema's ordered
// `severity`. One cluster, so the aggregate's spread is 0 and no held step
// materializes.
const blockerPayload = `[{"id":"C-1","severity":"blocker"}]`

// activateMultiLaneDoc registers the schema the aggregate needs, the multi-lane
// definition, and activates a run over one issue on lane A.
func activateMultiLaneDoc(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(multiLaneDocSrc), "multi-lane-doc.toml")
	issue := createIssue(t, conn, "write the doc", "the issue body", "task",
		[]string{"lane:a"})
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// driveMultiLaneRoundZero completes lane A, both judges, and the synthesis with
// a blocker, then runs the aggregate — which routes `fix-loop` and instantiates
// `revise-a@1` beside the re-entered `review@1` chain.
func driveMultiLaneRoundZero(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "author-a@0", authoredDoc, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@0")
}

// inputHeaders returns the packet's `== INPUT` section headers, in order. The
// `== INPUT PAYLOAD` lines are excluded: a payload is the same input's
// structured half, not a second inlining of it.
func inputHeaders(packet string) []string {
	var out []string
	for _, line := range strings.Split(packet, "\n") {
		if !strings.HasPrefix(line, "== INPUT ") ||
			strings.HasPrefix(line, "== INPUT PAYLOAD ") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// TestReReviewPacketInlinesTheRevisedDocOnce is DKT-491's exact incident: a
// re-review packet whose six per-lane `<author>.doc` entries all redirect onto
// the one revised doc must inline that doc ONCE, and must still carry every
// other input exactly once.
func TestReReviewPacketInlinesTheRevisedDocOnce(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateMultiLaneDoc(t, conn)
	driveMultiLaneRoundZero(t, conn, e)

	if !stepExists(t, conn, "revise-a@1") {
		t.Fatalf("premise: the blocker did not enter the fix loop; revise-a@1 " +
			"was never instantiated")
	}
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise-a@1", revisedDoc, "")

	packet, err := RenderStep(conn, stepIDByInstance(t, conn, "review@1#0"), "", nowMS)
	testsupport.Must(t, err, "rendering review@1#0: %v", err)

	// The artifact itself, counted by its own bytes rather than by a header.
	if got := strings.Count(packet.Packet, revisedDoc); got != 1 {
		t.Errorf("the revised doc is inlined %d times, want 1 — the packet is "+
			"%d bytes:\n%s", got, len(packet.Packet), packet.Packet)
	}

	// And every input the step legitimately carries, still there and still
	// once: the doc, the issue body, and the previous round's three findings
	// sets from review's own lineage (two judges, the reconciliation the body
	// acted on — the synthesis between them is neither, DKT-1055).
	want := []string{
		"== INPUT doc from revise-a@1",
		"== INPUT issue.body",
		"== INPUT findings from review@0#0",
		"== INPUT findings from review@0#1",
		"== INPUT findings from reconcile@0",
	}
	headers := inputHeaders(packet.Packet)
	if len(headers) != len(want) {
		t.Fatalf("the packet carries %d input blocks, want %d:\n%v",
			len(headers), len(want), headers)
	}
	for _, header := range want {
		if n := countStrings(headers, header); n != 1 {
			t.Errorf("%q appears %d times among the input blocks, want 1:\n%v",
				header, n, headers)
		}
	}
}

// TestReReviewBundleBindsTheRevisedDocOnce is the same fact one layer down: the
// §11.4 bundle `step context` serves carries one entry per DISTINCT artifact,
// so a dispatcher reading the bundle pays the duplicate no more than a rendered
// packet does. Its `--meta` byte count is measured off this list, and a bundle
// deduplicated only at render would keep reporting the inflated figure.
func TestReReviewBundleBindsTheRevisedDocOnce(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateMultiLaneDoc(t, conn)
	driveMultiLaneRoundZero(t, conn, e)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise-a@1", revisedDoc, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@1#0"), nowMS)
	testsupport.Must(t, err, "assembling review@1#0's bundle: %v", err)

	var docs []string
	for _, in := range bundle.Inputs {
		if in.Kind != "doc" {
			continue
		}
		docs = append(docs, in.Artifact+" from "+in.ProducerStep)
		if in.Body != revisedDoc {
			t.Errorf("the bound doc is %q, want the revised draft", in.Body)
		}
	}
	if len(docs) != 1 {
		t.Errorf("the bundle binds %d doc inputs, want 1: %v", len(docs), docs)
	}
	if len(bundle.Inputs) != 5 {
		t.Errorf("the bundle carries %d inputs, want 5 (the doc, the issue "+
			"body, and the previous round's three findings sets from review's "+
			"own lineage — two judges and the reconciliation, DKT-1055)",
			len(bundle.Inputs))
	}
}

// TestFirstRoundBundleIsUnchanged is the falsification: the collapse must not
// touch a bundle that never had a duplicate. At ordinal 0 the five lanes an
// issue did not take are `skipped` and resolve to nothing, so `review@0` binds
// the one authored doc and the issue body — exactly as it did before, and with
// the authored draft rather than any later one.
func TestFirstRoundBundleIsUnchanged(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateMultiLaneDoc(t, conn)
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "author-a@0", authoredDoc, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@0#0"), nowMS)
	testsupport.Must(t, err, "assembling review@0#0's bundle: %v", err)

	if len(bundle.Inputs) != 2 {
		t.Fatalf("review@0#0 binds %d inputs, want the doc and the issue body: %+v",
			len(bundle.Inputs), bundle.Inputs)
	}
	if bundle.Inputs[0].ProducerStep != "author-a@0" ||
		bundle.Inputs[0].Body != authoredDoc {
		t.Errorf("the first input is %q from %q, want the authored doc from "+
			"author-a@0", bundle.Inputs[0].Body, bundle.Inputs[0].ProducerStep)
	}
	if bundle.Inputs[1].Kind != inputIssueBody {
		t.Errorf("the second input is %q, want %q — declared order is the "+
			"author's order", bundle.Inputs[1].Kind, inputIssueBody)
	}
}

// TestDedupeInputsKeepsDistinctArtifactsWithOneName is the unit-level guard on
// the identity rule: the engine-produced forms share ONE `artifact` name across
// producers, so a key of `artifact` alone would collapse two different steps'
// gate results into one and silently drop a producer from the packet.
func TestDedupeInputsKeepsDistinctArtifactsWithOneName(t *testing.T) {
	inputs := []ContextInput{
		{Artifact: inputGateResults, Kind: inputGateResults, ProducerStep: "build@0", Body: "[]"},
		{Artifact: inputGateResults, Kind: inputGateResults, ProducerStep: "test@0", Body: "[]"},
		{Artifact: "ARTIFACT-7", Kind: "doc", ProducerStep: "revise-a@1", Body: "d"},
		{Artifact: "ARTIFACT-7", Kind: "doc", ProducerStep: "revise-a@1", Body: "d"},
	}
	got := dedupeInputs(inputs)
	if len(got) != 3 {
		t.Fatalf("dedupeInputs kept %d inputs, want 3: %+v", len(got), got)
	}
	if got[0].ProducerStep != "build@0" || got[1].ProducerStep != "test@0" {
		t.Errorf("two producers' gate results collapsed into one: %+v", got)
	}

	// And a list with nothing repeated is handed back untouched.
	distinct := inputs[:3]
	if kept := dedupeInputs(distinct); len(kept) != 3 {
		t.Errorf("dedupeInputs dropped an entry from a duplicate-free list: %+v", kept)
	}
}

// countStrings counts exact matches of want in got.
func countStrings(got []string, want string) int {
	n := 0
	for _, s := range got {
		if s == want {
			n++
		}
	}
	return n
}
