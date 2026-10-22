package engine

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4257: a gap-only park whose issue's depends_on prerequisites are all done
// becomes visible as retry-eligible, on `dispatch open` and as one event.

func retryEligibleEvents(t *testing.T, conn *sql.DB, runID int) []map[string]any {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: EventParkRetryEligible})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var out []map[string]any
	for _, e := range page.Events {
		var data map[string]any
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding event data")
		out = append(out, data)
	}
	return out
}

// retryFixture returns a run whose only issue depends on a prerequisite issue,
// with the issue's `one@0` step parked gap-only. The prerequisite is outside
// the run and still in backlog.
func retryFixture(t *testing.T, conn *sql.DB) (runID, issueID, prereqID, stepID int) {
	t.Helper()
	runID = claimRefusedRun(t, conn)
	testsupport.Must(t, conn.QueryRow(
		`SELECT issue_id FROM run_issues WHERE run_id = ?`, runID).Scan(&issueID), "issue id")
	// The park comes first: a claim is refused while the issue's dependencies
	// are unmet, and the edge is added after the writer parked, the order
	// RUN-158 met it in.
	stepID = stepIDByInstance(t, conn, "one@0")
	parkGapOnly(t, conn, stepID)
	prereqID = createIssue(t, conn, "prerequisite", "body", "task", nil)
	_, err := db.CreateRelation(conn, &model.Relation{
		SourceIssueID: issueID, TargetIssueID: prereqID, RelationType: model.RelationDependsOn,
	})
	testsupport.Must(t, err, "CreateRelation: %v", err)
	stampPrerequisite(t, conn, issueID, prereqID, time.Minute)
	return runID, issueID, prereqID, stepID
}

// stampPrerequisite dates the edge and the prerequisite's last change relative
// to the park, which the engine stamps with the test clock nowMS: a positive
// offset is after the park, a negative one before it.
func stampPrerequisite(t *testing.T, conn *sql.DB, issueID, prereqID int, offset time.Duration) {
	t.Helper()
	at := time.UnixMilli(nowMS).Add(offset).UTC().Format(time.RFC3339)
	execSQL(t, conn, `UPDATE issue_relations SET created_at = ? WHERE source_issue_id = ? AND target_issue_id = ?`,
		at, issueID, prereqID)
	execSQL(t, conn, `UPDATE issues SET updated_at = ? WHERE id = ?`, at, prereqID)
}

