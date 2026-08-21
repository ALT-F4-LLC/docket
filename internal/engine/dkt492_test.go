package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-492 — a revision step's doc input resolved to the original author's
// draft, not the latest revision: RUN-39's `revise@2` packet carried the
// round-0 author document while every finding beside it cited the round-1
// revision, so revising the document the packet supplied would have discarded
// the round it was entered to build on.
//
// The asymmetry that looked like an engine bug is structural, and these tests
// pin both halves. A step downstream of `after_loop` (`review`) re-runs at
// ordinal k AFTER the loop body emitted at k, so loopProducerRedirect rebinds
// its stale inputs; the loop BODY (`revise`) can never be in that set — a
// `loop = true` step declares no `after` (V10/V18) and the set is the `after`
// closure of the `after_loop` target — and at the moment its own inputs
// resolve, no ordinal-k body artifact exists yet. Entry count never mattered.
// A blanket redirect for the body would break §7.4's pinned
// `fix@1 -> implement@0` case (TestOrdinalScopedInputBindingFallsBackPerInput),
// so the live document is reached by declaring it: `issue.latest.<kind>`, the
// issue's latest recorded round of one kind, whoever produced it.

// docReviseLoopSrc is spec-doc's revise loop, minimized: an author drafts the
// doc, a review fanout critiques it, an aggregate routes `fix-loop` on
// blockers, and the loop-body `revise` step rewrites the doc itself. Its doc
// input is the DKT-492 form — the live document, not a named producer's.
const docReviseLoopSrc = `
[pipeline]
name = "doc-revise-loop"
version = 1

[match]
kind = ["task"]

[[step]]
name = "author"
after = []
executor = "author"
emits = "doc"
inputs = ["issue.body"]

[[step]]
name = "review"
after = ["author"]
fanout = ["judge-one", "judge-two"]
emits = "findings"
inputs = ["author.doc", "issue.body"]

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
name = "revise"
executor = "author"
emits = "doc"
loop = true
inputs = ["reconcile.findings", "issue.latest.doc"]
after_loop = "review"
`

