package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// minimalWorkflow is a definition small enough that a test's intent is not
// buried in grammar. Two steps, so `after` is exercised.
const minimalWorkflow = `
[pipeline]
name = "unit"
version = 1
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
[[step]]
name = "second"
after = ["first"]
type = "human"
on_fail = "skip"
`

func workflowRegisterCmdWithDB(conn *sql.DB) *cobra.Command {
	return cmdWithDB(conn)
}

func workflowListCmdWithDB(conn *sql.DB, limit int) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("name", "", "")
	cmd.Flags().Int("limit", limit, "")
	return cmd
}

func workflowShowCmdWithDB(conn *sql.DB, source bool) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().Bool("source", source, "")
	return cmd
}

// codeOf extracts the error code a CmdError carries, which is what determines
// the process exit code.
func codeOf(t *testing.T, err error) output.ErrorCode {
	t.Helper()
	var ce *CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CmdError", err)
		return ""
	}
	return ce.Code
}

// registerSource writes src to a temp file and registers it.
func registerSource(t *testing.T, conn *sql.DB, src string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.toml")
	err := os.WriteFile(path, []byte(src), 0o644)
	testsupport.Must(t, err, "writing the definition: %v", err)
	cmd := workflowRegisterCmdWithDB(conn)
	w, _ := bufWriter(true)
	return runWorkflowRegister(cmd, []string{path}, w)
}

func TestWorkflowRegisterSucceeds(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "register: %v", err)

	wf, err := db.GetWorkflow(conn, 1, "unit", 1)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	if wf.Body != minimalWorkflow {
		t.Error("the stored body is not the registered bytes, verbatim")
	}
	// The parsed form is stored alongside the body: activation reads it on a
	// hot path, and it is the pinned interpretation.
	if _, err := workflow.FromCanonical([]byte(wf.Parsed)); err != nil {
		t.Errorf("the stored parsed form does not decode: %v", err)
	}
}

// TestWorkflowRegisterRefusalMatrix walks §4.5 by error code. Exit codes are
// derived from the code, so asserting the code asserts both.
func TestWorkflowRegisterRefusalMatrix(t *testing.T) {
	t.Run("validation failure is VALIDATION_ERROR", func(t *testing.T) {
		conn := newTestDB(t)
		// A type="human" step declaring no on_fail — V13a.
		err := registerSource(t, conn, `
[pipeline]
name = "bad"
version = 1
[[step]]
name = "gate"
type = "human"
`)
		if err == nil {
			t.Fatal("an invalid definition registered successfully")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %s, want VALIDATION_ERROR", got)
		}
		if output.ExitCodeForError(codeOf(t, err)) != output.ExitValidation {
			t.Error("a validation failure does not exit 3")
		}
	})

	t.Run("missing file is NOT_FOUND", func(t *testing.T) {
		conn := newTestDB(t)
		cmd := workflowRegisterCmdWithDB(conn)
		w, _ := bufWriter(true)
		err := runWorkflowRegister(cmd, []string{filepath.Join(t.TempDir(), "absent.toml")}, w)
		if err == nil {
			t.Fatal("registering a missing file succeeded")
		}
		if got := codeOf(t, err); got != output.ErrNotFound {
			t.Errorf("code = %s, want NOT_FOUND", got)
		}
		if output.ExitCodeForError(codeOf(t, err)) != output.ExitNotFound {
			t.Error("a missing file does not exit 2")
		}
	})

	t.Run("differing bytes at an existing name@version is CONFLICT", func(t *testing.T) {
		conn := newTestDB(t)
		err := registerSource(t, conn, minimalWorkflow)
		testsupport.Must(t, err, "first register: %v", err)
		err = registerSource(t, conn, minimalWorkflow+"\n# a different file\n")
		if err == nil {
			t.Fatal("re-registering different bytes succeeded")
		}
		if got := codeOf(t, err); got != output.ErrConflict {
			t.Errorf("code = %s, want CONFLICT", got)
		}
		if output.ExitCodeForError(codeOf(t, err)) != output.ExitConflict {
			t.Error("a conflicting re-register does not exit 4")
		}
	})
}

// TestWorkflowRegisterIsIdempotent: identical bytes register again as a
// SUCCESS, so a pipeline that re-applies its configuration is not an error.
func TestWorkflowRegisterIsIdempotent(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "first register: %v", err)
	err = registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "re-registering identical bytes failed: %v", err)

	_, total, err := db.ListWorkflows(conn, db.WorkflowListOptions{})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if total != 1 {
		t.Errorf("two identical registrations produced %d rows", total)
	}
}

