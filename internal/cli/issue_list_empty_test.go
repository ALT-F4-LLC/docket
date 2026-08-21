package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestEmptyListStateDistinguishesNoneFromNoneOpen is DKT-246.
//
// The default listing hides done issues, so an empty result proves only that
// nothing is OPEN. Printing "No issues found. Create one with: docket issue
// create" over a project holding dozens of done issues is a false claim about
// the store — it read as store damage and cost a --all round trip to disprove.
func TestEmptyListStateDistinguishesNoneFromNoneOpen(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	opts := db.ListOptions{ProjectID: db.DefaultProjectID, Limit: 50}

	// A genuinely empty store keeps the create hint: there is nothing hidden,
	// so "create one" is the true next move.
	empty := emptyListState(conn, opts, false)
	if !strings.Contains(empty, "No issues found.") ||
		!strings.Contains(empty, "docket issue create") {
		t.Errorf("an empty store should still say No issues found and offer create:\n%s", empty)
	}

	// Two done issues, nothing open.
	for _, title := range []string{"first", "second"} {
		id, err := db.CreateIssue(conn, &model.Issue{
			Title:  title,
			Status: model.StatusDone,
			Kind:   model.IssueKindTask,
		}, nil, nil)
		testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
		_ = id
	}

	hidden := emptyListState(conn, opts, false)
	if strings.Contains(hidden, "No issues found.") {
		t.Errorf("done issues are being hidden but the hint still claims the "+
			"store is empty — the exact misread DKT-246 filed:\n%s", hidden)
	}
	if !strings.Contains(hidden, "2 done issues") {
		t.Errorf("the hint does not say how many are hidden:\n%s", hidden)
	}
	if !strings.Contains(hidden, "--all") {
		t.Errorf("the hint does not name the flag that shows them:\n%s", hidden)
	}

	// --all already includes done, so an empty result under it is genuinely
	// empty and must not blame a filter that is not applied.
	withAll := emptyListState(conn, opts, true)
	if !strings.Contains(withAll, "No issues found.") {
		t.Errorf("under --all the hidden-done hint is a lie:\n%s", withAll)
	}

	// A filter that matches nothing even WITH done included is not a
	// hidden-done situation either.
	filtered := emptyListState(conn, db.ListOptions{
		ProjectID: db.DefaultProjectID,
		Labels:    []string{"no-such-label"},
		Limit:     50,
	}, false)
	if !strings.Contains(filtered, "No issues found.") {
		t.Errorf("a filter matching nothing at all reported hidden done issues:\n%s", filtered)
	}
}

// TestPluralIssuesReadsNaturally keeps the count legible at one.
func TestPluralIssuesReadsNaturally(t *testing.T) {
	if got := pluralIssues(1); got != "1 done issue" {
		t.Errorf("pluralIssues(1) = %q", got)
	}
	if got := pluralIssues(7); got != "7 done issues" {
		t.Errorf("pluralIssues(7) = %q", got)
	}
}
