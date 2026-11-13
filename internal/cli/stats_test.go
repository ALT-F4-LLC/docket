package cli

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// seedStatsIssues writes two root issues and one sub-issue, with labels on
// two of them, into the default project.
func seedStatsIssues(t *testing.T, conn *sql.DB) {
	t.Helper()
	parent, err := db.CreateIssue(conn, &model.Issue{
		Title: "parent", Status: model.StatusTodo, Priority: model.PriorityHigh,
		Kind: model.IssueKindFeature,
	}, []string{"bug"}, nil)
	testsupport.Must(t, err, "CreateIssue(parent): %v", err)
	_, err = db.CreateIssue(conn, &model.Issue{
		Title: "child", Status: model.StatusDone, Priority: model.PriorityLow,
		Kind: model.IssueKindFeature, ParentID: &parent,
	}, []string{"bug", "ui"}, nil)
	testsupport.Must(t, err, "CreateIssue(child): %v", err)
	createIssue(t, conn, "sibling", model.StatusTodo, model.PriorityHigh)
}

func TestStatsJSONReportsCountsAndLabels(t *testing.T) {
	conn := newTestDB(t)
	seedStatsIssues(t, conn)

	w, buf := bufWriter(true)
	if err := runStats(cmdWithDB(conn), nil, w); err != nil {
		t.Fatalf("runStats: %v", err)
	}

	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	const want = `{"total":3,"root_issues":2,"sub_issues":1,` +
		`"by_status":{"done":1,"todo":2},"by_priority":{"high":2,"low":1},` +
		`"labels":[{"name":"bug","count":2},{"name":"ui","count":1}]}`
	if string(env.Data) != want {
		t.Errorf("data =\n%s\nwant\n%s", env.Data, want)
	}
}

func TestStatsHumanOutputListsEverySection(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)
	seedStatsIssues(t, conn)

	w, buf := bufWriter(false)
	if err := runStats(cmdWithDB(conn), nil, w); err != nil {
		t.Fatalf("runStats: %v", err)
	}

	const want = "Overview\n" +
		"  Total issues:  3\n" +
		"  Root issues:   2\n" +
		"  Sub-issues:    1\n" +
		"\nBy Status\n" +
		"  backlog:       0\n" +
		"  todo:          2\n" +
		"  in-progress:   0\n" +
		"  review:        0\n" +
		"  done:          1\n" +
		"\nBy Priority\n" +
		"  critical:      0\n" +
		"  high:          2\n" +
		"  medium:        0\n" +
		"  low:           1\n" +
		"  none:          0\n" +
		"\nLabels\n" +
		"  bug:                 2\n" +
		"  ui:                  1\n" +
		"\n"
	if buf.String() != want {
		t.Errorf("output =\n%q\nwant\n%q", buf.String(), want)
	}
}

func TestStatsCommandResolvesItsOwnProject(t *testing.T) {
	if !verbOwnsProject(statsCmd) {
		t.Error("statsCmd lacks the verbOwnsProject annotation")
	}
}
