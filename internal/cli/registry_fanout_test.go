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
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-615: cross-project targeting for the three registry-WRITING verbs.
//
// The state it exists for, measured on 2026-08-24: `workflow deprecate
// release@7` run once from one checkout retired the version in project 2 and
// left the identical row active in ten others, with nothing in the output
// saying so. Registration had the same shape. The criterion is one invocation,
// every project, and a report that says which project got which outcome —
// because the failure mode being fixed is a partial sweep that LOOKS complete.

// fanoutCmd builds a command carrying the targeting flags, since a test wiring
// a bare command never runs cobra's flag parsing (and therefore never runs
// MarkFlagsMutuallyExclusive either — which is why the refusal is also checked
// inside resolveRegistryTargets).
func fanoutCmd(conn *sql.DB, project string, all bool) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("project", project, "")
	cmd.Flags().Bool("all-projects", all, "")
	cmd.Flags().Bool("restore", false, "")
	return cmd
}

// threeProjects returns three project rows. The FIRST EnsureProject claims the
// default row, so all three are named rather than one of them being assumed.
func threeProjects(t *testing.T, conn *sql.DB) (int, int, int) {
	t.Helper()
	one, err := db.EnsureProject(conn, "/repo/one.git", "one.git", model.NowMS())
	testsupport.Must(t, err, "creating one.git: %v", err)
	two, err := db.EnsureProject(conn, "/repo/two.git", "two.git", model.NowMS())
	testsupport.Must(t, err, "creating two.git: %v", err)
	three, err := db.EnsureProject(conn, "/repo/three.git", "three.git", model.NowMS())
	testsupport.Must(t, err, "creating three.git: %v", err)
	return one, two, three
}

