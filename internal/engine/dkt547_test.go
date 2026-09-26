package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-547 — the `issue.linked.<relation>.<kind>` input form: a step consuming
// an artifact recorded under ANOTHER issue, reached through the consuming
// issue's declared relations, pinned at activation, and enforced loudly.
//
// The incident: ui-change@12 claimed its changes were "bound to an accepted
// ux-spec" produced by a spec-doc run on a different issue, and no input form
// reached it — every legal form is same-run and same-issue — so the spec
// reached executors only when the issue body happened to cite it (33 design-qa
// instances across 3 runs, all relying on prose). These tests pin the whole
// contract: resolution through relations, activation-time pinning, the loud
// refusals when the relation or the artifact is missing, and the pin's
// immunity to artifacts recorded after activation.

// specDocMiniSrc is the producing side, minimized: one step on a `spike`
// issue that drafts and records the ux-spec.
const specDocMiniSrc = `
[pipeline]
name = "spec-doc-mini"
version = 1

[match]
kind = ["spike"]

[[step]]
name = "draft-spec"
after = []
executor = "author"
emits = "ux-spec"
inputs = ["issue.body"]
`

// uiChangeMiniSrc is the consuming side: design-qa on a `task` issue reads
// the accepted spec of the issue(s) this issue depends on. No step of this
// workflow produces `ux-spec` — the producer is another issue's run, which is
// exactly the relaxation V11 grants the form.
const uiChangeMiniSrc = `
[pipeline]
name = "ui-change-mini"
version = 1

[match]
kind = ["task"]

[[step]]
name = "design-qa"
after = []
executor = "qa"
emits = "qa-report"
inputs = ["issue.body", "issue.linked.depends_on.ux-spec"]
`

// blockedUiMiniSrc consumes through the INVERSE token: the spec issue BLOCKS
// this one, so the consumer reads its `blocked-by` issues' spec — the same
// binding declared from the relation's other end, hyphenated to prove the
// spelling normalizes.
const blockedUiMiniSrc = `
[pipeline]
name = "blocked-ui-mini"
version = 1

[match]
kind = ["chore"]

[[step]]
name = "design-qa-b"
after = []
executor = "qa"
emits = "qa-report"
inputs = ["issue.linked.blocked-by.ux-spec"]
`

const specDocV1 = "the accepted ux spec, v1"

// produceSpec drives a spec-doc-mini run over a fresh spike issue to done,
// leaving the issue holding one recorded `ux-spec` artifact, and returns the
// issue id.
func produceSpec(t *testing.T, conn *sql.DB, e *Engine, title, doc string) int {
	t.Helper()
	issue := createIssue(t, conn, title, "the spec request", "spike", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activating the spec run: %v", err)
	claimAndCompleteInRun(t, conn, e, run.ID, "draft-spec@0", doc)
	return issue
}

// claimAndCompleteInRun is claimAndComplete scoped to one run's instance
// namespace — these tests hold several runs whose step instances collide
// (every spec run has a `draft-spec@0`), so the global instance lookup the
// shared helper uses would be ambiguous.
func claimAndCompleteInRun(
	t *testing.T, conn *sql.DB, e *Engine, runID int, instance, artifact string,
) {
	t.Helper()
	var stepID int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND instance = ?`,
		runID, instance).Scan(&stepID)
	testsupport.Must(t, err, "finding %s in run %d: %v", instance, runID, err)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte(artifact), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
}

// linkIssues records one relation through the ordinary db path.
func linkIssues(t *testing.T, conn *sql.DB, source, target int, rt model.RelationType) {
	t.Helper()
	_, err := db.CreateRelation(conn, &model.Relation{
		SourceIssueID: source, TargetIssueID: target, RelationType: rt,
	})
	testsupport.Must(t, err, "linking %d -> %d (%s): %v", source, target, rt, err)
}

// specInputs filters a bundle's inputs down to the `ux-spec` entries.
func specInputs(inputs []ContextInput) []ContextInput {
	var out []ContextInput
	for _, in := range inputs {
		if in.Kind == "ux-spec" {
			out = append(out, in)
		}
	}
	return out
}

// TestLinkedInputPinsAndServesTheDoc is the happy path end to end: the spec
// run records the doc under its own issue; the consumer links `depends_on`
// and activates; design-qa's bundle carries the doc as a declared input, with
// the producer named as `<ISSUE>/<instance>` and the artifact id the spec
// run's ledger attributes to it.
func TestLinkedInputPinsAndServesTheDoc(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(specDocMiniSrc), "spec-doc-mini.toml")
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	specIssue := produceSpec(t, conn, e, "write the spec", specDocV1)

	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	linkIssues(t, conn, consumer, specIssue, model.RelationDependsOn)
	runB := startRun(t, conn, consumer)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activating the consumer run: %v", err)

	// The pin is in the issue snapshot — the same column every other frozen
	// fact of the issue lives in.
	var snapshot string
	err = conn.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runB.ID, consumer).Scan(&snapshot)
	testsupport.Must(t, err, "reading the consumer snapshot: %v", err)
	if !strings.Contains(snapshot, `"linked"`) ||
		!strings.Contains(snapshot, `"depends_on.ux-spec"`) {
		t.Errorf("the issue snapshot carries no linked pin: %s", snapshot)
	}

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "design-qa@0"), nowMS)
	testsupport.Must(t, err, "assembling design-qa@0's bundle: %v", err)

	specs := specInputs(bundle.Inputs)
	if len(specs) != 1 {
		t.Fatalf("design-qa@0 binds %d ux-spec inputs, want 1: %+v", len(specs), specs)
	}
	if specs[0].Body != specDocV1 {
		t.Errorf("the bound spec is %q, want %q", specs[0].Body, specDocV1)
	}
	wantProducer := model.FormatID(specIssue) + "/draft-spec@0"
	if specs[0].ProducerStep != wantProducer {
		t.Errorf("producer = %q, want %q — the cross-issue provenance",
			specs[0].ProducerStep, wantProducer)
	}
	var artifactID int
	err = conn.QueryRow(
		`SELECT a.id FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE s.issue_id = ? AND a.kind = 'ux-spec'`, specIssue).Scan(&artifactID)
	testsupport.Must(t, err, "reading the spec artifact id: %v", err)
	if want := fmt.Sprintf("ARTIFACT-%d", artifactID); specs[0].Artifact != want {
		t.Errorf("artifact = %q, want %q", specs[0].Artifact, want)
	}
}

// TestLinkedInputActivationFailsWithoutRelation is the first loud refusal: the
// workflow declares the binding and the issue never linked anything, so
// activation refuses inside the fat transaction and writes nothing — the
// binding is enforced rather than an issue-body convention.
func TestLinkedInputActivationFailsWithoutRelation(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	run := startRun(t, conn, consumer)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation succeeded with no depends_on relation to resolve")
	}
	for _, want := range []string{
		model.FormatID(consumer), "issue.linked.depends_on.ux-spec",
		"no depends_on relation", "docket issue link",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// The fat transaction rolled back whole: no steps, no snapshot.
	var steps int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE run_id = ?`, run.ID).Scan(&steps)
	testsupport.Must(t, err, "counting steps: %v", err)
	if steps != 0 {
		t.Errorf("the refused activation left %d steps behind", steps)
	}
}

