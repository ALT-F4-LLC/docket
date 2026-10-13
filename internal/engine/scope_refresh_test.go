package engine

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// refreshableStepsIn runs refreshableSteps in its own transaction, as a refresh
// verb would, and rolls it back.
func refreshableStepsIn(t *testing.T, conn *sql.DB, runID, issueID int) ([]string, error) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "beginning: %v", err)
	defer tx.Rollback()
	return refreshableSteps(tx, runID, issueID, "RUN-test")
}

// TestRefreshableStepsRefusesAMidFlightStep: the quiescence refusal lives in
// refreshableSteps itself, so a caller that discards its detail still refuses.
func TestRefreshableStepsRefusesAMidFlightStep(t *testing.T) {
	conn := mustDB(t)
	runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepClaimed, stepID)

	instances, err := refreshableStepsIn(t, conn, runID, issue)

	var straddle *refreshStraddleError
	if !errors.As(err, &straddle) {
		t.Fatalf("err = %v (instances %v), want a *refreshStraddleError", err, instances)
	}
	if want := []string{"flaky@0 (claimed)"}; !slices.Equal(straddle.Straddled, want) {
		t.Errorf("Straddled = %v, want %v", straddle.Straddled, want)
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	if instances != nil {
		t.Errorf("instances = %v alongside a refusal, want none", instances)
	}
}

// TestRefreshableStepsRefusesWhenEveryStepIsTerminal: nothing left to render is
// its own refusal, distinct from a straddle.
func TestRefreshableStepsRefusesWhenEveryStepIsTerminal(t *testing.T) {
	conn := mustDB(t)
	runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, stepID)

	_, err := refreshableStepsIn(t, conn, runID, issue)

	var terminal *refreshAllTerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("err = %v, want a *refreshAllTerminalError", err)
	}
	var straddle *refreshStraddleError
	if errors.As(err, &straddle) {
		t.Errorf("an all-terminal issue was reported as a straddle: %v", err)
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
}

// TestRefreshVerbsWordTheQuiescenceRefusals: each verb maps the shared refusal
// into its own wording, naming what it would have moved and nothing the other
// verbs move.
func TestRefreshVerbsWordTheQuiescenceRefusals(t *testing.T) {
	type verb struct {
		name string
		// run arranges a refreshable change, applies status to every step of
		// the issue, and attempts the refresh.
		run          func(t *testing.T, status string) error
		straddleText string
		terminalText string
	}
	verbs := []verb{
		{
			name: "scope",
			run: func(t *testing.T, status string) error {
				conn := mustDB(t)
				runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
				widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
				setIssueSteps(t, conn, runID, issue, status)
				_, err := refresh(conn, runID, issue)
				return err
			},
			straddleText: "paths the artifact it gates never covered",
			terminalText: "widened scope",
		},
		{
			name: "body",
			run: func(t *testing.T, status string) error {
				conn := mustDB(t)
				runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
				amend(t, conn, issue, "amended")
				setIssueSteps(t, conn, runID, issue, status)
				_, err := refreshBody(conn, runID, issue)
				return err
			},
			straddleText: "the description an executor's packet asked it to satisfy",
			terminalText: "amended description",
		},
		{
			name: "files",
			run: func(t *testing.T, status string) error {
				conn, issue, runID := twoAttachmentRun(t)
				detach(t, conn, issue, "docs/b.md")
				setIssueSteps(t, conn, runID, issue, status)
				_, err := refreshFilesAsConductor(conn, runID, issue, "detached b")
				return err
			},
			straddleText: "which files an executor's packet carries",
			terminalText: "changed list",
		},
	}

	for _, v := range verbs {
		for _, tc := range []struct {
			refusal, status string
			text            func(verb) string
		}{
			{"mid-flight", db.StepRunning, func(v verb) string { return v.straddleText }},
			{"terminal", db.StepDone, func(v verb) string { return v.terminalText }},
		} {
			t.Run(v.name+"/"+tc.refusal, func(t *testing.T) {
				err := v.run(t, tc.status)
				if code, ok := CodeOf(err); !ok || code != CodeConflict {
					t.Fatalf("error = %v (code %v), want CONFLICT", err, code)
				}
				if !strings.Contains(err.Error(), tc.text(v)) {
					t.Errorf("the %s refusal lacks its own wording %q:\n%s",
						v.name, tc.text(v), err)
				}
				for _, other := range verbs {
					if other.name != v.name && strings.Contains(err.Error(), tc.text(other)) {
						t.Errorf("the %s refusal speaks in the %s refresh's wording %q:\n%s",
							v.name, other.name, tc.text(other), err)
					}
				}
			})
		}
	}
}

func setIssueSteps(t *testing.T, conn *sql.DB, runID, issueID int, status string) {
	t.Helper()
	mustExec(t, conn, `UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ?`,
		status, runID, issueID)
}
