package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// registerSourceErr runs the register-path validation and RETURNS the error,
// for cases asserting a refusal.
func registerSourceErr(t *testing.T, src []byte) error {
	t.Helper()
	def, err := workflow.Parse(src)
	if err != nil {
		return err
	}
	if err := workflow.Validate(def); err != nil {
		return err
	}
	return workflow.Lint(def)
}

// `<step>.gate-results` (DKT-77): the producer's RECORDED gate results,
// addressable as an input. Before this form existed no syntax exposed what
// the engine had already recorded, so every review step re-ran the same
// checks independently — measured at 44/44 judge steps re-running the tests
// in one run — while the results sat in the ledger.

const gateResultsWorkflow = `
[pipeline]
name = "gateresults"
version = 1
[[step]]
name = "implement"
after = []
executor = "author"
emits = "change-summary"
gates = ["checks"]
[[step]]
name = "review"
after = ["implement"]
executor = "judge"
emits = "findings"
inputs = ["implement.gate-results"]
`

func TestGateResultsResolveAsAnInput(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(gateResultsWorkflow), "gateresults.toml")
	issue := createIssue(t, conn, "expose the trail", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)

	var body string
	for _, input := range claim.Context.Inputs {
		if input.Kind == "gate-results" {
			body = input.Body
			if input.ProducerStep != "implement@0" {
				t.Errorf("producer = %q, want implement@0", input.ProducerStep)
			}
		}
	}
	if body == "" {
		t.Fatal("review's context carries no gate-results input")
	}

	// The body is the §11.4 `gate result` shape, parsed rather than
	// re-grepped: the recorded gate, its verdict, and its exit are what a
	// consumer routes on instead of re-running the check.
	var results []struct {
		Gate    string `json:"gate"`
		Verdict string `json:"verdict"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(body), &results), "parsing: %v", err)
	if len(results) != 1 || results[0].Gate != "checks" || results[0].Verdict != "pass" {
		t.Errorf("gate results = %+v, want the recorded `checks` pass", results)
	}
}

// A step may declare ITS OWN gate-results (DKT-12): `pre = true` gates run at
// claim and their rows commit before context assembly, so a self-declared
// input is the step reading its own claim-time measurements. The done-filter
// used to drop the requesting step — `claimed` at assembly — and the input
// silently resolved absent.

const selfGateResultsWorkflow = `
[pipeline]
name = "selfgateresults"
version = 1
[[step]]
name = "verify"
after = []
executor = "checker"
emits = "verdict"
gates = [{ name = "ac-commands", pre = true }]
inputs = ["verify.gate-results"]
`

func TestOwnPreGateRowsResolveAsAnInput(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(selfGateResultsWorkflow), "selfgateresults.toml")
	issue := createIssue(t, conn, "read your own measurements", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// A TRUSTED pre-gate that runs and passes, so the recorded row carries a
	// real verdict rather than `unmatched`.
	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := stepIDByInstance(t, conn, "verify@0")
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "claim verify: %v", err)

	assertOwnPreGateInput(t, "claim bundle", claim.Context)

	// The read verb agrees: `step context` re-assembles while the step is
	// still `claimed`, which is exactly the status the done-filter dropped.
	ctx, err := ReadContext(conn, stepID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	assertOwnPreGateInput(t, "post-claim ReadContext", ctx)
}

func assertOwnPreGateInput(t *testing.T, where string, ctx *Context) {
	t.Helper()
	var body string
	for _, input := range ctx.Inputs {
		if input.Kind == "gate-results" {
			body = input.Body
			if input.ProducerStep != "verify@0" {
				t.Errorf("%s: producer = %q, want verify@0", where, input.ProducerStep)
			}
		}
	}
	if body == "" {
		t.Fatalf("%s carries no gate-results input", where)
	}
	var results []struct {
		Gate    string `json:"gate"`
		Verdict string `json:"verdict"`
		Pre     bool   `json:"pre"`
	}
	err := json.Unmarshal([]byte(body), &results)
	testsupport.Must(t, err, "parsing the %s body: %v", where, err)
	if len(results) != 1 || results[0].Gate != "ac-commands" ||
		results[0].Verdict != "pass" || !results[0].Pre {
		t.Errorf("%s: gate results = %+v, want one passing pre `ac-commands` row",
			where, results)
	}
}

// claimOwnPreGateContext claims selfGateResultsWorkflow's verify@0 with its
// `ac-commands` pre-gate trusted by an entry whose placeholder declaration is
// `stubEntry`, and returns the claim's context bundle.
func claimOwnPreGateContext(t *testing.T, stubEntry bool) *Context {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(selfGateResultsWorkflow), "selfgateresults.toml")
	issue := createIssue(t, conn, "flag placeholder passes", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot), Stub: stubEntry,
	})
	e.Gates = runner

	claim, err := e.ClaimStepWithGates(conn, stepIDByInstance(t, conn, "verify@0"),
		ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim verify: %v", err)
	return claim.Context
}

// gateResultsRows parses the context's single gate-results body into generic
// maps, so a test can tell an absent key from a false one.
func gateResultsRows(t *testing.T, ctx *Context) []map[string]any {
	t.Helper()
	var rows []map[string]any
	found := false
	for _, input := range ctx.Inputs {
		if input.Kind == "gate-results" {
			found = true
			err := json.Unmarshal([]byte(input.Body), &rows)
			testsupport.Must(t, err, "parsing the gate-results body: %v", err)
		}
	}
	if !found {
		t.Fatal("the context carries no gate-results input")
	}
	if len(rows) != 1 {
		t.Fatalf("gate results = %+v, want exactly one row", rows)
	}
	return rows
}

// A pass authorized by a placeholder trust entry must say so in the input a
// reviewer reads instead of re-running the check, and a real entry's pass must
// say so too: an absent key cannot be told apart from a docket too old to
// carry the flag.
func TestGateResultsInputCarriesStubEntry(t *testing.T) {
	for _, stubEntry := range []bool{true, false} {
		row := gateResultsRows(t, claimOwnPreGateContext(t, stubEntry))[0]
		got, present := row["stub"]
		if !present {
			t.Errorf("stub entry %v: row %+v has no `stub` key", stubEntry, row)
			continue
		}
		if got != stubEntry {
			t.Errorf("stub entry %v: `stub` = %v", stubEntry, got)
		}
	}
}

// A row migrated from an S3 pass-through trail renders `s3_migrated: true`;
// any other row omits the key, matching `docket step gates`.
func TestGateResultsInputCarriesS3Migrated(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(gateResultsWorkflow), "gateresults.toml")
	issue := createIssue(t, conn, "flag migrated rows", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// testEngine's PassThroughRunner is the S3 runner: every row it records
	// carries Stub.
	claimAndComplete(t, conn, testEngine(), "implement@0", "summary", "")
	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)

	migrated := gateResultsRows(t, claim.Context)[0]
	if migrated["s3_migrated"] != true {
		t.Errorf("S3-migrated row %+v lacks `s3_migrated: true`", migrated)
	}

	current := gateResultsRows(t, claimOwnPreGateContext(t, false))[0]
	if _, present := current["s3_migrated"]; present {
		t.Errorf("row from the real runner %+v carries `s3_migrated`; "+
			"the key is omitted when false", current)
	}
}

// TestGateResultsRegisterRules: the form validates against the step's
// EXISTENCE only — gates can arrive from a fence source the definition does
// not enumerate — and the kind itself is reserved from `emits`.
func TestGateResultsRegisterRules(t *testing.T) {

	// Naming a step that does not exist still refuses.
	bad := `
[pipeline]
name = "gr-missing"
version = 1
[[step]]
name = "review"
after = []
executor = "judge"
emits = "findings"
inputs = ["implement.gate-results"]
`
	if err := registerSourceErr(t, []byte(bad)); err == nil {
		t.Error("an input naming a missing step registered")
	}

	// Emitting the reserved kind refuses: the input form would shadow it.
	shadowed := `
[pipeline]
name = "gr-shadow"
version = 1
[[step]]
name = "implement"
after = []
executor = "author"
emits = "gate-results"
`
	if err := registerSourceErr(t, []byte(shadowed)); err == nil {
		t.Error("a step emitting the reserved gate-results kind registered")
	}
}

// A skipped producer at the latest ordinal makes `<step>.gate-results` resolve
// empty for that ordinal instead of falling back to an earlier round's rows.
// An ordinary `<step>.<kind>` input over the same producer still falls back.
// The fixture's `review` re-runs every round, records its own `review-checks`
// gate, and feeds `synthesize` both forms.
const skippedGateProducerSrc = `
[pipeline]
name = "skipped-gate-producer"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "author"
emits = "change-summary"

[[step]]
name = "review"
after = ["implement"]
executor = "judge"
emits = "findings"
gates = ["review-checks"]

[[step]]
name = "synthesize"
after = ["review"]
executor = "synthesize-findings"
emits = "findings"
inputs = ["review.findings", "review.gate-results"]

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
loop = true
inputs = ["reconcile.findings"]
after_loop = "review"
`

const (
	roundZeroReview = "REVIEW-0: the round-0 findings."
	roundOneReview  = "REVIEW-1: findings the operator skipped past."
)

// skippedReviewRoundOne drives round 0 into the fix loop, then records
// `review@1` (its findings and a passing `review-checks` row) and moves it to
// `skipped`, as `resolve --as skip` does to a parked step that had recorded.
// It returns `synthesize@1`'s assembled inputs.
func skippedReviewRoundOne(t *testing.T) []ContextInput {
	t.Helper()
	conn := mustDB(t)
	e := testEngine()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(skippedGateProducerSrc), "skipped-gate-producer.toml")
	issue := createIssue(t, conn, "land the change", "the issue body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "implement@0", "the change", "")
	claimAndComplete(t, conn, e, "review@0", roundZeroReview, "")
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", blockerPayload)
	driveAction(t, conn, e, "reconcile@0")
	if !stepExists(t, conn, "review@1") || !stepExists(t, conn, "synthesize@1") {
		t.Fatal("premise: the blocker did not re-enter review at ordinal 1")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "review@1", roundOneReview, "")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = 'review@1'`,
		db.StepSkipped)

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "synthesize@1"), nowMS)
	testsupport.Must(t, err, "assembling synthesize@1's bundle: %v", err)
	return bundle.Inputs
}