// TestWorkflowRegisterReadsStdin covers the `-` argument, so configuration
// generated in a pipeline needs no temp file.
func TestWorkflowRegisterReadsStdin(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowRegisterCmdWithDB(conn)
	cmd.SetIn(strings.NewReader(minimalWorkflow))
	w, _ := bufWriter(true)

	err := runWorkflowRegister(cmd, []string{"-"}, w)
	testsupport.Must(t, err, "register -: %v", err)

	wf, err := db.GetWorkflow(conn, 1, "unit", 1)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	// stdin has no path to record, and source_path is provenance only.
	if wf.SourcePath != "" {
		t.Errorf("source_path = %q for a stdin registration", wf.SourcePath)
	}
}

func TestWorkflowRegisterRejectsEmptyStdin(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowRegisterCmdWithDB(conn)
	cmd.SetIn(strings.NewReader(""))
	w, _ := bufWriter(true)

	err := runWorkflowRegister(cmd, []string{"-"}, w)
	if err == nil {
		t.Fatal("an empty stdin registered successfully")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want VALIDATION_ERROR", got)
	}
}

// TestWorkflowShowSelectsHighestVersion: `workflow show NAME` without
// @version means the highest registered.
func TestWorkflowShowSelectsHighestVersion(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "v1: %v", err)
	if err := registerSource(t, conn,
		strings.Replace(minimalWorkflow, "version = 1", "version = 3", 1)); err != nil {
		t.Fatalf("v3: %v", err)
	}

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(true)
	err = runWorkflowShow(cmd, []string{"unit"}, w)
	testsupport.Must(t, err, "show: %v", err)

	var envelope struct {
		Data struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if envelope.Data.Version != 3 {
		t.Errorf("show without @version returned v%d, want the highest (v3)",
			envelope.Data.Version)
	}

	// And an explicit version selects that one.
	cmd = workflowShowCmdWithDB(conn, false)
	w, buf = bufWriter(true)
	err = runWorkflowShow(cmd, []string{"unit@1"}, w)
	testsupport.Must(t, err, "show @1: %v", err)
	err = json.Unmarshal(buf.Bytes(), &envelope)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if envelope.Data.Version != 1 {
		t.Errorf("show @1 returned v%d", envelope.Data.Version)
	}
}

// costWorkflow exercises the three cases the cost column has to render apart:
// a plain step declaring a cost, a fanout step whose declared cost is PER
// SIBLING, and a step declaring no cost at all.
const costWorkflow = `
[pipeline]
name = "costed"
version = 1
[[step]]
name = "implement"
after = []
executor = "implement"
emits = "change-summary"
expected_cost = 1.50
[[step]]
name = "review"
after = ["implement"]
fanout = ["judge-a", "judge-b", "judge-c", "judge-d"]
emits = "findings"
expected_cost = 0.60
[[step]]
name = "approve"
after = ["review"]
type = "human"
on_fail = "skip"
`

// TestWorkflowShowRendersExpectedCost: the human summary carries the per-step
// `expected_cost` and a fanout-expanded total, which is the floor a plan's
// budget arithmetic is compared against (DKT-528). Before this the numbers
// existed only in the registered TOML and no read verb answered for them.
func TestWorkflowShowRendersExpectedCost(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, costWorkflow)
	testsupport.Must(t, err, "register: %v", err)

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(false)
	err = runWorkflowShow(cmd, []string{"costed"}, w)
	testsupport.Must(t, err, "show: %v", err)
	out := buf.String()

	for _, want := range []string{
		// A plain step's declared cost.
		"cost=1.50",
		// A fanout step is PER SIBLING: four hints at 0.60 contribute 2.40,
		// and the arithmetic is shown rather than left for the reader.
		"cost=0.60 x4 = 2.40",
		// A step declaring nothing renders explicitly, never silently omitted.
		"cost=-",
		// 1.50 + 2.40 + 0.
		"expected_cost total: 3.90",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output is missing %q:\n%s", want, out)
		}
	}
}

// TestWorkflowShowCostTotalIncludesGatedAndLoopSteps: the floor the plan skill
// budgets against sums `when`-gated and `loop` steps IN. A gated step may not
// run, but a budget that assumed it would not is a budget that breaks the first
// time the label is present.
func TestWorkflowShowCostTotalIncludesGatedAndLoopSteps(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, `
[pipeline]
name = "gated"
version = 1
[[step]]
name = "implement"
after = []
executor = "implement"
emits = "change-summary"
expected_cost = 1.00
[[step]]
name = "review-security"
after = ["implement"]
executor = "judge-security"
when = "labels contains security"
emits = "findings"
expected_cost = 0.60
[[step]]
name = "fix"
executor = "fix"
emits = "change-summary"
expected_cost = 0.40
loop = true
after_loop = "review-security"
`)
	testsupport.Must(t, err, "register: %v", err)

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(false)
	err = runWorkflowShow(cmd, []string{"gated"}, w)
	testsupport.Must(t, err, "show: %v", err)
	out := buf.String()

	if !strings.Contains(out, "expected_cost total: 2.00") {
		t.Errorf("the total does not include the gated and loop steps:\n%s", out)
	}
	// A loop step's cost is per loop ENTRY — the total counts one, and every
	// additional fix round costs it again. Saying so is what stops the floor
	// from being read as a ceiling.
	if !strings.Contains(out, "cost=0.40 per loop entry") {
		t.Errorf("a loop step's cost is not annotated per entry:\n%s", out)
	}
}

