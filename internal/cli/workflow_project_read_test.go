package cli

import (
	"context"
	"database/sql"
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

// --project on the two registry-READING verbs, `workflow list` and `workflow
// lint`. The state it exists for: a store-wide reconcile can WRITE every
// project from one checkout, but reading each project's registry state needed
// a process started inside that project's checkout — and a project whose
// checkout is missing from this machine could not be read at all.
//
// The criterion in every test below is the same: the flag's answer is the
// answer the same command gives when run FROM that project's checkout, which
// these tests stand in for by placing the project in the command's context the
// way the root hook does.

// inCheckoutOf builds a command as if the working directory resolved to
// projectID, with no --project flag set.
func inCheckoutOf(cmd *cobra.Command, projectID int) *cobra.Command {
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, projectID))
	return cmd
}

func workflowListJSON(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	w, buf := bufWriter(true)
	testsupport.Must(t, runWorkflowList(cmd, nil, w), "list")
	return buf.String()
}

func lintWith(t *testing.T, cmd *cobra.Command, src string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.toml")
	testsupport.Must(t, os.WriteFile(path, []byte(src), 0o644), "writing the definition")
	w, buf := bufWriter(true)
	err := runWorkflowLint(cmd, []string{path}, w)
	return buf.String(), err
}

func lintCmd(conn *sql.DB, project string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("project", project, "")
	return cmd
}

// TestWorkflowListProjectReadsAnotherRegistry: from a checkout bound to A,
// `--project B --deprecated` returns B's rows, retired ones included, and
// matches the same command run from B's checkout.
func TestWorkflowListProjectReadsAnotherRegistry(t *testing.T) {
	conn := newTestDB(t)
	a, b, _ := threeProjects(t, conn)

	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "two.git", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w), "registering unit@1 in B")
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "two.git", false),
		[]string{writeWorkflowFile(t, strings.Replace(minimalWorkflow, "version = 1", "version = 2", 1))}, w),
		"registering unit@2 in B")
	_, err := db.DeprecateWorkflow(conn, b, "unit", 2, model.NowMS())
	testsupport.Must(t, err, "retiring unit@2 in B: %v", err)

	// A holds nothing, and says so without the flag.
	if out := workflowListJSON(t, inCheckoutOf(workflowListCmdWithDB(conn, 50), a)); strings.Contains(out, `"unit"`) {
		t.Fatalf("A's own listing shows B's rows:\n%s", out)
	}

	// From A, --project B --deprecated is what B's checkout would print.
	fromA := workflowListCmdWithDB(conn, 50)
	testsupport.Must(t, fromA.Flags().Set("project", "two.git"), "setting --project")
	testsupport.Must(t, fromA.Flags().Set("deprecated", "true"), "setting --deprecated")
	viaFlag := workflowListJSON(t, inCheckoutOf(fromA, a))

	fromB := workflowListCmdWithDB(conn, 50)
	testsupport.Must(t, fromB.Flags().Set("deprecated", "true"), "setting --deprecated")
	inB := workflowListJSON(t, inCheckoutOf(fromB, b))

	if viaFlag != inB {
		t.Errorf("--project B from A differs from B's own listing:\n%s\n---\n%s", viaFlag, inB)
	}
	for _, want := range []string{`"version":1`, `"version":2`, `"total":2`} {
		if !strings.Contains(viaFlag, want) {
			t.Errorf("the --project B listing lacks %s:\n%s", want, viaFlag)
		}
	}

	// Without --deprecated the retired version is hidden there too: the
	// project changes which registry is read, not how it is read.
	fromA = workflowListCmdWithDB(conn, 50)
	testsupport.Must(t, fromA.Flags().Set("project", "two.git"), "setting --project")
	if out := workflowListJSON(t, inCheckoutOf(fromA, a)); strings.Contains(out, `"version":2`) || !strings.Contains(out, `"total":1`) {
		t.Errorf("--project B without --deprecated shows the retired version:\n%s", out)
	}

	// The ref forms are the write verbs': prefix, name, identity, id all
	// reach the same registry.
	for _, ref := range []string{"two.git", "/repo/two.git"} {
		cmd := workflowListCmdWithDB(conn, 50)
		testsupport.Must(t, cmd.Flags().Set("project", ref), "setting --project")
		testsupport.Must(t, cmd.Flags().Set("deprecated", "true"), "setting --deprecated")
		if out := workflowListJSON(t, inCheckoutOf(cmd, a)); out != inB {
			t.Errorf("--project %s resolves differently from two.git:\n%s", ref, out)
		}
	}
}

