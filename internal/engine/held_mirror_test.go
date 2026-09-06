package engine

import (
	"database/sql"
	"slices"
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
// and drives the aggregate over mirrorPayload to its hold.
func driveMirrorReconcile(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	driveMirrorReconcileOver(t, conn, e, mirrorPayload)
}

// driveMirrorReconcileOver is driveMirrorReconcile over a caller's payload.
func driveMirrorReconcileOver(t *testing.T, conn *sql.DB, e *Engine, payload string) {
	t.Helper()
	registerSchemaFixture(t, conn, "mirror-findings", 1, mirrorSchemaSrc)
	registerSource(t, conn, []byte(mirrorWorkflowSrc), "mirror-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", payload)
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
	var mirrors []string
	for _, name := range resolved[KeyOperatorSetMirrors].([]any) {
		mirrors = append(mirrors, name.(string))
	}
	if !slices.Equal(mirrors, []string{"open_severity"}) {
		t.Errorf("operator_set_mirrors = %v, want [open_severity] — the keys core "+
			"wrote on the author's behalf are recorded beside the decision that "+
			"caused it, not left to be inferred", mirrors)
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

// TestProducerDemotedFromDoesNotSteerCorrection is DKT-1680: RUN-90's shape
// under `max` with a producer key literally named `demoted_from`. `max` never
// demotes, so the key can only be the producer's — and it must neither survive
// into the emitted element nor steer clusterTop, or the mirror equality fails
// and the declined fix round runs anyway.
func TestProducerDemotedFromDoesNotSteerCorrection(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	driveMirrorReconcileOver(t, conn, e, `[
	  {"id":"AGT-1223-C1","severity":["blocker","low"],"open_severity":"blocker","demoted_from":"medium"}
	]`)

	reconcileID := stepIDByInstance(t, conn, "reconcile@0")
	emitted := artifactPayloads(t, conn, reconcileID)[0][0]
	if got, present := emitted[KeyDemotedFrom]; present {
		t.Errorf("emitted demoted_from = %v under max, want the key absent — a "+
			"producer's value under a core-owned name reads downstream as core's "+
			"own demotion trail", got)
	}

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStepValue(conn, held.ID, true, "no fix round", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, reconcileID)
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["open_severity"].(string); got != "high" {
		t.Errorf("open_severity = %q, want %q — the mirror equality was keyed "+
			"on the producer's demoted_from instead of the cluster's real top", got, "high")
	}
	routing := heldStep(t, conn, "reconcile@0")
	if routing.Routing != "drain-highs" {
		t.Errorf("reconcile@0 routed %q, want %q — RUN-90 resurrected by a "+
			"producer-supplied demoted_from", routing.Routing, "drain-highs")
	}
}

// TestUncorrectedHoldStillRoutesTheBlockerArm characterizes the no-correction
// path: an approve without `--value` never enters the mirror code at all, so
// the mirror stays as the producer wrote it and the blocker arm still routes.
// The unconditional-rewrite mutant is killed by
// TestCorrectionLeavesAnUnrelatedThresholdFieldAlone, not by this test.
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
	if _, ok := resolved[KeyOperatorSetMirrors]; ok {
		t.Errorf("operator_set_mirrors = %v, want the key absent — no mirror was "+
			"rewritten, and an empty list would claim a rewrite happened",
			resolved[KeyOperatorSetMirrors])
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

// medianMirrorWorkflowSrc is mirrorWorkflowSrc reduced by `median` instead of
// `max`, so the cluster's computed value and its top member differ.
const medianMirrorWorkflowSrc = `
[pipeline]
name = "median-mirror-change"
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
params = { field = "severity", method = "median", hold_spread = 3, output = "findings" }
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

// TestCorrectedValueRoutesTheHighArmUnderMedian is DKT-1548 under a REDUCING
// method. `median` over {blocker, low} computes `low` while the producer's
// max-derived `open_severity` reads `blocker`, so a rewrite keyed on the
// computed value the correction replaced never fires and the reported incident
// survives. The cluster's top member is what such a mirror tracks.
func TestCorrectedValueRoutesTheHighArmUnderMedian(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSchemaFixture(t, conn, "mirror-findings", 1, mirrorSchemaSrc)
	registerSource(t, conn, []byte(medianMirrorWorkflowSrc), "median-mirror-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", mirrorPayload)
	driveAction(t, conn, e, "reconcile@0")

	held := heldStep(t, conn, "reconcile-held@0#0")
	err = e.DecideStepValue(conn, held.ID, true, "no fix round", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["open_severity"].(string); got != "high" {
		t.Errorf("open_severity = %q, want %q — the mirror tracks the cluster's "+
			"top member, which the correction displaced just as it displaced the "+
			"computed value", got, "high")
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.Routing != "drain-highs" {
		t.Errorf("reconcile@0 routed %q, want %q — RUN-90's symptom under a "+
			"reducing method", routing.Routing, "drain-highs")
	}
}

// minMirrorWorkflowSrc reduces by `min` and thresholds on `lowest_open`, a
// producer field whose declared meaning is the LOWEST open member.
const minMirrorWorkflowSrc = `
[pipeline]
name = "min-mirror-change"
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
params = { field = "severity", method = "min", hold_spread = 3, output = "findings" }
inputs = ["synthesize.findings"]
payload = "min-mirror-findings@1"
threshold = { "drain-lows" = "any(lowest_open >= high)" }
max_fix_loops = 2

[[step]]
name = "drain-lows"
after = ["reconcile"]
executor = "drain-highs"
emits = "findings"
inputs = ["reconcile.findings"]
`

const minMirrorSchemaSrc = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "min-mirror-findings@1",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      },
      "lowest_open": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      }
    },
    "required": ["severity"],
    "additionalProperties": true
  }
}`

// TestCorrectionLeavesACoincidentallyEqualFieldAlone is the false-positive
// guard. Under `min` the computed value coincides with an independent producer
// fact — the lowest open member — and sharing a value is not being a copy. Only
// a field holding the value the CLUSTER's top member carried is treated as a
// mirror of the corrected field.
func TestCorrectionLeavesACoincidentallyEqualFieldAlone(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSchemaFixture(t, conn, "min-mirror-findings", 1, minMirrorSchemaSrc)
	registerSource(t, conn, []byte(minMirrorWorkflowSrc), "min-mirror-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// `min` over {blocker, low} computes `low`; `lowest_open` reads `low` too,
	// meaning something else entirely.
	const coincident = `[
	  {"id":"C-1","severity":["blocker","low"],"lowest_open":"low"}
	]`
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", coincident)
	driveAction(t, conn, e, "reconcile@0")

	held := heldStep(t, conn, "reconcile-held@0#0")
	err = e.DecideStepValue(conn, held.ID, true, "call it high", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["lowest_open"].(string); got != "low" {
		t.Errorf("lowest_open = %q, want %q untouched — it coincided with the "+
			"computed value while carrying an independent fact, and a correction "+
			"of the severity is not a correction of it", got, "low")
	}
}

// crossEnumWorkflowSrc thresholds on `confidence`, a field over a DIFFERENT
// declared order than the aggregated `severity`.
const crossEnumWorkflowSrc = `
[pipeline]
name = "cross-enum-change"
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
payload = "cross-enum-findings@1"
threshold = { "fix-loop" = "any(confidence >= certain)", "drain-highs" = "any(severity >= high)" }
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

const crossEnumSchemaSrc = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "cross-enum-findings@1",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      },
      "confidence": {
        "type": "string",
        "enum": ["low", "blocker", "firm", "certain"],
        "ordered_enum": true
      }
    },
    "required": ["severity"],
    "additionalProperties": true
  }
}`

// TestCorrectionSkipsAThresholdFieldThatRejectsTheValue is the enum guard. A
// threshold field declaring its own vocabulary cannot receive a value that
// vocabulary does not contain: writing one would persist a schema-invalid
// payload and park the very routing step the approve was made to resolve.
func TestCorrectionSkipsAThresholdFieldThatRejectsTheValue(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSchemaFixture(t, conn, "cross-enum-findings", 1, crossEnumSchemaSrc)
	registerSource(t, conn, []byte(crossEnumWorkflowSrc), "cross-enum-change.toml")
	issue := createIssue(t, conn, "correct the severity", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// `confidence` reads `blocker`, the value the correction displaces on
	// `severity` — the coincidence that reaches the rewrite at all — while its
	// own declared order stops short of `high`.
	const crossEnum = `[
	  {"id":"C-1","severity":["blocker","low"],"confidence":"blocker"}
	]`
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", crossEnum)
	driveAction(t, conn, e, "reconcile@0")

	held := heldStep(t, conn, "reconcile-held@0#0")
	err = e.DecideStepValue(conn, held.ID, true, "call it high", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := artifactPayloads(t, conn, stepIDByInstance(t, conn, "reconcile@0"))
	resolved := elements[len(elements)-1][0]
	if got, _ := resolved["confidence"].(string); got != "blocker" {
		t.Errorf("confidence = %q, want %q untouched — `high` is not a value its "+
			"declared order contains, and writing it there persists a payload the "+
			"threshold cannot evaluate", got, "blocker")
	}

	routing := heldStep(t, conn, "reconcile@0")
	if strings.HasPrefix(routing.Routing, workflow.OnFailWaitingHuman) {
		t.Fatalf("reconcile@0 routed %q — the approve parked the step it was "+
			"made to resolve", routing.Routing)
	}
	if routing.Routing != "drain-highs" {
		t.Errorf("reconcile@0 routed %q, want %q", routing.Routing, "drain-highs")
	}
}

// TestThresholdMirrorFieldsCollectsEachComparedFieldOnce pins the helper's own
// contract, which the end-to-end fixtures above exercise only one field at a
// time: every distinct compared field, in threshold order, without the
// aggregated field the correction already lands on, and without failing a
// decision over a predicate the threshold's own evaluation will report.
func TestThresholdMirrorFieldsCollectsEachComparedFieldOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold map[string]string
		field     string
		want      []string
	}{
		{
			name: "distinct fields in threshold order",
			threshold: map[string]string{
				"fix-loop":    "any(open_severity >= blocker)",
				"drain-highs": "any(worst_open >= high)",
			},
			field: "severity",
			want:  []string{"open_severity", "worst_open"},
		},
		{
			name: "a field compared by two arms is collected once",
			threshold: map[string]string{
				"fix-loop":    "any(open_severity >= blocker)",
				"drain-highs": "any(open_severity >= high)",
			},
			field: "severity",
			want:  []string{"open_severity"},
		},
		{
			name: "the aggregated field is excluded",
			threshold: map[string]string{
				"fix-loop":    "any(severity >= blocker)",
				"drain-highs": "any(open_severity >= high)",
			},
			field: "severity",
			want:  []string{"open_severity"},
		},
		{
			name:      "an unparseable predicate contributes nothing",
			threshold: map[string]string{"fix-loop": "not a predicate"},
			field:     "severity",
			want:      nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := thresholdMirrorFields(tc.threshold, tc.field)
			if !slices.Equal(got, tc.want) {
				t.Errorf("thresholdMirrorFields = %v, want %v", got, tc.want)
			}
		})
	}
}
