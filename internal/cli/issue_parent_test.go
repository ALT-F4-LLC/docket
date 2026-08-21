package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// otherProject mints a second project in a fresh store. EnsureProject claims
// the unclaimed default row in place before it inserts, so two calls are what
// it takes to get a project that is NOT the default one.
func otherProject(t *testing.T, conn *sql.DB) int {
	t.Helper()
	_, err := db.EnsureProject(conn, "/repo/default", "default", model.NowMS())
	testsupport.Must(t, err, "claiming the default project: %v", err)
	id, err := db.EnsureProject(conn, "/repo/other", "other", model.NowMS())
	testsupport.Must(t, err, "creating the other project: %v", err)
	if id == db.DefaultProjectID {
		t.Fatalf("other project = %d, want a non-default id", id)
	}
	return id
}

func createIssueInProject(t *testing.T, conn *sql.DB, projectID int, title string) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &model.Issue{
		ProjectID: projectID,
		Title:     title,
		Status:    model.StatusBacklog,
		Priority:  model.PriorityNone,
		Kind:      model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	return id
}

// TestResolveParentIssueProjectRules pins DKT-22: a prospective parent from
// another project is refused, because a subtree spanning projects breaks
// everything built on subtree homogeneity (`issue move --project` migrates
// whole subtrees; per-project tree rendering can't reach a foreign child).
// The comparison is issue-to-issue, so a parent still resolves freely within
// its OWN project whatever the invoking cwd's project is.
func TestResolveParentIssueProjectRules(t *testing.T) {
	conn := newTestDB(t)
	other := otherProject(t, conn)
	local := createIssueInProject(t, conn, db.DefaultProjectID, "local parent")
	foreign := createIssueInProject(t, conn, other, "foreign parent")

	got, err := resolveParentIssue(conn, model.FormatID(local), db.DefaultProjectID)
	testsupport.Must(t, err, "same-project parent: %v", err)
	if got.ID != local {
		t.Errorf("resolved parent %d, want %d", got.ID, local)
	}

	if _, err := resolveParentIssue(conn, model.FormatID(foreign), other); err != nil {
		t.Errorf("foreign parent within its own project: %v, want success", err)
	}

	cases := []struct {
		name  string
		ref   string
		child int
		want  output.ErrorCode
	}{
		{"cross-project parent", model.FormatID(foreign), db.DefaultProjectID, output.ErrValidation},
		{"missing parent", "DKT-999", db.DefaultProjectID, output.ErrNotFound},
		{"malformed reference", "nope", db.DefaultProjectID, output.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveParentIssue(conn, tc.ref, tc.child)
			if err == nil {
				t.Fatal("resolved, want a refusal")
			}
			if got := codeOf(t, err); got != tc.want {
				t.Errorf("code = %s, want %s", got, tc.want)
			}
		})
	}
}
