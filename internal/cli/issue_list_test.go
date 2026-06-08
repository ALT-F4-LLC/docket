package cli

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/spf13/cobra"
)

func listCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("type", nil, "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().String("parent", "", "")
	cmd.Flags().Bool("roots", false, "")
	cmd.Flags().Bool("tree", false, "")
	cmd.Flags().String("sort", "", "")
	cmd.Flags().Int("limit", 50, "")
	cmd.Flags().Bool("all", false, "")
	return cmd
}

type listJSON struct {
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

func TestListJSON_HydratesFilesAndDocs(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssueWithFile(t, conn, "ready", "internal/db/doc_links.go")
	doc := createDoc(t, conn, "Docket Doc CLI", "tdd", "approved")
	linkDocIssue(t, conn, doc, issueID)

	cmd := listCmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList: %v", err)
	}

	var lj listJSON
	if err := json.Unmarshal(buf.Bytes(), &lj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(lj.Data.Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(lj.Data.Issues))
	}
	iss := lj.Data.Issues[0]
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

func TestListJSON_FilesAndDocsEmptyAreArrays(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "ready no context", model.StatusTodo, model.PriorityHigh)

	cmd := listCmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList: %v", err)
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