func TestSkippedLatestProducerResolvesEmptyGateResults(t *testing.T) {
	gates := gateResultsInputs(t, skippedReviewRoundOne(t))
	rows, ok := gates["review@1"]
	if len(gates) != 1 || !ok {
		t.Fatalf("synthesize@1's gate-results are from %v, want review@1 alone: "+
			"a skipped producer pins the ordinal, never falls back to review@0",
			keysOf(gates))
	}
	if len(rows) != 0 {
		t.Errorf("review@1's gate results = %+v, want an empty array: the "+
			"skipped instance's recorded rows are not its input", rows)
	}
}

func TestSkippedLatestProducerArtifactInputFallsBack(t *testing.T) {
	var found []string
	for _, in := range skippedReviewRoundOne(t) {
		// The loop also carries the prior round's synthesize and reconcile
		// findings; only the review.findings binding is under test.
		if in.Kind != "findings" || !strings.HasPrefix(in.ProducerStep, "review@") {
			continue
		}
		found = append(found, in.ProducerStep)
		if in.ProducerStep != "review@0" || in.Body != roundZeroReview {
			t.Errorf("synthesize@1's review.findings is %s %q, want review@0 %q: "+
				"an ordinary input skips past a skipped producer",
				in.ProducerStep, in.Body, roundZeroReview)
		}
	}
	if len(found) != 1 {
		t.Fatalf("synthesize@1 binds review.findings from %v, want review@0 alone", found)
	}
}
