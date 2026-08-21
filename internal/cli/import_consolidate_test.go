package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The consolidation case the pre-v12 import silently mangled: two per-repo
// stores whose numeric ids overlap, merged into one shared store. The old
// INSERT OR IGNORE counted every collision under `skipped` — links and all —
// so DKT-1 of the second repo simply vanished. Remapping is the fix, and this
// test is the trap staying shut.
func TestImportConsolidatesOverlappingStores(t *testing.T) {
	seed := func(conn *sql.DB, title string) int {
		id, err := db.CreateIssue(conn, &model.Issue{
			Title:  title,
			Status: model.StatusTodo, Priority: model.PriorityNone,
			Kind: model.IssueKindTask,
		}, []string{"bug"}, nil)
		testsupport.Must(t, err, "CreateIssue(%s): %v", title, err)
		_, err = db.CreateComment(conn, &model.Comment{
			IssueID: id, Body: "comment on " + title, Author: "tester"})
		testsupport.Must(t, err, "CreateComment(%s): %v", title, err)
		return id
	}

	fetchAll := func(conn *sql.DB) *model.ExportData {
		data := &model.ExportData{Version: 1}
		for _, c := range exportCollections {
			err := c.fetch(conn, 0, data)
			testsupport.Must(t, err, "fetch %s: %v", c.name, err)
		}
		return data
	}

	// Two independent legacy stores; both hand out issue id 1, label id 1.
	srcOne, srcTwo := newTestDB(t), newTestDB(t)
	idOne := seed(srcOne, "from repo one")
	idTwo := seed(srcTwo, "from repo two")
	if idOne != idTwo {
		t.Fatalf("test premise broken: source ids differ (%d vs %d)", idOne, idTwo)
	}

	// One shared store, two projects.
	dst := newTestDB(t)
	projectOne, err := db.EnsureProject(dst, "/repo/one", "one", 1)
	testsupport.Must(t, err, "EnsureProject one: %v", err)
	projectTwo, err := db.EnsureProject(dst, "/repo/two", "two", 2)
	testsupport.Must(t, err, "EnsureProject two: %v", err)

	first, err := doImport(dst, fetchAll(srcOne), false, projectOne)
	testsupport.Must(t, err, "importing repo one: %v", err)
	if first.Remapped != 0 {
		t.Errorf("the first import into an empty store remapped %d rows; free ids must be kept", first.Remapped)
	}

	second, err := doImport(dst, fetchAll(srcTwo), false, projectTwo)
	testsupport.Must(t, err, "importing repo two: %v", err)
	if second.Remapped == 0 {
		t.Error("the second import remapped nothing despite colliding ids")
	}
	if second.Skipped != 0 {
		t.Errorf("the second import skipped %d rows; consolidation must drop NOTHING", second.Skipped)
	}

	// Both issues exist, each in its own project, each with its comment
	// following the (possibly remapped) id.
	for _, want := range []struct {
		project int
		title   string
	}{{projectOne, "from repo one"}, {projectTwo, "from repo two"}} {
		issues, _, err := db.ListIssues(dst, db.ListOptions{ProjectID: want.project, IncludeDone: true})
		testsupport.Must(t, err, "ListIssues(%d): %v", want.project, err)
		if len(issues) != 1 || issues[0].Title != want.title {
			t.Fatalf("project %d lists %+v, want exactly %q", want.project, issues, want.title)
		}
		comments, err := db.ListComments(dst, issues[0].ID)
		testsupport.Must(t, err, "ListComments: %v", err)
		if len(comments) != 1 || comments[0].Body != "comment on "+want.title {
			t.Errorf("project %d's issue lost its comment across the remap: %+v", want.project, comments)
		}
	}
}
