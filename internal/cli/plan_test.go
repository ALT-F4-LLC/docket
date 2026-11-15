package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func planCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().StringSlice("type", nil, "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().String("root", "", "")
	return cmd
}

type planJSON struct {
	Data struct {
		Phases []struct {
			Phase  int `json:"phase"`
			Level  int `json:"level"`
			Issues []struct {
				ID        string   `json:"id"`
				Files     []string `json:"files"`
				BlockedBy []string `json:"blocked_by"`
				Docs      []struct {
					ID     string `json:"id"`
					Type   string `json:"type"`
					Title  string `json:"title"`
					Status string `json:"status"`
				} `json:"docs"`
			} `json:"issues"`
		} `json:"phases"`
		TotalIssues int `json:"total_issues"`
		TotalLevels int `json:"total_levels"`
	} `json:"data"`
}

func runPlanJSON(t *testing.T, conn *sql.DB) planJSON {
	t.Helper()
	cmd := planCmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runPlan(cmd, nil, w)
	testsupport.Must(t, err, "runPlan: %v", err)
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

func TestPlanJSON_FilterByPriority(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "high prio", model.StatusTodo, model.PriorityHigh)
	createIssue(t, conn, "low prio", model.StatusTodo, model.PriorityLow)

	cmd := planCmdWithDB(conn)
	err := cmd.Flags().Set("priority", "high")
	testsupport.Must(t, err, "Set(priority): %v", err)
	w, buf := bufWriter(true)
	err = runPlan(cmd, nil, w)
	testsupport.Must(t, err, "runPlan: %v", err)

	var pj planJSON
	if err := json.Unmarshal(buf.Bytes(), &pj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if pj.Data.TotalIssues != 1 {
		t.Fatalf("total_issues = %d, want 1", pj.Data.TotalIssues)
	}
	if len(pj.Data.Phases) != 1 || len(pj.Data.Phases[0].Issues) != 1 {
		t.Fatalf("expected 1 phase with 1 issue, got %+v", pj.Data.Phases)
	}
	if pj.Data.Phases[0].Issues[0].ID != "DKT-1" {
		t.Errorf("expected DKT-1 (high priority), got %s", pj.Data.Phases[0].Issues[0].ID)
	}
}

func TestPlanJSON_FilterByPriorityInvalid(t *testing.T) {
	conn := newTestDB(t)

	cmd := planCmdWithDB(conn)
	err := cmd.Flags().Set("priority", "not-a-priority")
	testsupport.Must(t, err, "Set(priority): %v", err)
	w, _ := bufWriter(true)
	if err := runPlan(cmd, nil, w); err == nil {
		t.Fatal("expected validation error for invalid priority, got nil")
	}
}

func TestPlanJSON_FilesAndDocsEmptyAreArrays(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "no context", model.StatusTodo, model.PriorityHigh)

	cmd := planCmdWithDB(conn)
	w, buf := bufWriter(true)
	err := runPlan(cmd, nil, w)
	testsupport.Must(t, err, "runPlan: %v", err)

	var env struct {
		Data struct {
			Phases []struct {
				Issues []map[string]json.RawMessage `json:"issues"`
			} `json:"phases"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if len(env.Data.Phases) != 1 || len(env.Data.Phases[0].Issues) != 1 {
		t.Fatalf("expected 1 phase with 1 issue, got %+v", env.Data.Phases)
	}
	for _, key := range []string{"files", "docs"} {
		if got := string(env.Data.Phases[0].Issues[0][key]); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}

func TestPlanJSON_LevelFields(t *testing.T) {
	conn := newTestDB(t)
	createIssueWithFile(t, conn, "issue one", "shared.go")
	createIssueWithFile(t, conn, "issue two", "shared.go")

	pj := runPlanJSON(t, conn)
	if len(pj.Data.Phases) != 2 {
		t.Fatalf("expected 2 phases (file collision split), got %d", len(pj.Data.Phases))
	}
	if pj.Data.TotalLevels != 1 {
		t.Errorf("total_levels = %d, want 1", pj.Data.TotalLevels)
	}
	if pj.Data.Phases[0].Level != 1 || pj.Data.Phases[1].Level != 1 {
		t.Errorf("expected both phases at level 1, got %d and %d", pj.Data.Phases[0].Level, pj.Data.Phases[1].Level)
	}
}

func TestPlanHuman_SameLevelSplitVsNewLevel(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn := newTestDB(t)

	// blocker1 and blocker2 share a file (same topo-level, file-collision
	// split into two sub-phases). blocked depends on both, forming a
	// genuine new dependency level.
	blocker1 := createIssueWithFile(t, conn, "blocker one", "shared.go")
	blocker2 := createIssueWithFile(t, conn, "blocker two", "shared.go")
	blocked := createIssue(t, conn, "blocked", model.StatusTodo, model.PriorityHigh)
	for _, blockerID := range []int{blocker1, blocker2} {
		if _, err := db.CreateRelation(conn, &model.Relation{
			SourceIssueID: blocked,
			TargetIssueID: blockerID,
			RelationType:  model.RelationDependsOn,
		}); err != nil {
			t.Fatalf("CreateRelation: %v", err)
		}
	}

	cmd := planCmdWithDB(conn)
	w, buf := bufWriter(false)
	err := runPlan(cmd, nil, w)
	testsupport.Must(t, err, "runPlan: %v", err)
	out := buf.String()

	if !strings.Contains(out, "Phase 2 (same dependency level as Phase 1, split by file collision):") {
		t.Errorf("expected same-level split header for phase 2, got:\n%s", out)
	}
	if !strings.Contains(out, "Phase 3 (parallel, after Phase 2):") {
		t.Errorf("expected genuine new-level header for phase 3, got:\n%s", out)
	}
}

func TestPlanJSON_BlockedBy(t *testing.T) {
	conn := newTestDB(t)
	blockerID := createIssue(t, conn, "blocker", model.StatusTodo, model.PriorityHigh)
	blockedID := createIssue(t, conn, "blocked", model.StatusTodo, model.PriorityHigh)
	if _, err := db.CreateRelation(conn, &model.Relation{
		SourceIssueID: blockedID,
		TargetIssueID: blockerID,
		RelationType:  model.RelationDependsOn,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	pj := runPlanJSON(t, conn)

	byID := map[string][]string{}
	for _, phase := range pj.Data.Phases {
		for _, issue := range phase.Issues {
			byID[issue.ID] = issue.BlockedBy
		}
	}

	blockedBy := byID[model.FormatID(blockedID)]
	if len(blockedBy) != 1 || blockedBy[0] != model.FormatID(blockerID) {
		t.Errorf("blocked issue's blocked_by = %v, want [%s]", blockedBy, model.FormatID(blockerID))
	}

	blockerBlockedBy, ok := byID[model.FormatID(blockerID)]
	if !ok {
		t.Fatalf("blocker issue %s not found in plan output", model.FormatID(blockerID))
	}
	if blockerBlockedBy == nil || len(blockerBlockedBy) != 0 {
		t.Errorf("blocker's blocked_by = %v, want []", blockerBlockedBy)
	}
}
