package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-591 — a `<step>.<kind>` input naming a SKIPPED step resolved to a
// different step's artifact of the same kind. RUN-39's spec-doc review
// declared six per-lane `<author>.doc` inputs; five lanes were skipped, one
// revise body ran, and loopProducerRedirect's kind-only scan handed that one
// body's doc to EVERY entry — six byte-identical copies, five of them from a
// step none of those inputs named.
//
// The remedy: `<step>.<kind>` resolves to the named step's own artifact (or
// its ordinal/loop substitute when that substitute is UNAMBIGUOUS in the
// definition) or to nothing. With several same-kind loop bodies re-entering
// the same consumer, the redirect refuses to guess.

// multiReviseDocSrc is RUN-39's shape, minimized to the incident's mechanics:
// six when-gated authoring lanes that all emit `doc`, a review step declaring
// one input entry per lane, and ONE `loop = true` revise body PER LANE — all
// emitting `doc`, all re-entering `review`. Only the issue's own lane ever
// runs; the other five authors are `skipped` and their revise bodies idle.
const multiReviseDocSrc = `
[pipeline]
name = "multi-revise-doc"
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
inputs = ["reconcile.findings"]
after_loop = "review"

[[step]]
name = "revise-b"
executor = "author-b"
emits = "doc"
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"

[[step]]
name = "revise-c"
executor = "author-c"
emits = "doc"
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"

[[step]]
name = "revise-d"
executor = "author-d"
emits = "doc"
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"

[[step]]
name = "revise-e"
executor = "author-e"
emits = "doc"
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"

[[step]]
name = "revise-f"
executor = "author-f"
emits = "doc"
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"
`

// activateMultiReviseDoc registers the schema the aggregate needs, the
// multi-revise definition, and activates a run over one issue on lane A.
func activateMultiReviseDoc(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(multiReviseDocSrc), "multi-revise-doc.toml")
	issue := createIssue(t, conn, "write the doc", "the issue body", "task",
		[]string{"lane:a"})
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// TestSkippedProducerInputDoesNotBindAnotherBodysDoc is RUN-39 verbatim: on
// re-entry, one revise body has run and emitted `doc` at this ordinal, and the
// five inputs naming SKIPPED authors must resolve to NOTHING — not to that
// body's artifact, which none of them named. The one input whose author
// actually authored keeps the author's own draft: with six same-kind bodies
// re-entering `review`, no redirect target is identifiable, so ordinalScoped's
// per-input fallback stands.
func TestSkippedProducerInputDoesNotBindAnotherBodysDoc(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateMultiReviseDoc(t, conn)

	// Round 0: lane A authors, both judges review, the synthesis carries a
	// blocker, and the aggregate routes `fix-loop`.
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "author-a@0", authoredDoc, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@0")

	if !stepExists(t, conn, "review@1#0") {
		t.Fatalf("premise: the blocker did not enter the fix loop; review@1 " +
			"was never instantiated")
	}
	// The incident's asymmetry: of the six bodies instantiated at ordinal 1,
	// exactly ONE runs — and it is NOT lane A's, so no declared input names it.
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "revise-d@1", revisedDoc, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@1#0"), nowMS)
	testsupport.Must(t, err, "assembling review@1#0's bundle: %v", err)

	var docs []string
	for _, in := range bundle.Inputs {
		if in.ProducerStep == "revise-d@1" {
			t.Errorf("an input bound revise-d@1's %s artifact — no declared "+
				"input names revise-d, and a kind-only redirect must not "+
				"guess between six same-kind bodies", in.Kind)
		}
		if strings.Contains(in.Body, revisedDoc) {
			t.Errorf("input %q from %q carries the unrelated body's revised "+
				"doc", in.Artifact, in.ProducerStep)
		}
		if in.Kind == "doc" {
			docs = append(docs, in.ProducerStep)
		}
	}
	// The five skipped lanes resolve to nothing, so exactly ONE doc input
	// survives: lane A's own draft, bound by §7.4's per-input fallback.
	if len(docs) != 1 || docs[0] != "author-a@0" {
		t.Errorf("the bundle binds doc inputs from %v, want exactly "+
			"[author-a@0] — a skipped author's input resolves to NOTHING",
			docs)
	}
}

// TestClusterScopedBodiesStillRedirect pins what DKT-591's refusal must NOT
// take down: two same-kind bodies whose serves-scoped clusters re-enter
// DISJOINT chains (DKT-544's shape) are unambiguous for any one consumer —
// only one body's `after_loop` chain contains it — so the re-entered gate
// still reads its own cluster's revision, never the original draft.
func TestClusterScopedBodiesStillRedirect(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateInterposed(t, conn, clusterSrc)

	claimAndComplete(t, conn, e, "draft@0", authoredDoc, "")
	claimAndComplete(t, conn, e, "prd-gate@0", "findings", blockedPayload)

	if !stepExists(t, conn, "prd-fix@1") {
		t.Fatalf("premise: cluster A's entry did not instantiate prd-fix@1")
	}
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "prd-fix@1", revisedDoc, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "prd-gate@1"), nowMS)
	testsupport.Must(t, err, "assembling prd-gate@1's bundle: %v", err)

	var sawDoc bool
	for _, in := range bundle.Inputs {
		if in.Kind != "doc" {
			continue
		}
		sawDoc = true
		if in.ProducerStep != "prd-fix@1" || in.Body != revisedDoc {
			t.Errorf("prd-gate@1's draft.doc bound %q from %q, want the "+
				"revised doc from prd-fix@1 — with disjoint clusters the "+
				"redirect target is unambiguous and must still fire",
				in.Body, in.ProducerStep)
		}
	}
	if !sawDoc {
		t.Error("prd-gate@1 bound no doc input at all; the unambiguous " +
			"cluster redirect must not have been refused")
	}
}