// activateDocReviseLoop registers the fixture schema and the definition —
// through the same parse-validate-lint path `workflow register` uses, so the
// `issue.latest.doc` declaration is proven registrable — and activates a run
// over one issue.
func activateDocReviseLoop(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(docReviseLoopSrc), "doc-revise-loop.toml")
	issue := createIssue(t, conn, "write the doc", "the issue body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// driveDocReviseRoundZero completes the author, both judges, and the synthesis
// with a blocker, then runs the aggregate — which routes `fix-loop` and
// instantiates `revise@1` beside the re-entered `review@1` chain.
func driveDocReviseRoundZero(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "author@0", authoredDoc, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@0")
}

// driveDocReviseRoundOne records the revision, re-reviews it with the blocker
// still open, and reconciles — entering the loop a second time, which
// instantiates `revise@2`. The stub tree is moved per round (driveFixtureRound)
// so DKT-340's non-convergence guard sees rounds that changed something.
func driveDocReviseRoundOne(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise@1", revisedDoc, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@1#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@1", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@1")
}

// docInputs filters a bundle's inputs down to the `doc` entries.
func docInputs(inputs []ContextInput) []ContextInput {
	var out []ContextInput
	for _, in := range inputs {
		if in.Kind == "doc" {
			out = append(out, in)
		}
	}
	return out
}

// docArtifactIDOf reads the id of the one artifact a producer instance
// recorded of a kind — the identity the run report prints, so a test asserting
// against it is asserting what an operator auditing the run would check.
func docArtifactIDOf(t *testing.T, conn *sql.DB, instance, kind string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT a.id FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE s.instance = ? AND a.kind = ?`, instance, kind).Scan(&id)
	testsupport.Must(t, err, "reading %s's %s artifact id: %v", instance, kind, err)
	return id
}

// TestReviseRoundOneBindsTheAuthoredDoc: at ordinal 1 nothing has revised the
// document yet, so `issue.latest.doc` resolves to the author's draft — the
// same artifact a declared `author.doc` would have bound, which is what makes
// the form a drop-in replacement for the first round.
func TestReviseRoundOneBindsTheAuthoredDoc(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateDocReviseLoop(t, conn)
	driveDocReviseRoundZero(t, conn, e)

	if !stepExists(t, conn, "revise@1") {
		t.Fatalf("premise: the blocker did not enter the fix loop; revise@1 " +
			"was never instantiated")
	}

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "revise@1"), nowMS)
	testsupport.Must(t, err, "assembling revise@1's bundle: %v", err)

	docs := docInputs(bundle.Inputs)
	if len(docs) != 1 {
		t.Fatalf("revise@1 binds %d doc inputs, want 1: %+v", len(docs), docs)
	}
	if docs[0].ProducerStep != "author@0" || docs[0].Body != authoredDoc {
		t.Errorf("revise@1's doc input is %q from %q, want the authored draft "+
			"from author@0", docs[0].Body, docs[0].ProducerStep)
	}
}

// TestReviseRoundTwoBindsTheLatestRevision is DKT-492's exact incident,
// inverted into the fix: on the SECOND loop entry the revise step's doc input
// must be the round-1 revision — the artifact the findings beside it were
// written against — never the round-0 author draft it would discard. The
// binding is verified against the artifact id the run's tables attribute to
// `revise@1`, the same identity `run report` prints.
func TestReviseRoundTwoBindsTheLatestRevision(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateDocReviseLoop(t, conn)
	driveDocReviseRoundZero(t, conn, e)
	driveDocReviseRoundOne(t, conn, e)

	if !stepExists(t, conn, "revise@2") {
		t.Fatalf("premise: the still-open blocker did not enter the fix loop " +
			"a second time; revise@2 was never instantiated")
	}

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "revise@2"), nowMS)
	testsupport.Must(t, err, "assembling revise@2's bundle: %v", err)

	docs := docInputs(bundle.Inputs)
	if len(docs) != 1 {
		t.Fatalf("revise@2 binds %d doc inputs, want 1: %+v", len(docs), docs)
	}
	if docs[0].ProducerStep != "revise@1" || docs[0].Body != revisedDoc {
		t.Errorf("revise@2's doc input is %q from %q, want the round-1 revision "+
			"from revise@1", docs[0].Body, docs[0].ProducerStep)
	}
	want := fmt.Sprintf("ARTIFACT-%d", docArtifactIDOf(t, conn, "revise@1", "doc"))
	if docs[0].Artifact != want {
		t.Errorf("revise@2's doc input is %s, want %s — the artifact the run "+
			"attributes to revise@1", docs[0].Artifact, want)
	}

	// And the round-0 draft is nowhere in the bundle: carrying both documents
	// is the ambiguity the form exists to remove.
	for _, in := range bundle.Inputs {
		if in.Body == authoredDoc {
			t.Errorf("revise@2's bundle still carries the round-0 author draft "+
				"as %s from %s", in.Artifact, in.ProducerStep)
		}
	}
}

// TestReviseRoundTwoPacketCarriesTheLatestRevision is the same fact at the
// rendered layer — the packet a worker actually reads. One doc block, from
// `revise@1`, holding the revision; the author draft absent; the reconciled
// findings bound fresh at ordinal 1 beside it.
func TestReviseRoundTwoPacketCarriesTheLatestRevision(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateDocReviseLoop(t, conn)
	driveDocReviseRoundZero(t, conn, e)
	driveDocReviseRoundOne(t, conn, e)

	packet, err := RenderStep(conn, stepIDByInstance(t, conn, "revise@2"), "", nowMS)
	testsupport.Must(t, err, "rendering revise@2: %v", err)

	if got := strings.Count(packet.Packet, revisedDoc); got != 1 {
		t.Errorf("the revision is inlined %d times, want 1:\n%s", got, packet.Packet)
	}
	if got := strings.Count(packet.Packet, authoredDoc); got != 0 {
		t.Errorf("the round-0 author draft is inlined %d times, want 0:\n%s",
			got, packet.Packet)
	}

	want := []string{
		"== INPUT findings from reconcile@1",
		"== INPUT doc from revise@1",
	}
	headers := inputHeaders(packet.Packet)
	if len(headers) != len(want) {
		t.Fatalf("the packet carries %d input blocks, want %d:\n%v",
			len(headers), len(want), headers)
	}
	for i, header := range want {
		if headers[i] != header {
			t.Errorf("input block %d is %q, want %q — declared order is the "+
				"author's order", i, headers[i], header)
		}
	}
}

// TestReviseRerenderExcludesItsOwnEmit pins the form's self-exclusion: a
// packet re-rendered AFTER the step completed must not grow the step's own
// output as an "input" it never saw. `revise@1`'s own revision is the newest
// doc at its ordinal once it lands, and `issue.latest.doc` must still answer
// with what the step actually read — the author's draft.
func TestReviseRerenderExcludesItsOwnEmit(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateDocReviseLoop(t, conn)
	driveDocReviseRoundZero(t, conn, e)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise@1", revisedDoc, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "revise@1"), nowMS)
	testsupport.Must(t, err, "re-assembling revise@1's bundle: %v", err)

	docs := docInputs(bundle.Inputs)
	if len(docs) != 1 {
		t.Fatalf("revise@1 re-renders %d doc inputs, want 1: %+v", len(docs), docs)
	}
	if docs[0].ProducerStep != "author@0" || docs[0].Body != authoredDoc {
		t.Errorf("revise@1's re-rendered doc input is from %q, want author@0 — "+
			"a step's own emit is never its input", docs[0].ProducerStep)
	}
}

// TestLoopBodyDeclaredInputStillBindsTheNamedProducer pins the OTHER half of
// the acceptance: a loop body's declared `<author>.<kind>` input is untouched
// by DKT-492 and still binds the named producer's ordinal-0 artifact per
// §7.4's fallback — the documented behavior `fix`'s `implement.change-summary`
// depends on (DKT-12 deliberately excludes the body from the redirect). The
// multi-lane DKT-491 fixture declares exactly that shape on `revise-a`, so a
// second loop entry is where a blanket redirect would show up as a regression.
func TestLoopBodyDeclaredInputStillBindsTheNamedProducer(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateMultiLaneDoc(t, conn)
	driveMultiLaneRoundZero(t, conn, e)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise-a@1", revisedDoc, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@1#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@1", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@1")

	if !stepExists(t, conn, "revise-a@2") {
		t.Fatalf("premise: the still-open blocker did not enter the fix loop " +
			"a second time; revise-a@2 was never instantiated")
	}

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "revise-a@2"), nowMS)
	testsupport.Must(t, err, "assembling revise-a@2's bundle: %v", err)

	docs := docInputs(bundle.Inputs)
	if len(docs) != 1 {
		t.Fatalf("revise-a@2 binds %d doc inputs, want 1: %+v", len(docs), docs)
	}
	if docs[0].ProducerStep != "author-a@0" || docs[0].Body != authoredDoc {
		t.Errorf("revise-a@2's declared author-a.doc bound %q from %q, want the "+
			"authored draft from author-a@0 — a declared producer name resolves "+
			"per §7.4 and never redirects for the loop body", docs[0].Body,
			docs[0].ProducerStep)
	}
}