// A prerequisite that was already met before the park is not what the park
// waits on, so the park is not signalled as retry-eligible.
func TestRetryEligibleIgnoresAPrerequisiteMetBeforeThePark(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, prereqID, _ := retryFixture(t, conn)
	stampPrerequisite(t, conn, issueID, prereqID, -time.Minute)
	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`, string(model.StatusDone), prereqID)

	got, err := RetryEligibleParks(conn, runID)
	testsupport.Must(t, err, "RetryEligibleParks: %v", err)
	if len(got) != 0 {
		t.Errorf("a park whose prerequisite predates it was listed: %+v", got)
	}
	if m := openDispatch(t, conn, runID, 0, nowMS); len(m.RetryEligible) != 0 || len(retryEligibleEvents(t, conn, runID)) != 0 {
		t.Errorf("dispatch open signalled it: %+v", m.RetryEligible)
	}
}

// TestDispatchOpenListsRetryEligibleParkAndRecordsOneEvent is the acceptance
// check: park a step gap-only, add a depends_on edge to issue X, close X; then
// a park-retry-eligible event exists and dispatch open lists the step under
// retry_eligible.
func TestDispatchOpenListsRetryEligibleParkAndRecordsOneEvent(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, prereqID, stepID := retryFixture(t, conn)

	// The prerequisite is still open: the park is not eligible.
	before, err := RetryEligibleParks(conn, runID)
	testsupport.Must(t, err, "RetryEligibleParks: %v", err)
	if len(before) != 0 {
		t.Fatalf("park listed while its prerequisite is open: %+v", before)
	}

	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`, string(model.StatusDone), prereqID)

	m := openDispatch(t, conn, runID, 0, nowMS)
	if len(m.RetryEligible) != 1 {
		t.Fatalf("retry_eligible = %+v; want the one parked step", m.RetryEligible)
	}
	got := m.RetryEligible[0]
	if got.Step != model.FormatStepID(stepID) || got.Issue != model.FormatID(issueID) ||
		got.ParkClass != string(db.ParkClassGapOnly) ||
		!reflect.DeepEqual(got.DependsOn, []string{model.FormatID(prereqID)}) {
		t.Errorf("retry_eligible[0] = %+v", got)
	}

	events := retryEligibleEvents(t, conn, runID)
	if len(events) != 1 {
		t.Fatalf("park-retry-eligible events = %d; want 1", len(events))
	}
	if events[0]["step"] != model.FormatStepID(stepID) {
		t.Errorf("event names step %v; want %s", events[0]["step"], model.FormatStepID(stepID))
	}

	// Recording again for the same park adds nothing.
	_, err = recordRetryEligible(conn, runID, nowMS)
	testsupport.Must(t, err, "recordRetryEligible: %v", err)
	if n := len(retryEligibleEvents(t, conn, runID)); n != 1 {
		t.Errorf("a second record wrote %d events total; want 1", n)
	}

	// A fresh park of the same step (its row version moved) is signalled again.
	execSQL(t, conn, `UPDATE steps SET row_version = row_version + 1 WHERE id = ?`, stepID)
	_, err = recordRetryEligible(conn, runID, nowMS)
	testsupport.Must(t, err, "recordRetryEligible: %v", err)
	if n := len(retryEligibleEvents(t, conn, runID)); n != 2 {
		t.Errorf("a re-park wrote %d events total; want 2", n)
	}
}

// TestRetryEligibleExcludesOtherParksAndOpenPrerequisites: only a gap-only park
// with at least one depends_on edge, every one of them done, is eligible.
func TestRetryEligibleExcludesOtherParksAndOpenPrerequisites(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, prereqID, stepID := retryFixture(t, conn)
	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`, string(model.StatusDone), prereqID)

	eligible := func() int {
		got, err := RetryEligibleParks(conn, runID)
		testsupport.Must(t, err, "RetryEligibleParks: %v", err)
		return len(got)
	}
	if eligible() != 1 {
		t.Fatal("premise: the fixture park is not eligible")
	}

	// A second, still-open prerequisite holds it.
	other := createIssue(t, conn, "second prerequisite", "body", "task", nil)
	_, err := db.CreateRelation(conn, &model.Relation{
		SourceIssueID: issueID, TargetIssueID: other, RelationType: model.RelationDependsOn,
	})
	testsupport.Must(t, err, "CreateRelation: %v", err)
	if eligible() != 0 {
		t.Error("park listed while one of two prerequisites is open")
	}
	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`, string(model.StatusDone), other)
	if eligible() != 1 {
		t.Fatal("park not listed once both prerequisites are done")
	}

	// A different park class is not a gap wait.
	execSQL(t, conn, `UPDATE steps SET park_class = ? WHERE id = ?`,
		string(db.ParkClassGateFailed), stepID)
	if eligible() != 0 {
		t.Error("a gate-failed park was listed as retry-eligible")
	}
}

// TestRetryEligibleNeedsADependsOnEdge: a gap-only park on an issue with no
// prerequisites has nothing to wait on, so it is not a prerequisite-driven
// retry.
func TestRetryEligibleNeedsADependsOnEdge(t *testing.T) {
	conn := mustDB(t)
	runID := claimRefusedRun(t, conn)
	parkGapOnly(t, conn, stepIDByInstance(t, conn, "one@0"))

	got, err := RetryEligibleParks(conn, runID)
	testsupport.Must(t, err, "RetryEligibleParks: %v", err)
	if len(got) != 0 {
		t.Errorf("a park with no depends_on edge was listed: %+v", got)
	}
}
