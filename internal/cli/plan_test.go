package cli

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/spf13/cobra"
)

func planCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().String("root", "", "")
	return cmd
}

type planJSON struct {
	Data struct {
		Phases []struct {
			Phase  int `json:"phase"`
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
		} `json:"phases"`
		TotalIssues int `json:"total_issues"`
	} `json:"data"`
}

func runPlanJSON(t *testing.T, conn *sql.DB) planJSON {
	t.Helper()
	cmd := planCmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runPlan(cmd, nil, w); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	var pj planJSON
	if err := json.Unmarshal(buf.Bytes(), &pj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	return pj
}

func TestPlanJSON_HydratesFilesAndDocs(t *testing.T) {
	conn := newTestDB(t)
	issueID := createIssueWithFile(t, conn, "ready", "internal/cli/plan.go")
	doc := createDoc(t, conn, "Plan Doc", "tdd", "approved")
	linkDocIssue(t, conn, doc, issueID)

	pj := runPlanJSON(t, conn)
	if len(pj.Data.Phases) != 1 {
		t.Fatalf("phases = %d, want 1", len(pj.Data.Phases))
	}
	if len(pj.Data.Phases[0].Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(pj.Data.Phases[0].Issues))
	}
	iss := pj.Data.Phases[0].Issues[0]
	if len(iss.Files) != 1 || iss.Files[0] != "internal/cli/plan.go" {
		t.Errorf("files = %v, want [internal/cli/plan.go]", iss.Files)
	}
	if len(iss.Docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(iss.Docs))
	}
	if iss.Docs[0].ID != "DOC-1" || iss.Docs[0].Type != "tdd" || iss.Docs[0].Status != "approved" || iss.Docs[0].Title != "Plan Doc" {
		t.Errorf("doc shape wrong: %+v", iss.Docs[0])
	}
}

func TestPlanJSON_FilesAndDocsEmptyAreArrays(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "no context", model.StatusTodo, model.PriorityHigh)

	cmd := planCmdWithDB(conn)
	w, buf := bufWriter(true)
	if err := runPlan(cmd, nil, w); err != nil {
		t.Fatalf("runPlan: %v", err)
	}

	var env struct {
		Data struct {
			Phases []struct {
				Issues []map[string]json.RawMessage `json:"issues"`
			} `json:"phases"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Phases) != 1 || len(env.Data.Phases[0].Issues) != 1 {
		t.Fatalf("expected 1 phase with 1 issue, got %+v", env.Data.Phases)
	}
	for _, key := range []string{"files", "docs"} {
		if got := string(env.Data.Phases[0].Issues[0][key]); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}