func TestWorkflowShowNotFound(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowShowCmdWithDB(conn, false)
	w, _ := bufWriter(true)

	err := runWorkflowShow(cmd, []string{"ghost"}, w)
	if err == nil {
		t.Fatal("showing an unregistered workflow succeeded")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("code = %s, want NOT_FOUND", got)
	}

	// An unregistered VERSION of a registered name is equally NOT_FOUND.
	err = registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "register: %v", err)
	cmd = workflowShowCmdWithDB(conn, false)
	w, _ = bufWriter(true)
	err = runWorkflowShow(cmd, []string{"unit@9"}, w)
	if err == nil {
		t.Fatal("showing an unregistered version succeeded")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("code = %s, want NOT_FOUND", got)
	}
}

// TestWorkflowShowSourceEmitsTheRegisteredBytes: --source is what makes the
// content hash auditable — what comes out is what was hashed.
func TestWorkflowShowSourceEmitsTheRegisteredBytes(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "register: %v", err)

	cmd := workflowShowCmdWithDB(conn, true)
	w, buf := bufWriter(true)
	err = runWorkflowShow(cmd, []string{"unit"}, w)
	testsupport.Must(t, err, "show --source: %v", err)

	var envelope struct {
		Data struct {
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if envelope.Data.Source != minimalWorkflow {
		t.Errorf("--source emitted:\n%q\nwant:\n%q", envelope.Data.Source, minimalWorkflow)
	}
}

func TestWorkflowShowRejectsAMalformedRef(t *testing.T) {
	conn := newTestDB(t)
	for _, ref := range []string{"unit@", "unit@x", "unit@0", "@1"} {
		cmd := workflowShowCmdWithDB(conn, false)
		w, _ := bufWriter(true)
		err := runWorkflowShow(cmd, []string{ref}, w)
		if err == nil {
			t.Errorf("ref %q was accepted", ref)
			continue
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("ref %q: code = %s, want VALIDATION_ERROR", ref, got)
		}
	}
}

// TestWorkflowListIsACollection: v2 renders {items, total, truncated} and the
// total ignores the limit.
func TestWorkflowListIsACollection(t *testing.T) {
	conn := newTestDB(t)
	for _, v := range []int{1, 2, 3} {
		src := strings.Replace(minimalWorkflow, "version = 1",
			"version = "+string(rune('0'+v)), 1)
		if err := registerSource(t, conn, src); err != nil {
			t.Fatalf("registering v%d: %v", v, err)
		}
	}

	cmd := workflowListCmdWithDB(conn, 2)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runWorkflowList(cmd, nil, w)
	testsupport.Must(t, err, "list: %v", err)

	var envelope struct {
		Data struct {
			Items     []json.RawMessage `json:"items"`
			Total     int               `json:"total"`
			Truncated bool              `json:"truncated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(envelope.Data.Items) != 2 {
		t.Errorf("items = %d under --limit 2", len(envelope.Data.Items))
	}
	if envelope.Data.Total != 3 {
		t.Errorf("total = %d, want the true pre-limit count 3", envelope.Data.Total)
	}
	if !envelope.Data.Truncated {
		t.Error("truncated = false when the limit dropped a row")
	}
}

// TestWorkflowListItemsCarryRowVersionUnderV2: the v2 envelope consults
// output.Versioned on the items CONTAINER, not on every element, so a bare
// slice of workflows renders v1 items inside a v2 envelope. QA caught exactly
// that, and this is the unit-level guard for it.
func TestWorkflowListItemsCarryRowVersionUnderV2(t *testing.T) {
	conn := newTestDB(t)
	err := registerSource(t, conn, minimalWorkflow)
	testsupport.Must(t, err, "register: %v", err)

	cmd := workflowListCmdWithDB(conn, 50)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err = runWorkflowList(cmd, nil, w)
	testsupport.Must(t, err, "list: %v", err)

	var envelope struct {
		Data struct {
			Items []struct {
				Name       string `json:"name"`
				Version    int    `json:"version"`
				RowVersion *int   `json:"row_version"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(envelope.Data.Items) == 0 {
		t.Fatal("no items")
	}
	for _, item := range envelope.Data.Items {
		if item.RowVersion == nil {
			t.Errorf("v2 item %q carries no row_version", item.Name)
		}
		// The two versions stay named apart: `version` is the definition's.
		if item.Version != 1 {
			t.Errorf("item %q: version = %d, want the definition's 1", item.Name, item.Version)
		}
	}

	// And v1 items carry none, which is the dormancy rule at the envelope.
	cmd = workflowListCmdWithDB(conn, 50)
	w, buf = bufWriter(true)
	err = runWorkflowList(cmd, nil, w)
	testsupport.Must(t, err, "list v1: %v", err)
	if strings.Contains(buf.String(), "row_version") {
		t.Errorf("v1 output carries row_version: %s", buf.String())
	}
}

// TestWorkflowListIsEmptyWhenNothingIsRegistered is the phase-1 dormancy
// claim at the verb: the table exists, and reports nothing.
func TestWorkflowListIsEmptyWhenNothingIsRegistered(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowListCmdWithDB(conn, 50)
	w, buf := bufWriter(true)
	err := runWorkflowList(cmd, nil, w)
	testsupport.Must(t, err, "list: %v", err)

	var envelope struct {
		Data struct {
			Total int `json:"total"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &envelope)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if envelope.Data.Total != 0 {
		t.Errorf("total = %d in a repo that never registered a workflow", envelope.Data.Total)
	}
}

func workflowInitCmdWith(template, dir string, force bool) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("quiet", false, "")
	cmd.Flags().String("template", template, "")
	cmd.Flags().String("dir", dir, "")
	cmd.Flags().Bool("force", force, "")
	return cmd
}

// TestWorkflowInitWritesARegisterableFile closes the stranger-test loop in a
// unit test: what `init` writes, `register` accepts.
func TestWorkflowInitWritesARegisterableFile(t *testing.T) {
	conn := newTestDB(t)

	for _, name := range workflow.TemplateNames() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := workflowInitCmdWith(name, dir, false)
			w, _ := bufWriter(true)
			err := runWorkflowInit(cmd, nil, w)
			testsupport.Must(t, err, "init: %v", err)

			path := filepath.Join(dir, name+".toml")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("init wrote nothing at %s: %v", path, err)
			}

			rcmd := workflowRegisterCmdWithDB(conn)
			rw, _ := bufWriter(true)
			err = runWorkflowRegister(rcmd, []string{path}, rw)
			testsupport.Must(t, err, "registering what init wrote: %v", err)
		})
	}
}

// TestWorkflowInitRefusesToOverwrite: the refusal is CONFLICT naming the path.
func TestWorkflowInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()

	cmd := workflowInitCmdWith("standard-dev", dir, false)
	w, _ := bufWriter(true)
	err := runWorkflowInit(cmd, nil, w)
	testsupport.Must(t, err, "first init: %v", err)

	cmd = workflowInitCmdWith("standard-dev", dir, false)
	w, _ = bufWriter(true)
	err = runWorkflowInit(cmd, nil, w)
	if err == nil {
		t.Fatal("init overwrote an existing file without --force")
	}
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %s, want CONFLICT", got)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "standard-dev.toml")) {
		t.Errorf("the refusal %q does not name the existing path", err)
	}

	// --force overwrites.
	cmd = workflowInitCmdWith("standard-dev", dir, true)
	w, _ = bufWriter(true)
	err = runWorkflowInit(cmd, nil, w)
	testsupport.Must(t, err, "init --force: %v", err)
}

func TestWorkflowInitRejectsAnUnknownTemplate(t *testing.T) {
	cmd := workflowInitCmdWith("no-such-template", t.TempDir(), false)
	w, _ := bufWriter(true)

	err := runWorkflowInit(cmd, nil, w)
	if err == nil {
		t.Fatal("an unknown template was accepted")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want VALIDATION_ERROR", got)
	}
	// The operator is told what they could have typed instead.
	for _, name := range workflow.TemplateNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q does not list %q", err, name)
		}
	}
}

// TestWorkflowInitCreatesTheDirectoryTree: `init` works in a repo that has
// never had a config directory.
func TestWorkflowInitCreatesTheDirectoryTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deeply", "nested", "workflows")
	cmd := workflowInitCmdWith("standard-dev", dir, false)
	w, _ := bufWriter(true)

	err := runWorkflowInit(cmd, nil, w)
	testsupport.Must(t, err, "init into a missing tree: %v", err)
	if _, err := os.Stat(filepath.Join(dir, "standard-dev.toml")); err != nil {
		t.Errorf("init did not create the tree: %v", err)
	}
}
