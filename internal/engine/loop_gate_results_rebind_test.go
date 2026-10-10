package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `<step>.gate-results` did not rebind to the loop body's latest round. The
// artifact form rides loopProducerRedirect: a consumer downstream of
// `after_loop` re-entered at ordinal N reads `fix@N`'s change-summary where
// it declared `implement.change-summary`. The
// gate-results form resolved through its own instance scan and never applied
// the redirect, so the same consumer's `implement.gate-results` read
// `implement@0`'s recorded gates on every round. In one measured run, a
// verify-ac re-run after a fix round judged the tree `fix@N` produced against
// the tests `implement@0` had run.
//
// These tests pin the fix and its boundary. The consumer downstream of
// `after_loop` reads the body's recorded gates at its ordinal; the loop body's
// own `implement.gate-results` still binds `implement@0`, exactly as its
// `implement.change-summary` does.

// gateRebindLoopSrc is standard-change's fix loop, minimized: implement runs
// `checks`, a review fanout reads its recorded gates and its change-summary,
// an aggregate routes `fix-loop` on blockers, and the loop-body `fix` step
// emits a fresh change-summary and runs its own `fix-checks` gate. The two
// gate names differ so a bundle says which round's results it carries.
const gateRebindLoopSrc = `
[pipeline]
name = "gate-rebind-loop"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "author"
emits = "change-summary"
gates = ["checks"]
inputs = ["issue.body"]

[[step]]
name = "review"
after = ["implement"]
fanout = ["judge-one", "judge-two"]
emits = "findings"
inputs = ["implement.gate-results", "implement.change-summary"]

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
name = "fix"
executor = "author"
emits = "change-summary"
gates = ["fix-checks"]
loop = true
inputs = ["reconcile.findings", "implement.change-summary", "implement.gate-results"]
after_loop = "review"
`

const (
	implementSummary = "IMPLEMENT-SUMMARY: the first change."
	fixSummary       = "FIX-SUMMARY: the round-1 change."
)

// activateGateRebindLoop registers the fixture schema and the definition —
// through the register path, so the loop body's `implement.gate-results`
// declaration is proven registrable — and activates a run over one issue.
func activateGateRebindLoop(t *testing.T, conn *sql.DB) {
	t.Helper()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(gateRebindLoopSrc), "gate-rebind-loop.toml")
	issue := createIssue(t, conn, "land the change", "the issue body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
}

// driveGateRebindRoundZero completes implement, both judges, and the synthesis
// with a blocker, then runs the aggregate — which routes `fix-loop` and
// instantiates `fix@1` beside the re-entered `review@1` fanout.
func driveGateRebindRoundZero(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "implement@0", implementSummary, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@0")
	if !stepExists(t, conn, "fix@1") {
		t.Fatalf("premise: the blocker did not enter the fix loop; fix@1 " +
			"was never instantiated")
	}
}

// recordedGate is the one field of the §11.4 shape these tests route on.
type recordedGate struct {
	Gate    string `json:"gate"`
	Verdict string `json:"verdict"`
}

// gateResultsInputs filters a bundle down to its gate-results entries, each
// parsed: the producer instance and the gates it recorded.
func gateResultsInputs(t *testing.T, inputs []ContextInput) map[string][]recordedGate {
	t.Helper()
	out := map[string][]recordedGate{}
	for _, in := range inputs {
		if in.Kind != inputGateResults {
			continue
		}
		var rows []recordedGate
		err := json.Unmarshal([]byte(in.Body), &rows)
		testsupport.Must(t, err, "parsing %s's gate-results body: %v", in.ProducerStep, err)
		out[in.ProducerStep] = rows
	}
	return out
}

