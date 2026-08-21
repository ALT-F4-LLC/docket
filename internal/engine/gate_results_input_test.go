package engine

import (
	"encoding/json"
	"testing"

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
