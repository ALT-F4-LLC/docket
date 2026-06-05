package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/spf13/cobra"
)

func nextCmdWithDB(conn *sql.DB, limit int) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("type", nil, "")
	cmd.Flags().Int("limit", limit, "")
	return cmd
}

type nextJSON struct {
	Data struct {
		Issues []struct {
			ID    string   `json:"id"`
			Files []string `json:"files"`
			Docs  []struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"docs"`
		} `json:"issues"`
		Total int `json:"total"`
	} `json:"data"`
}

func runNextJSON(t *testing.T, conn *sql.DB, limit int) nextJSON {
	t.Helper()
	cmd := nextCmdWithDB(conn, limit)
	w, buf := bufWriter(true)
	if err := runNext(cmd, nil, w); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	var nj nextJSON
	if err := json.Unmarshal(buf.Bytes(), &nj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	return nj
}

func createIssueWithFile(t *testing.T, conn *sql.DB, title, file string) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:    title,
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindFeature,
	}, nil, []string{file})
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", title, err)
	}
	return id
}

func TestNextJSON_HydratesFilesAndDocs(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssueWithFile(t, conn, "ready", "internal/db/doc_links.go")
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkDocIssue(t, conn, doc, issueID)

	nj := runNextJSON(t, conn, 10)
	if len(nj.Data.Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(nj.Data.Issues))
	}
	iss := nj.Data.Issues[0]
	if len(iss.Files) != 1 || iss.Files[0] != "internal/db/doc_links.go" {
		t.Errorf("files = %v, want [internal/db/doc_links.go]", iss.Files)
	}
	if len(iss.Docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(iss.Docs))
	}
	if iss.Docs[0].ID != "DOC-1" || iss.Docs[0].Type != "tdd" || iss.Docs[0].Status != "approved" || iss.Docs[0].Title != "Docket Doc CLI" {
		t.Errorf("doc shape wrong: %+v", iss.Docs[0])
	}
}

func TestNextJSON_FilesAndDocsEmptyAreArrays(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "ready no context", model.StatusTodo, model.PriorityHigh)

	cmd := nextCmdWithDB(conn, 10)
	w, buf := bufWriter(true)
	if err := runNext(cmd, nil, w); err != nil {
		t.Fatalf("runNext: %v", err)
	}

	var env struct {
		Data struct {
			Issues []map[string]json.RawMessage `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(env.Data.Issues))
	}
	for _, key := range []string{"files", "docs"} {
		if got := string(env.Data.Issues[0][key]); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}

func TestNextHumanTableUnchanged(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	withContext := newTestDB(t)
	a := createIssueWithFile(t, withContext, "Alpha", "internal/db/doc_links.go")
	createIssue(t, withContext, "Beta", model.StatusTodo, model.PriorityMedium)
	doc := createDoc(t, withContext, "Some Doc", "tdd", "approved")
	linkDocIssue(t, withContext, doc, a)

	without := newTestDB(t)
	createIssue(t, without, "Alpha", model.StatusTodo, model.PriorityHigh)
	createIssue(t, without, "Beta", model.StatusTodo, model.PriorityMedium)

	gotWith := runNextHuman(t, withContext)
	gotWithout := runNextHuman(t, without)

	if gotWith != gotWithout {
		t.Errorf("next human table changed by linked docs/files.\n--- with context ---\n%q\n--- without ---\n%q", gotWith, gotWithout)
	}
	if strings.Contains(gotWith, "Some Doc") || strings.Contains(gotWith, "doc_links.go") {
		t.Errorf("next human table leaked doc/file data:\n%s", gotWith)
	}
}

func runNextHuman(t *testing.T, conn *sql.DB) string {
	t.Helper()
	cmd := nextCmdWithDB(conn, 10)
	w, buf := bufWriter(false)
	if err := runNext(cmd, nil, w); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	return buf.String()
}

func TestNext_HydratesPostLimitOnly(t *testing.T) {
	conn := newTestDB(t)
	first := createIssue(t, conn, "First", model.StatusTodo, model.PriorityHigh)
	second := createIssue(t, conn, "Second", model.StatusTodo, model.PriorityLow)

	docFirst := createDoc(t, conn, "First Doc", "tdd", "approved")
	docSecond := createDoc(t, conn, "Second Doc", "ux", "draft")
	linkDocIssue(t, conn, docFirst, first)
	linkDocIssue(t, conn, docSecond, second)

	nj := runNextJSON(t, conn, 1)
	if len(nj.Data.Issues) != 1 {
		t.Fatalf("issues = %d, want 1 (limit)", len(nj.Data.Issues))
	}
	emitted := nj.Data.Issues[0]
	if emitted.ID != model.FormatID(first) {
		t.Fatalf("emitted = %s, want %s (priority order)", emitted.ID, model.FormatID(first))
	}
	if len(emitted.Docs) != 1 || emitted.Docs[0].Title != "First Doc" {
		t.Errorf("emitted issue not hydrated with its doc: %+v", emitted.Docs)
	}

	cmd := nextCmdWithDB(conn, 1)
	w, buf := bufWriter(true)
	if err := runNext(cmd, nil, w); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	if strings.Contains(buf.String(), "Second Doc") {
		t.Errorf("beyond-limit issue's doc leaked into output:\n%s", buf.String())
	}
}
