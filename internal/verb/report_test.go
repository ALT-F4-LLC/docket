package verb_test

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func insertLedgerRun(t *testing.T, st store.Store, projectID int, nowMS int64) int {
	t.Helper()
	run, _, err := st.InsertRunWithContextIdempotent(projectID, "", 0, nowMS, store.RunContext{}, "")
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	return run.ID
}

// The window is the caller's project by default and the whole store with
// AllProjects; the result names which.
func TestReportExecutorsScopesToTheCallersProject(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row.
	first := ensureProject(t, st, "/repos/first", "first")
	other := ensureProject(t, st, "/repos/other", "other")
	insertLedgerRun(t, st, first, 1)
	insertLedgerRun(t, st, other, 1)
	insertLedgerRun(t, st, other, 1)
	env := projectEnv(st, "/repos/other", true)

	got, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{})
	if err != nil {
		t.Fatalf("ReportExecutors: %v", err)
	}
	if got.Ledger.Runs != 2 || got.Scope != "project" || got.Since != "" {
		t.Errorf("runs, scope, since = %d, %q, %q; want 2, project, empty", got.Ledger.Runs, got.Scope, got.Since)
	}

	all, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{AllProjects: true})
	if err != nil {
		t.Fatalf("ReportExecutors all projects: %v", err)
	}
	if all.Ledger.Runs != 3 || all.Scope != "store" {
		t.Errorf("runs, scope = %d, %q; want 3, store", all.Ledger.Runs, all.Scope)
	}
}

// Since takes a run id or an instant, echoes it in canonical form, and
// refuses anything else as a validation error naming the flag.
func TestReportExecutorsSinceBoundsTheWindow(t *testing.T) {
	st := openStore(t)
	insertLedgerRun(t, st, store.DefaultProjectID, 1)
	second := insertLedgerRun(t, st, store.DefaultProjectID, 1_788_220_800_000)
	env := projectEnv(st, "", false)

	for _, tc := range []struct {
		since     string
		wantRuns  int
		wantLabel string
	}{
		{"", 2, ""},
		{"run-2", 1, "RUN-2"},
		{"2", 1, "RUN-2"},
		{"2026-09-01", 1, "2026-09-01T00:00:00Z"},
		{"2026-09-01T00:00:01Z", 0, "2026-09-01T00:00:01Z"},
	} {
		got, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{Since: tc.since})
		if err != nil {
			t.Fatalf("ReportExecutors(since %q): %v", tc.since, err)
		}
		if got.Ledger.Runs != tc.wantRuns || got.Since != tc.wantLabel {
			t.Errorf("since %q: runs, label = %d, %q; want %d, %q",
				tc.since, got.Ledger.Runs, got.Since, tc.wantRuns, tc.wantLabel)
		}
	}
	if second != 2 {
		t.Fatalf("second run id = %d, want 2; the table above assumes it", second)
	}

	for _, raw := range []string{"yesterday", "RUN-0", "2026-13-01", "-5"} {
		_, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{Since: raw})
		requireVerbCode(t, err, output.ErrValidation, "--since")
	}
}

// A project-scoped read from an identity no project holds is refused, naming
// the caller's directory, rather than answering an empty ledger; it registers
// nothing. AllProjects reads the store regardless.
func TestReportExecutorsRefusesAnUnboundCaller(t *testing.T) {
	st := openStore(t)
	insertLedgerRun(t, st, store.DefaultProjectID, 1)
	env := projectEnv(st, "/work/elsewhere", true)

	_, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{})
	requireVerbCode(t, err, output.ErrValidation, env.Cwd)
	requireUnregistered(t, st, "/work/elsewhere")

	all, err := verb.ReportExecutors(context.Background(), env, verb.ReportExecutorsInput{AllProjects: true})
	if err != nil {
		t.Fatalf("ReportExecutors all projects: %v", err)
	}
	if all.Ledger.Runs != 1 || all.Scope != "store" {
		t.Errorf("runs, scope = %d, %q; want 1, store", all.Ledger.Runs, all.Scope)
	}
}
