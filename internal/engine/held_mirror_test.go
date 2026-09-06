package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-1548 — RUN-90's shape: the operator corrected a held cluster's severity
// from `blocker` to `high`, and the reconcile step routed `fix-loop` anyway,
// because its threshold reads `open_severity` and only `severity` was
// corrected. The stale mirror kept `any(open_severity >= blocker)` true, so the
// engine scheduled the fix round the ruling had declined.

// mirrorSchemaSrc declares BOTH the aggregated field and the mirror the
// threshold reads, over the same order. `open_severity` is the producer's
// "maximum severity among open members" — derived from the same vocabulary,
// which is what makes a stale copy of it a routing decision rather than an
// unrelated field.
const mirrorSchemaSrc = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "mirror-findings@1",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      },
      "open_severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      }
    },
    "required": ["severity"],
    "additionalProperties": true
  }
}`

// mirrorWorkflowSrc is RUN-90's standard-change shape, minimized: an aggregate
// on `severity` whose two threshold arms both read `open_severity`.
const mirrorWorkflowSrc = `
[pipeline]
name = "mirror-change"
version = 1

[match]
kind = ["task"]

[[step]]
name = "synthesize"
after = []
executor = "synthesize-findings"
emits = "findings"
inputs = ["issue.body"]

[[step]]
name = "reconcile"
after = ["synthesize"]
action = "aggregate"
params = { field = "severity", method = "max", hold_spread = 3, output = "findings" }
inputs = ["synthesize.findings"]
payload = "mirror-findings@1"
threshold = { "fix-loop" = "any(open_severity >= blocker)", "drain-highs" = "any(open_severity >= high)" }
max_fix_loops = 2

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
inputs = ["reconcile.findings"]
after_loop = "synthesize"