// TestReviewRoundOneReadsTheFixRoundGates is the incident inverted into the
// fix: on loop re-entry the review fanout's `implement.gate-results` must
// carry `fix@1`'s recorded gates — the checks run against the tree the
// judges are about to judge — and never `implement@0`'s round-0 results.
func TestReviewRoundOneReadsTheFixRoundGates(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateGateRebindLoop(t, conn)
	driveGateRebindRoundZero(t, conn, e)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", fixSummary, "")

	for i := range 2 {
		instance := fmt.Sprintf("review@1#%d", i)
		bundle, err := ReadContext(conn, stepIDByInstance(t, conn, instance), nowMS)
		testsupport.Must(t, err, "assembling %s's bundle: %v", instance, err)

		gates := gateResultsInputs(t, bundle.Inputs)
		if len(gates) != 1 {
			t.Fatalf("%s binds %d gate-results inputs, want 1: %+v",
				instance, len(gates), gates)
		}
		rows, ok := gates["fix@1"]
		if !ok {
			t.Fatalf("%s's gate-results input is from %v, want fix@1 — the "+
				"round whose tree the judges read", instance, keysOf(gates))
		}
		if len(rows) != 1 || rows[0].Gate != "fix-checks" || rows[0].Verdict != "pass" {
			t.Errorf("%s's gate results = %+v, want fix@1's one passing "+
				"`fix-checks` row", instance, rows)
		}

		// The change-summary beside it rebinds by the same rule, so the
		// packet's account of the tree and its measurements agree.
		for _, in := range bundle.Inputs {
			if in.Kind == "change-summary" && in.ProducerStep != "fix@1" {
				t.Errorf("%s's change-summary is from %s, want fix@1",
					instance, in.ProducerStep)
			}
		}
	}
}

// TestReviewRoundZeroReadsTheImplementGates pins the ordinal-0 binding: with
// no loop body yet, the review fanout reads `implement@0`'s recorded gates.
func TestReviewRoundZeroReadsTheImplementGates(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateGateRebindLoop(t, conn)
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "implement@0", implementSummary, "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@0#0"), nowMS)
	testsupport.Must(t, err, "assembling review@0#0's bundle: %v", err)

	gates := gateResultsInputs(t, bundle.Inputs)
	rows, ok := gates["implement@0"]
	if len(gates) != 1 || !ok {
		t.Fatalf("review@0#0's gate-results are from %v, want implement@0 alone",
			keysOf(gates))
	}
	if len(rows) != 1 || rows[0].Gate != "checks" || rows[0].Verdict != "pass" {
		t.Errorf("review@0#0's gate results = %+v, want implement@0's one "+
			"passing `checks` row", rows)
	}
}

// TestLoopBodyGateResultsStillBindTheNamedProducer pins the boundary: the
// loop body's own `implement.gate-results` never redirects. `fix@1` reads
// `implement@0`'s gates, the same instance its `implement.change-summary`
// binds, because `implement` is genuinely upstream of the loop and the body
// is never in `after_loop`'s downstream set.
func TestLoopBodyGateResultsStillBindTheNamedProducer(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateGateRebindLoop(t, conn)
	driveGateRebindRoundZero(t, conn, e)

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "fix@1"), nowMS)
	testsupport.Must(t, err, "assembling fix@1's bundle: %v", err)

	gates := gateResultsInputs(t, bundle.Inputs)
	rows, ok := gates["implement@0"]
	if len(gates) != 1 || !ok {
		t.Fatalf("fix@1's gate-results are from %v, want implement@0 alone — "+
			"a loop body's declared producer never redirects", keysOf(gates))
	}
	if len(rows) != 1 || rows[0].Gate != "checks" {
		t.Errorf("fix@1's gate results = %+v, want implement@0's `checks` row", rows)
	}
}

// TestReviewRoundTwoReadsTheLatestFixRoundGates: a second loop entry rebinds
// to `fix@2`, not `fix@1` — the redirect is to the body's instance at the
// consumer's own ordinal, never merely the newest body that ever ran.
func TestReviewRoundTwoReadsTheLatestFixRoundGates(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	activateGateRebindLoop(t, conn)
	driveGateRebindRoundZero(t, conn, e)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", fixSummary, "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@1#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@1", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@1")
	if !stepExists(t, conn, "fix@2") {
		t.Fatalf("premise: the still-open blocker did not enter the fix loop " +
			"a second time; fix@2 was never instantiated")
	}
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "FIX-SUMMARY: the round-2 change.", "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@2#0"), nowMS)
	testsupport.Must(t, err, "assembling review@2#0's bundle: %v", err)

	gates := gateResultsInputs(t, bundle.Inputs)
	if _, ok := gates["fix@2"]; len(gates) != 1 || !ok {
		t.Fatalf("review@2#0's gate-results are from %v, want fix@2 alone",
			keysOf(gates))
	}
}

// keysOf lists a map's keys for a failure message.
func keysOf(m map[string][]recordedGate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