// TestWorkflowLintProjectJudgesAgainstAnotherRegistry: the verdict --project B
// returns is the one B's checkout returns — a CONFLICT present only in B and a
// schema reference that resolves only in B.
func TestWorkflowLintProjectJudgesAgainstAnotherRegistry(t *testing.T) {
	conn := newTestDB(t)
	a, b, _ := threeProjects(t, conn)

	// B alone holds findings@1 and a registered reads@1.
	_, _, err := db.InsertSchema(conn, &model.Schema{
		ProjectID: b, Name: "findings", Version: 1,
		SourceSHA256: "sha", Body: findingsSchema, Ordered: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "seeding findings@1 in B: %v", err)
	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "two.git", false),
		[]string{writeWorkflowFile(t, readsFindings)}, w), "registering reads@1 in B")

	// From A without the flag: the schema does not resolve here.
	_, err = lintWith(t, inCheckoutOf(lintCmd(conn, ""), a), readsFindings)
	if err == nil || codeOf(t, err) != output.ErrValidation {
		t.Fatalf("lint in A of a definition whose schema lives in B = %v, want VALIDATION_ERROR", err)
	}

	// From A with --project B: B's verdict, unchanged.
	viaFlag, err := lintWith(t, inCheckoutOf(lintCmd(conn, "two.git"), a), readsFindings)
	testsupport.Must(t, err, "lint --project B: %v", err)
	inB, _ := lintWith(t, inCheckoutOf(lintCmd(conn, ""), b), readsFindings)
	if viaFlag != inB {
		t.Errorf("--project B from A differs from B's own verdict:\n%s\n---\n%s", viaFlag, inB)
	}
	if !strings.Contains(viaFlag, `"registration":"unchanged"`) {
		t.Errorf("the verdict does not report B's registration as unchanged:\n%s", viaFlag)
	}

	// An edit at the frozen slot is a CONFLICT in B only. From A it would be
	// `new` (A holds no reads@1) — and would also fail on the schema — so the
	// flag is the only way to see B's conflict from A.
	edited := readsFindings + "\n# edited\n"
	_, err = lintWith(t, inCheckoutOf(lintCmd(conn, "two.git"), a), edited)
	if err == nil || codeOf(t, err) != output.ErrConflict {
		t.Errorf("lint --project B of an edit at B's frozen slot = %v, want CONFLICT", err)
	}
	_, errB := lintWith(t, inCheckoutOf(lintCmd(conn, ""), b), edited)
	if errB == nil || err.Error() != errB.Error() {
		t.Errorf("--project B's conflict differs from B's own:\n%v\n---\n%v", err, errB)
	}

	// Lint wrote nothing anywhere.
	if n := countWorkflows(t, conn); n != 1 {
		t.Errorf("the registry holds %d workflows after linting, want the 1 registered", n)
	}
}