[[step]]
name = "drain-highs"
after = ["reconcile"]
executor = "drain-highs"
emits = "findings"
inputs = ["reconcile.findings"]
`

// mirrorPayload is AGT-1223-C1: members {blocker, low}, so `max` reduces to
// `blocker` and the spread of 4 trips `hold_spread = 3`. `open_severity` is the
// producer's mirror of that same maximum.
const mirrorPayload = `[
  {"id":"AGT-1223-C1","severity":["blocker","low"],"open_severity":"blocker"}
]`

// driveMirrorReconcile registers the schema and definition, activates a run,
// and drives the aggregate to its hold.
func driveMirrorReconcile(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	registerSchemaFixture(t, conn, "mirror-findings", 1, mirrorSchemaSrc)
	registerSource(t, conn, []byte(mirrorWorkflowSrc), "mirror-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", mirrorPayload)
	driveAction(t, conn, e, "reconcile@0")
}

// TestCorrectedValueReachesEveryThresholdField is DKT-1548's headline: an
// operator's correction governs the routing it was made to govern. The
// aggregated field carried it already; the threshold's own field did not, and
// the threshold is what routes.
func TestCorrectedValueReachesEveryThresholdField(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	driveMirrorReconcile(t, conn, e)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStepValue(conn, held.ID, true,
		"Approve with follow up issue made for another run", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]

	if got, _ := resolved["severity"].(string); got != "high" {
		t.Errorf("severity = %q, want the operator's corrected value", got)
	}
	if got, _ := resolved["open_severity"].(string); got != "high" {
		t.Errorf("open_severity = %q, want %q — the threshold evaluates THIS "+
			"field, so a stale copy of the pre-correction value spends the "+
			"decision the correction was made to make", got, "high")
	}
	if got, _ := resolved[KeyOperatorSetFrom].(string); got != "blocker" {
		t.Errorf("operator_set_from = %q, want the computed value it replaced", got)
	}
}

// TestCorrectedValueRoutesTheHighArm is the mutant the issue names: re-running
// RUN-90's shape must route `drain-highs`, not `fix-loop`.
func TestCorrectedValueRoutesTheHighArm(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	driveMirrorReconcile(t, conn, e)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStepValue(conn, held.ID, true, "no fix round", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	routing := heldStep(t, conn, "reconcile@0")
	if strings.HasPrefix(routing.Routing, workflow.OnFailFixLoop) {
		t.Fatalf("reconcile@0 routed %q. RUN-90 exactly: the operator declined "+
			"a fix round by correcting `blocker` to `high`, and the engine "+
			"scheduled one anyway because `any(open_severity >= blocker)` was "+
			"read against the value the correction replaced.", routing.Routing)
	}
	if routing.Routing != "drain-highs" {
		t.Errorf("reconcile@0 routed %q, want %q — `high` reaches the second "+
			"arm and not the first", routing.Routing, "drain-highs")
	}
	if hasEventKind(t, conn, routing.ID, EventLoopEntered) {
		t.Error("the corrected cluster entered the fix loop")
	}
}

// TestUncorrectedHoldStillRoutesTheBlockerArm is the falsification: without a
// `--value` the mirror is untouched and the blocker arm still routes. A fix
// that rewrote the mirror unconditionally would pass the two tests above and
// break this one.
func TestUncorrectedHoldStillRoutesTheBlockerArm(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	driveMirrorReconcile(t, conn, e)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, true, "accepted the computed value", nowMS)
	testsupport.Must(t, err, "approve: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["open_severity"].(string); got != "blocker" {
		t.Errorf("open_severity = %q with no correction made, want %q untouched",
			got, "blocker")
	}

	routing := heldStep(t, conn, "reconcile@0")
	if !strings.HasPrefix(routing.Routing, workflow.OnFailFixLoop) {
		t.Errorf("reconcile@0 routed %q, want %q — nothing was corrected",
			routing.Routing, workflow.OnFailFixLoop)
	}
}

// TestCorrectionLeavesAnUnrelatedThresholdFieldAlone is the specificity guard.
// A threshold field that did NOT mirror the aggregated value is a different
// fact about the cluster, and the correction has no opinion about it.
func TestCorrectionLeavesAnUnrelatedThresholdFieldAlone(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSchemaFixture(t, conn, "mirror-findings", 1, mirrorSchemaSrc)
	registerSource(t, conn, []byte(mirrorWorkflowSrc), "mirror-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// `open_severity` here is `medium`: it never carried the computed
	// `blocker`, so it is not a mirror of it.
	const divergent = `[
	  {"id":"C-1","severity":["blocker","low"],"open_severity":"medium"}
	]`
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", divergent)
	driveAction(t, conn, e, "reconcile@0")

	held := heldStep(t, conn, "reconcile-held@0#0")
	err = e.DecideStepValue(conn, held.ID, true, "call it high", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["open_severity"].(string); got != "medium" {
		t.Errorf("open_severity = %q, want %q untouched — it did not carry the "+
			"value the correction replaced, so it is a different fact and the "+
			"correction has no opinion about it", got, "medium")
	}
}

// TestParkedHeldVoteCorrectionRoutesTheHighArm is RUN-90 END TO END, on the
// path the incident actually took: a hold panel REJECTED the cluster 3/3, the
// failed tally parked it for the operator, and the operator approved it at a
// corrected severity. The routing must follow the correction there too — the
// vote-minted hold reaches the same resolution code, and a fix that held only
// on the human-minted path would leave the reported incident unfixed.
func TestParkedHeldVoteCorrectionRoutesTheHighArm(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	// BEFORE the hold is driven: the kind is fixed when the hold is minted, so
	// a tally configured afterwards would leave a `human` step behind.
	configureHoldTally(t, conn, "panel", "alice,bob,carol")
	driveMirrorReconcile(t, conn, e)

	// The panel convenes on the next invocation, rejects, and the failed tally
	// parks the hold for the operator rather than routing the aggregate.
	nextRun(t, conn, e)
	held := heldStep(t, conn, "reconcile-held@0#0")
	setProposalStatus(t, conn,
		heldProposalID(t, conn, e, held.Instance), model.ProposalStatusRejected)
	nextRun(t, conn, e)

	err := e.DecideStepValue(conn, held.ID, true,
		"Approve with follow up issue made for another run", "high", nowMS)
	testsupport.Must(t, err, "approving the parked hold with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["open_severity"].(string); got != "high" {
		t.Errorf("open_severity = %q, want %q", got, "high")
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.Routing != "drain-highs" {
		t.Errorf("reconcile@0 routed %q, want %q — the conductor presented this "+
			"option as `no fix round; reconcile routes drain-highs`, reading the "+
			"threshold against the corrected value", routing.Routing, "drain-highs")
	}
}