// fanoutReportOf parses the report out of the JSON envelope. It asserts the
// payload is the REPORT and not a single row: a caller that passed a targeting
// flag is owed every project's outcome.
func fanoutReportOf(t *testing.T, raw []byte) registryFanoutReport {
	t.Helper()
	var envelope struct {
		Data registryFanoutReport `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(envelope.Data.Results) == 0 {
		t.Fatalf("the report names no per-project results:\n%s", raw)
	}
	return envelope.Data
}

// outcomeIn finds one project's row in the report.
func outcomeIn(t *testing.T, report registryFanoutReport, projectID int) registryFanoutResult {
	t.Helper()
	for _, r := range report.Results {
		if r.ProjectID == projectID {
			return r
		}
	}
	t.Fatalf("project %d has no row in the report: %+v", projectID, report.Results)
	return registryFanoutResult{}
}

// reportedCodeOf asserts a partial failure surfaced as a reportedFailure — the
// report is already on stdout and must not be replaced by a single error
// envelope describing whichever failure was picked to stand for the rest.
func reportedCodeOf(t *testing.T, err error) output.ErrorCode {
	t.Helper()
	var rf *reportedFailure
	if !errors.As(err, &rf) {
		t.Fatalf("error %v is not a *reportedFailure, so the per-project report "+
			"was replaced by one flattened envelope", err)
		return ""
	}
	return rf.Code
}

func writeWorkflowFile(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.toml")
	testsupport.Must(t, os.WriteFile(path, []byte(src), 0o644),
		"writing the definition")
	return path
}

// registerAcross runs `workflow register` with the targeting flags set.
func registerAcross(
	t *testing.T, conn *sql.DB, src, project string, all bool,
) (registryFanoutReport, error) {
	t.Helper()
	w, buf := bufWriter(true)
	err := runWorkflowRegister(
		fanoutCmd(conn, project, all), []string{writeWorkflowFile(t, src)}, w)
	return fanoutReportOf(t, buf.Bytes()), err
}

// TestWorkflowRegisterAllProjectsLandsInEveryRegistry is the acceptance
// criterion for register: ONE invocation, a row in every project.
func TestWorkflowRegisterAllProjectsLandsInEveryRegistry(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)

	report, err := registerAcross(t, conn, minimalWorkflow, "", true)
	testsupport.Must(t, err, "register --all-projects: %v", err)

	if report.Scope != scopeAllProjects || report.Succeeded != 3 || report.Failed != 0 {
		t.Fatalf("report = %+v, want 3 succeeded in scope all-projects", report)
	}
	for _, id := range []int{one, two, three} {
		if got := outcomeIn(t, report, id).Outcome; got != outcomeRegistered {
			t.Errorf("project %d reported %q, want %q", id, got, outcomeRegistered)
		}
		// The report is a claim about the store; the store is what settles it.
		if _, err := db.GetWorkflow(conn, id, "unit", 1); err != nil {
			t.Errorf("project %d has no unit@1 row after --all-projects: %v", id, err)
		}
	}
}

// TestWorkflowRegisterAllProjectsIsIdempotentPerProject: re-registering the
// same bytes is a no-op success in each project, exactly as it is for one.
func TestWorkflowRegisterAllProjectsIsIdempotentPerProject(t *testing.T) {
	conn := newTestDB(t)
	one, _, _ := threeProjects(t, conn)

	_, err := registerAcross(t, conn, minimalWorkflow, "", true)
	testsupport.Must(t, err, "first register: %v", err)

	report, err := registerAcross(t, conn, minimalWorkflow, "", true)
	testsupport.Must(t, err, "second register: %v", err)

	if report.Failed != 0 || report.Succeeded != 3 {
		t.Fatalf("re-registering identical bytes failed somewhere: %+v", report)
	}
	if got := outcomeIn(t, report, one).Outcome; got != outcomeUnchanged {
		t.Errorf("outcome = %q, want %q", got, outcomeUnchanged)
	}
}

// TestWorkflowRegisterOneProjectsConflictDoesNotSwallowAnothersSuccess is the
// criterion's sharpest clause. A conflict is real, is reported against the
// project that has it, does not stop the sweep, and still costs the process a
// non-zero exit.
func TestWorkflowRegisterOneProjectsConflictDoesNotSwallowAnothersSuccess(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)

	// DIFFERENT bytes at the same name@version in one project only.
	_, _, err := db.InsertWorkflow(conn, &model.Workflow{
		ProjectID: two, Name: "unit", Version: 1,
		SourceSHA256: "not-the-same-hash", Body: "other bytes", Parsed: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "seeding the conflicting row: %v", err)

	report, err := registerAcross(t, conn, minimalWorkflow, "", true)
	if err == nil {
		t.Fatal("a conflicting project exited 0")
	}
	if got := reportedCodeOf(t, err); got != output.ErrConflict {
		t.Errorf("exit code = %q, want %q: every failure agreed on CONFLICT",
			got, output.ErrConflict)
	}

	if report.Succeeded != 2 || report.Failed != 1 {
		t.Fatalf("report = %+v, want 2 succeeded / 1 failed", report)
	}
	conflicted := outcomeIn(t, report, two)
	if conflicted.Outcome != outcomeConflict || conflicted.Code != output.ErrConflict {
		t.Errorf("the conflicting project reported %+v, want a CONFLICT", conflicted)
	}
	if conflicted.Detail == "" {
		t.Error("the conflict carries no detail, so the report does not say WHY")
	}
	for _, id := range []int{one, three} {
		if got := outcomeIn(t, report, id).Outcome; got != outcomeRegistered {
			t.Errorf("project %d reported %q; one project's conflict swallowed "+
				"another's success", id, got)
		}
		if _, err := db.GetWorkflow(conn, id, "unit", 1); err != nil {
			t.Errorf("project %d never got its row: %v", id, err)
		}
	}
}

// TestWorkflowRegisterProjectTargetsExactlyOne: --project writes where it is
// told and nowhere else, including not in the ambient project.
func TestWorkflowRegisterProjectTargetsExactlyOne(t *testing.T) {
	conn := newTestDB(t)
	one, two, _ := threeProjects(t, conn)

	report, err := registerAcross(t, conn, minimalWorkflow, "two.git", false)
	testsupport.Must(t, err, "register --project: %v", err)

	if report.Scope != scopeOneProject || len(report.Results) != 1 {
		t.Fatalf("report = %+v, want exactly one target", report)
	}
	if report.Results[0].ProjectID != two {
		t.Fatalf("--project wrote to project %d, want %d", report.Results[0].ProjectID, two)
	}
	if _, err := db.GetWorkflow(conn, one, "unit", 1); !errors.Is(err, db.ErrWorkflowNotFound) {
		t.Errorf("--project also wrote to the ambient project: %v", err)
	}
}

// TestWorkflowRegisterValidatesAgainstEachTargetProject is the semantics call
// this issue actually turns on. `payload` resolves in the registry of the
// project being WRITTEN TO, so the same bytes are valid in a project holding
// the schema and invalid in one that does not — and the invalid project is
// refused THERE rather than the whole sweep being decided by the invoking
// project's registry.
func TestWorkflowRegisterValidatesAgainstEachTargetProject(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)

	_, _, err := db.InsertSchema(conn, &model.Schema{
		ProjectID: two, Name: "findings", Version: 1,
		SourceSHA256: "sha", Body: findingsSchema, Ordered: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "seeding the schema in two.git: %v", err)

	const needsSchema = `
[pipeline]
name = "reads"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
payload = "findings@1"
`

	report, err := registerAcross(t, conn, needsSchema, "", true)
	if err == nil {
		t.Fatal("projects with no findings@1 registered the definition anyway")
	}
	if got := reportedCodeOf(t, err); got != output.ErrValidation {
		t.Errorf("exit code = %q, want %q", got, output.ErrValidation)
	}

	if got := outcomeIn(t, report, two).Outcome; got != outcomeRegistered {
		t.Errorf("the project holding the schema reported %q, want %q",
			got, outcomeRegistered)
	}
	for _, id := range []int{one, three} {
		got := outcomeIn(t, report, id)
		if got.Outcome != outcomeInvalid || got.Code != output.ErrValidation {
			t.Errorf("project %d reported %+v, want invalid: the schema its "+
				"`payload` names is not in ITS registry", id, got)
		}
		if _, err := db.GetWorkflow(conn, id, "reads", 1); !errors.Is(err, db.ErrWorkflowNotFound) {
			t.Errorf("project %d stored a definition whose payload it cannot "+
				"resolve; it would fail at activation instead: %v", id, err)
		}
	}
}

// TestWorkflowDeprecateAllProjectsRetiresEverywhere is DKT-615's originating
// case, verbatim: one deprecate, every project's identical row.
func TestWorkflowDeprecateAllProjectsRetiresEverywhere(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)
	for _, id := range []int{one, two, three} {
		auditWorkflowRow(t, conn, id, "release", 7)
	}

	w, buf := bufWriter(true)
	err := runWorkflowDeprecate(fanoutCmd(conn, "", true), []string{"release@7"}, w)
	testsupport.Must(t, err, "deprecate --all-projects: %v", err)

	report := fanoutReportOf(t, buf.Bytes())
	if report.Succeeded != 3 || report.Failed != 0 {
		t.Fatalf("report = %+v, want 3 retired", report)
	}
	for _, id := range []int{one, two, three} {
		if got := outcomeIn(t, report, id).Outcome; got != outcomeDeprecated {
			t.Errorf("project %d reported %q, want %q", id, got, outcomeDeprecated)
		}
		wf, err := db.GetWorkflow(conn, id, "release", 7)
		testsupport.Must(t, err, "reading release@7 back from project %d: %v", id, err)
		if !wf.Deprecated() {
			t.Errorf("project %d still binds release@7 after --all-projects", id)
		}
	}
}

// TestWorkflowDeprecateAllProjectsReportsMixedOutcomes: a sweep across a store
// meets projects in different states, and each one's own rule decides its own
// row — not-registered where nothing was registered, already-deprecated where
// the work was already done, retired where it was not.
func TestWorkflowDeprecateAllProjectsReportsMixedOutcomes(t *testing.T) {
	conn := newTestDB(t)
	fresh, retired, absent := threeProjects(t, conn)
	auditWorkflowRow(t, conn, fresh, "release", 7)
	auditWorkflowRow(t, conn, retired, "release", 7)
	_, err := db.DeprecateWorkflow(conn, retired, "release", 7, model.NowMS())
	testsupport.Must(t, err, "pre-retiring in one project: %v", err)

	w, buf := bufWriter(true)
	err = runWorkflowDeprecate(fanoutCmd(conn, "", true), []string{"release@7"}, w)
	if err == nil {
		t.Fatal("a sweep meeting two refusals exited 0")
	}
	// The two failures disagree — CONFLICT and NOT_FOUND — so no single code is
	// honest and the process exits GENERAL_ERROR, pointing at the report.
	if got := reportedCodeOf(t, err); got != output.ErrGeneral {
		t.Errorf("exit code = %q, want %q for mixed failure codes",
			got, output.ErrGeneral)
	}

	report := fanoutReportOf(t, buf.Bytes())
	if report.Succeeded != 1 || report.Failed != 2 {
		t.Fatalf("report = %+v, want 1 retired / 2 refused", report)
	}
	if got := outcomeIn(t, report, fresh).Outcome; got != outcomeDeprecated {
		t.Errorf("the fresh project reported %q, want %q", got, outcomeDeprecated)
	}
	if got := outcomeIn(t, report, retired); got.Outcome != outcomeAlreadyDeprecated ||
		got.Code != output.ErrConflict {
		t.Errorf("the already-retired project reported %+v, want already-deprecated "+
			"carrying CONFLICT — the single-project rule, applied there", got)
	}
	if got := outcomeIn(t, report, absent); got.Outcome != outcomeNotFound ||
		got.Code != output.ErrNotFound {
		t.Errorf("the project that never registered it reported %+v, want "+
			"not-registered carrying NOT_FOUND", got)
	}
}

// TestWorkflowRestoreAllProjectsKeepsTheIdempotencyAsymmetry: restore is
// idempotent where deprecate is not, and the report shows that rather than
// smoothing it into one word.
func TestWorkflowRestoreAllProjectsKeepsTheIdempotencyAsymmetry(t *testing.T) {
	conn := newTestDB(t)
	binding, retired, _ := threeProjects(t, conn)
	auditWorkflowRow(t, conn, binding, "release", 7)
	auditWorkflowRow(t, conn, retired, "release", 7)
	_, err := db.DeprecateWorkflow(conn, retired, "release", 7, model.NowMS())
	testsupport.Must(t, err, "retiring in one project: %v", err)

	cmd := fanoutCmd(conn, "", true)
	testsupport.Must(t, cmd.Flags().Set("restore", "true"), "setting --restore")

	w, buf := bufWriter(true)
	// The third project never registered it, so the sweep still exits non-zero.
	if err := runWorkflowDeprecate(cmd, []string{"release@7"}, w); err == nil {
		t.Fatal("a restore over a project with no such row exited 0")
	}

	report := fanoutReportOf(t, buf.Bytes())
	if got := outcomeIn(t, report, retired).Outcome; got != outcomeRestored {
		t.Errorf("the retired project reported %q, want %q", got, outcomeRestored)
	}
	if got := outcomeIn(t, report, binding).Outcome; got != outcomeAlreadyBinding {
		t.Errorf("the never-retired project reported %q, want %q",
			got, outcomeAlreadyBinding)
	}
	wf, err := db.GetWorkflow(conn, retired, "release", 7)
	testsupport.Must(t, err, "reading release@7 back: %v", err)
	if wf.Deprecated() {
		t.Error("the retired project's row is still retired after --restore")
	}
}

// TestSchemaRegisterAllProjectsLandsInEveryRegistry, and its conflict half: a
// schema's frozen-bytes rule is decided per project too.
func TestSchemaRegisterAllProjectsLandsInEveryRegistry(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)

	// Different bytes at findings@1 in one project only.
	_, _, err := db.InsertSchema(conn, &model.Schema{
		ProjectID: two, Name: "findings", Version: 1,
		SourceSHA256: "not-the-same-hash", Body: `{"type":"object"}`, Ordered: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "seeding the conflicting schema: %v", err)

	w, buf := bufWriter(true)
	err = runSchemaRegister(fanoutCmd(conn, "", true),
		[]string{"findings@1", writeSchema(t, findingsSchema)}, w)
	if err == nil {
		t.Fatal("a conflicting project exited 0")
	}
	if got := reportedCodeOf(t, err); got != output.ErrConflict {
		t.Errorf("exit code = %q, want %q", got, output.ErrConflict)
	}

	report := fanoutReportOf(t, buf.Bytes())
	if report.Succeeded != 2 || report.Failed != 1 {
		t.Fatalf("report = %+v, want 2 registered / 1 conflict", report)
	}
	if got := outcomeIn(t, report, two).Outcome; got != outcomeConflict {
		t.Errorf("the conflicting project reported %q, want %q", got, outcomeConflict)
	}
	for _, id := range []int{one, three} {
		if _, err := db.GetSchema(conn, id, "findings", 1); err != nil {
			t.Errorf("project %d never got findings@1: %v", id, err)
		}
	}
}

// TestRegistryFanoutRefusesBothFlags: they both name the targets and they
// disagree. Checked in the resolver as well as by cobra, because a caller that
// builds the command directly never runs cobra's parsing.
func TestRegistryFanoutRefusesBothFlags(t *testing.T) {
	conn := newTestDB(t)
	threeProjects(t, conn)

	w, _ := bufWriter(true)
	err := runWorkflowRegister(fanoutCmd(conn, "two.git", true),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w)
	if err == nil {
		t.Fatal("--project and --all-projects were accepted together")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q", got, output.ErrValidation)
	}
}

// TestRegistryFanoutRefusesAnUnknownProject: the flag exists to say WHICH
// project, so a ref naming none is refused rather than silently falling back to
// the ambient one — which would write to a project the operator did not name.
func TestRegistryFanoutRefusesAnUnknownProject(t *testing.T) {
	conn := newTestDB(t)
	threeProjects(t, conn)

	w, _ := bufWriter(true)
	err := runWorkflowRegister(fanoutCmd(conn, "no-such-repo", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w)
	if err == nil {
		t.Fatal("an unknown --project was accepted")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("code = %q, want %q", got, output.ErrNotFound)
	}
	for _, id := range []int{1, 2, 3} {
		if _, err := db.GetWorkflow(conn, id, "unit", 1); !errors.Is(err, db.ErrWorkflowNotFound) {
			t.Errorf("a refused --project still wrote to project %d", id)
		}
	}
}

// TestRegistryWritesWithoutFlagsKeepTheirPayload: the fan-out is STRICTLY
// ADDITIVE. With neither flag the verbs emit the row they always emitted, not a
// one-element report — a consumer parsing `.data.name` keeps parsing it.
func TestRegistryWritesWithoutFlagsKeepTheirPayload(t *testing.T) {
	conn := newTestDB(t)

	w, buf := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w), "plain register")

	var envelope struct {
		Data map[string]any `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "unmarshal: %s", buf)
	if _, isReport := envelope.Data["results"]; isReport {
		t.Fatalf("a flagless register emitted the fan-out report:\n%s", buf)
	}
	if envelope.Data["name"] != "unit" {
		t.Errorf("the flagless payload is not the workflow row:\n%s", buf)
	}
}

// TestRegistryFanoutHumanReportNamesEveryProject: the operator's half. The
// whole complaint behind this issue was not knowing which projects a sweep had
// reached, so every target, its outcome, and the count have to be readable
// without --json.
func TestRegistryFanoutHumanReportNamesEveryProject(t *testing.T) {
	conn := newTestDB(t)
	_, two, _ := threeProjects(t, conn)

	_, _, err := db.InsertWorkflow(conn, &model.Workflow{
		ProjectID: two, Name: "unit", Version: 1,
		SourceSHA256: "not-the-same-hash", Body: "other bytes", Parsed: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "seeding the conflicting row: %v", err)

	w, buf := bufWriter(false)
	if err := runWorkflowRegister(fanoutCmd(conn, "", true),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w); err == nil {
		t.Fatal("the conflicting sweep exited 0")
	}

	human := buf.String()
	for _, want := range []string{
		"workflow register", "unit@1",
		"one.git", "two.git", "three.git",
		outcomeRegistered, outcomeConflict,
		"2 project(s) succeeded, 1 failed",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("the render never mentions %q:\n%s", want, human)
		}
	}
}