// TestWorkflowReadVerbsRefuseAnUnknownProject: the same error the write verbs
// give, because it is the same resolver.
func TestWorkflowReadVerbsRefuseAnUnknownProject(t *testing.T) {
	conn := newTestDB(t)
	threeProjects(t, conn)

	w, _ := bufWriter(true)
	writeErr := runWorkflowRegister(fanoutCmd(conn, "no-such-repo", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w)
	if writeErr == nil {
		t.Fatal("the write verb accepted an unknown --project")
	}

	listCmd := workflowListCmdWithDB(conn, 50)
	testsupport.Must(t, listCmd.Flags().Set("project", "no-such-repo"), "setting --project")
	w, _ = bufWriter(true)
	listErr := runWorkflowList(listCmd, nil, w)
	_, lintErr := lintWith(t, lintCmd(conn, "no-such-repo"), minimalWorkflow)

	for name, err := range map[string]error{"list": listErr, "lint": lintErr} {
		if err == nil {
			t.Errorf("%s accepted an unknown --project", name)
			continue
		}
		if codeOf(t, err) != codeOf(t, writeErr) || err.Error() != writeErr.Error() {
			t.Errorf("%s's refusal differs from the write verbs':\n%v\n---\n%v", name, err, writeErr)
		}
	}
}

// TestWorkflowLintAllProjectsReportsEachProjectsVerdict: one lint, every
// project, each judged on its own registry — new, unchanged, conflict, and
// invalid side by side — in the report shape `workflow register
// --all-projects` writes, and nothing written anywhere.
func TestWorkflowLintAllProjectsReportsEachProjectsVerdict(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)
	four, err := db.EnsureProject(conn, "/repo/four.git", "four.git", model.NowMS())
	testsupport.Must(t, err, "creating four.git: %v", err)

	// findings@1 everywhere but four.git; reads@1 registered in two.git with
	// these bytes and in three.git with different ones.
	for _, id := range []int{one, two, three} {
		_, _, err := db.InsertSchema(conn, &model.Schema{
			ProjectID: id, Name: "findings", Version: 1,
			SourceSHA256: "sha", Body: findingsSchema, Ordered: "{}",
		}, model.NowMS())
		testsupport.Must(t, err, "seeding findings@1 in project %d: %v", id, err)
	}
	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "two.git", false),
		[]string{writeWorkflowFile(t, readsFindings)}, w), "registering reads@1 in two.git")
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "three.git", false),
		[]string{writeWorkflowFile(t, readsFindings+"\n# other bytes\n")}, w),
		"registering different reads@1 bytes in three.git")
	before := countWorkflows(t, conn)

	cmd := lintCmd(conn, "")
	cmd.Flags().Bool("all-projects", true, "")
	path := filepath.Join(t.TempDir(), "wf.toml")
	testsupport.Must(t, os.WriteFile(path, []byte(readsFindings), 0o644), "writing the definition")
	w, buf := bufWriter(true)
	err = runWorkflowLint(cmd, []string{path}, w)
	if err == nil {
		t.Fatal("a lint with a conflict and an invalid project exited clean")
	}
	if got := reportedCodeOf(t, err); got != output.ErrGeneral {
		t.Errorf("exit code = %q, want %q for mixed CONFLICT and VALIDATION_ERROR", got, output.ErrGeneral)
	}
	report := fanoutReportOf(t, buf.Bytes())
	if report.Operation != "workflow lint" || report.Scope != scopeAllProjects {
		t.Errorf("report header = %q / %q", report.Operation, report.Scope)
	}

	for _, tc := range []struct {
		project int
		outcome string
		code    output.ErrorCode
	}{
		{one, outcomeNew, ""},
		{two, outcomeUnchanged, ""},
		{three, outcomeConflict, output.ErrConflict},
		{four, outcomeInvalid, output.ErrValidation},
	} {
		got := outcomeIn(t, report, tc.project)
		if got.Outcome != tc.outcome || got.Code != tc.code {
			t.Errorf("project %d reported %+v, want %s / %q", tc.project, got, tc.outcome, tc.code)
		}
	}
	if got := outcomeIn(t, report, three); !strings.Contains(got.Detail, "bump [pipeline].version to 2") {
		t.Errorf("the conflict row does not carry lint's remedy: %+v", got)
	}
	if n := countWorkflows(t, conn); n != before {
		t.Errorf("lint --all-projects changed the registry from %d to %d rows", before, n)
	}

	// --project and --all-projects together are refused by cobra's
	// mutual exclusion on the real command.
	root := workflowLintCmd
	if root.Flags().Lookup("all-projects") == nil || root.Flags().Lookup("project") == nil {
		t.Error("workflow lint does not declare both targeting flags")
	}
}
