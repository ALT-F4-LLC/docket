package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueStepListSpansRuns is DKT-244.
//
// The only step enumeration was --run, so a conductor holding an ISSUE — the
// thing it actually has in hand when asking "where did this get to" — paged a
// whole run's listing through an external filter, and guessed at --run/--issue
// combinations that did not exist. An issue's steps are not confined to one
// run either: a second activation mints a fresh round under a new run, and a
// listing that stopped at one run would answer a question nobody asked.
func TestIssueStepListSpansRuns(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	subject := createIssue(t, conn, "the subject", "body", "task", nil)
	other := createIssue(t, conn, "a neighbour", "body", "task", nil)

	firstRun := startRun(t, conn, subject, other)
	_, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "activate first run: %v", err)

	secondRun := startRun(t, conn, subject)
	_, err = activate(conn, secondRun.ID)
	testsupport.Must(t, err, "activate second run: %v", err)

	rows, err := IssueStepList(conn, subject, nowMS)
	testsupport.Must(t, err, "IssueStepList: %v", err)
	if len(rows) == 0 {
		t.Fatal("an issue with two activated runs listed no steps")
	}

	// Only the subject's steps, never the neighbour's — the filter is the
	// whole point of the flag.
	wantIssue := model.FormatID(subject)
	runs := map[string]int{}
	for _, row := range rows {
		if row.Issue != wantIssue {
			t.Errorf("listing leaked %s's step %s into %s's listing",
				row.Issue, row.Step, wantIssue)
		}
		if row.Run == "" {
			t.Errorf("step %s carries no run; two rounds of the same instance "+
				"are indistinguishable without it", row.Step)
		}
		runs[row.Run]++
	}

	// Both runs are represented: stopping at the first would silently answer
	// about one round while claiming to answer about the issue.
	firstLabel := model.FormatRunID(firstRun.ID)
	secondLabel := model.FormatRunID(secondRun.ID)
	if runs[firstLabel] == 0 {
		t.Errorf("no rows from %s; runs seen: %v", firstLabel, runs)
	}
	if runs[secondLabel] == 0 {
		t.Errorf("no rows from %s; runs seen: %v", secondLabel, runs)
	}

	// Rows arrive in run order, so a reader scanning top to bottom reads
	// rounds in the order they happened.
	seenSecond := false
	for _, row := range rows {
		if row.Run == secondLabel {
			seenSecond = true
		} else if seenSecond {
			t.Errorf("rows are not in run order: %s follows %s", row.Run, secondLabel)
			break
		}
	}

	// An issue nothing has stepped over lists nothing, and is not an error —
	// "no steps" is a fact, not a failure.
	empty, err := IssueStepList(conn, other+9000, nowMS)
	testsupport.Must(t, err, "IssueStepList(stepless issue): %v", err)
	if len(empty) != 0 {
		t.Errorf("a stepless issue listed %d rows", len(empty))
	}
}

// TestRunStepListNamesItsRun keeps the run-scoped listing honest about the
// field --issue introduced: it is populated there too, not only on the
// cross-run path.
func TestRunStepListNamesItsRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	rows, err := RunStepList(conn, run.ID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	if len(rows) == 0 {
		t.Fatal("an activated run listed no steps")
	}
	want := model.FormatRunID(run.ID)
	for _, row := range rows {
		if row.Run != want {
			t.Errorf("step %s reports run %q, want %q", row.Step, row.Run, want)
		}
	}
}
