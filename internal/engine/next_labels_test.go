package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// offeredRows is `next`'s offered set, rendered.
func offeredRows(t *testing.T, conn *sql.DB, runID int) []model.StepRow {
	t.Helper()
	ready, err := NewEngine().NextSteps(conn, runID, 0, model.NowMS())
	testsupport.Must(t, err, "next: %v", err)
	return ready.Steps
}

// A `next` row carries the issue's labels.
//
// Routing policy is label-keyed — doc type, security sensitivity, architecture
// gating — and a dispatcher decides BEFORE it spawns, from the row alone. Until
// this field existed the row carried no label at all, so every label-keyed rule
// fell through to its default: a doc:tdd issue routed to the PRD author's
// contract at a lower tier, and a security-labelled issue resolved identically
// to an unlabelled one, with its ceiling and never-list silently inert.
//
// Found 2026-08-06 against a live run, which is why the assertion here is on
// the RENDERED ROW rather than on the snapshot column it reads: the column was
// already correct, and the bug was entirely that nothing carried it outward.

// TestNextRowCarriesIssueLabels is the regression proper.
func TestNextRowCarriesIssueLabels(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "label me", "body", "task",
		[]string{"doc:tdd", "spec-doc"})
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	rows := offeredRows(t, conn, run.ID)
	if len(rows) == 0 {
		t.Fatal("next offered no rows")
	}

	// Labels are snapshotted SORTED (§5.1.1), so the order is asserted.
	want := []string{"doc:tdd", "spec-doc"}
	for _, row := range rows {
		if len(row.Labels) != len(want) {
			t.Fatalf("%s: labels = %v, want %v", row.Step, row.Labels, want)
		}
		for i, label := range want {
			if row.Labels[i] != label {
				t.Errorf("%s: labels = %v, want %v", row.Step, row.Labels, want)
				break
			}
		}
	}
}

// TestNextRowLabelsAreFrozenAtActivation pins WHICH source the field reads.
//
// Labels come from `run_issues.issue_snapshot`, never a live join against
// `issues` — the same discipline the context bundle keeps (§6.6). A relabel
// after activation must not change how an already-scheduled step routes, or a
// run's routing would depend on when a dispatcher happened to ask.
//
// This is the opposite choice from `scope_globs`, which readiness reads LIVE on
// purpose so an operator's mid-run collision fix takes effect. The two answer
// different questions, and this test exists so the difference stays deliberate.
func TestNextRowLabelsAreFrozenAtActivation(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "relabel me", "body", "task",
		[]string{"doc:tdd"})
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The relabel lands AFTER the freeze.
	err = db.AddLabelsToIssue(
		conn, issue, []string{"doc:prd"}, "", "tester",
	)
	testsupport.Must(t, err, "relabelling: %v", err)

	rows := offeredRows(t, conn, run.ID)
	if len(rows) == 0 {
		t.Fatal("next offered no rows")
	}
	for _, row := range rows {
		if len(row.Labels) != 1 || row.Labels[0] != "doc:tdd" {
			t.Errorf("%s: labels = %v, want the FROZEN [doc:tdd]; a live read "+
				"would have said [doc:prd]", row.Step, row.Labels)
		}
	}
}

// TestNextRowOmitsLabelsWhenIssueHasNone keeps the wire shape unchanged for
// every run that does not use labels: `omitempty` means such a row serializes
// exactly as it did before this field existed.
func TestNextRowOmitsLabelsWhenIssueHasNone(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "no labels", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	rows := offeredRows(t, conn, run.ID)
	if len(rows) == 0 {
		t.Fatal("next offered no rows")
	}
	for _, row := range rows {
		if len(row.Labels) != 0 {
			t.Errorf("%s: labels = %v, want none", row.Step, row.Labels)
		}
	}
}