// TestLinkedInputActivationFailsWithoutDoc is the second refusal: the relation
// exists but the linked issue has recorded no artifact of the kind — the spec
// issue's run has not produced it yet, so the consumer cannot bind and must
// not start.
func TestLinkedInputActivationFailsWithoutDoc(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	specIssue := createIssue(t, conn, "write the spec", "the spec request", "spike", nil)
	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	linkIssues(t, conn, consumer, specIssue, model.RelationDependsOn)
	run := startRun(t, conn, consumer)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation succeeded with no ux-spec artifact on the linked issue")
	}
	for _, want := range []string{
		model.FormatID(consumer), model.FormatID(specIssue),
		"issue.linked.depends_on.ux-spec",
		`has a recorded artifact of kind "ux-spec"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestLinkedInputPinsTheLatestAndOnlyTheLatest pins both halves of "latest,
// deterministically, at activation". Before activation, the linked issue holds
// two recorded spec artifacts and the pin takes the newest (highest id —
// latestPerProducer's own rule). After activation, a THIRD artifact lands on
// the linked issue and the bundle must not move: the pin froze at activation
// exactly as every other input does, so a mid-run spec revision cannot change
// what the run's steps were dispatched against.
func TestLinkedInputPinsTheLatestAndOnlyTheLatest(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(specDocMiniSrc), "spec-doc-mini.toml")
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	specIssue := produceSpec(t, conn, e, "write the spec", specDocV1)

	// A superseding emit from the same producer — the DKT-103 multi-emit
	// shape — leaves the issue holding v1 and v2, v2 newest.
	const specDocV2 = "the accepted ux spec, v2"
	recordExtraSpec(t, conn, specIssue, specDocV2)

	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	linkIssues(t, conn, consumer, specIssue, model.RelationDependsOn)
	runB := startRun(t, conn, consumer)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activating the consumer run: %v", err)

	// A revision recorded AFTER activation must not reach the bundle.
	recordExtraSpec(t, conn, specIssue, "the ux spec, revised mid-run")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "design-qa@0"), nowMS)
	testsupport.Must(t, err, "assembling design-qa@0's bundle: %v", err)

	specs := specInputs(bundle.Inputs)
	if len(specs) != 1 {
		t.Fatalf("design-qa@0 binds %d ux-spec inputs, want 1: %+v", len(specs), specs)
	}
	if specs[0].Body != specDocV2 {
		t.Errorf("the bound spec is %q, want the pre-activation latest %q",
			specs[0].Body, specDocV2)
	}
}

// recordExtraSpec records one more ux-spec artifact under the issue's done
// draft-spec step, through the artifact writer completion uses.
func recordExtraSpec(t *testing.T, conn *sql.DB, specIssue int, body string) {
	t.Helper()
	var stepID, runID int
	err := conn.QueryRow(
		`SELECT id, run_id FROM steps
		  WHERE issue_id = ? AND step_name = 'draft-spec' AND status = ?`,
		specIssue, db.StepDone).Scan(&stepID, &runID)
	testsupport.Must(t, err, "finding the done draft-spec step: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = db.InsertArtifactTx(tx, db.Artifact{
		RunID: runID, StepID: stepID, Kind: "ux-spec", Body: body,
		SHA256: workflow.SHA256([]byte(body)),
	}, nowMS)
	testsupport.Must(t, err, "recording the extra spec: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
}

// TestLinkedInputResolvesEveryLinkedIssueWithTheKind pins the multi-link
// rules. Relations are overloaded — `depends_on` orders scheduling as well as
// binding specs — so a consumer depending on two spec issues AND an
// implementation issue with no spec must bind both specs, in issue-id order,
// and must not refuse over the implementation issue.
func TestLinkedInputResolvesEveryLinkedIssueWithTheKind(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(specDocMiniSrc), "spec-doc-mini.toml")
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	specOne := produceSpec(t, conn, e, "spec one", "spec one's doc")
	specTwo := produceSpec(t, conn, e, "spec two", "spec two's doc")
	impl := createIssue(t, conn, "an implementation dependency", "code", "spike", nil)

	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	linkIssues(t, conn, consumer, specTwo, model.RelationDependsOn)
	linkIssues(t, conn, consumer, specOne, model.RelationDependsOn)
	linkIssues(t, conn, consumer, impl, model.RelationDependsOn)

	runB := startRun(t, conn, consumer)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activating the consumer run: %v", err)

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "design-qa@0"), nowMS)
	testsupport.Must(t, err, "assembling design-qa@0's bundle: %v", err)

	specs := specInputs(bundle.Inputs)
	if len(specs) != 2 {
		t.Fatalf("design-qa@0 binds %d ux-spec inputs, want 2: %+v", len(specs), specs)
	}
	// Issue-id order, not link-creation order: specOne was linked second and
	// still comes first, because the pin order is a pure function of the
	// store.
	if specs[0].Body != "spec one's doc" || specs[1].Body != "spec two's doc" {
		t.Errorf("bound specs out of issue order: %q then %q",
			specs[0].Body, specs[1].Body)
	}
}

// TestLinkedInputInverseRelationToken proves the form addresses BOTH
// directions of a relation: the spec issue `blocks` the consumer, and the
// consumer's `issue.linked.blocked-by.ux-spec` — the inverse token, in its
// hyphenated spelling — resolves through the relation's other end.
func TestLinkedInputInverseRelationToken(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(specDocMiniSrc), "spec-doc-mini.toml")
	registerSource(t, conn, []byte(blockedUiMiniSrc), "blocked-ui-mini.toml")

	specIssue := produceSpec(t, conn, e, "write the spec", specDocV1)

	consumer := createIssue(t, conn, "the blocked change", "the change", "chore", nil)
	linkIssues(t, conn, specIssue, consumer, model.RelationBlocks)

	runB := startRun(t, conn, consumer)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activating the consumer run: %v", err)

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "design-qa-b@0"), nowMS)
	testsupport.Must(t, err, "assembling design-qa-b@0's bundle: %v", err)

	specs := specInputs(bundle.Inputs)
	if len(specs) != 1 {
		t.Fatalf("design-qa-b@0 binds %d ux-spec inputs, want 1: %+v", len(specs), specs)
	}
	if specs[0].Body != specDocV1 {
		t.Errorf("the bound spec is %q, want %q", specs[0].Body, specDocV1)
	}
}

// TestLinkedInputPacketCarriesTheDoc is the happy path at the rendered layer —
// the packet a worker actually reads carries the spec's bytes, so the binding
// reaches the executor rather than stopping at the ledger.
func TestLinkedInputPacketCarriesTheDoc(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(specDocMiniSrc), "spec-doc-mini.toml")
	registerSource(t, conn, []byte(uiChangeMiniSrc), "ui-change-mini.toml")

	specIssue := produceSpec(t, conn, e, "write the spec", specDocV1)
	consumer := createIssue(t, conn, "apply the ui change", "the change", "task", nil)
	linkIssues(t, conn, consumer, specIssue, model.RelationDependsOn)
	runB := startRun(t, conn, consumer)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activating the consumer run: %v", err)

	packet, err := RenderStep(conn, stepIDByInstance(t, conn, "design-qa@0"), "", nowMS)
	testsupport.Must(t, err, "rendering design-qa@0: %v", err)

	if got := strings.Count(packet.Packet, specDocV1); got != 1 {
		t.Errorf("the spec is inlined %d times, want 1:\n%s", got, packet.Packet)
	}
}
