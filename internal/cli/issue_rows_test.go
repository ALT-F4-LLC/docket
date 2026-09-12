package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-1053: list-shaped issue verbs emit summary rows.
//
// These tests are the issue-side twin of doc_list_test.go (DKT-1045): the
// same corpus shape — a dozen rows with real multi-kilobyte bodies — measured
// on every verb that lists issues, plus the promise that makes the trim safe:
// `issue show` still carries the whole description.

// listCmdWithBody is listCmdWithDB plus the --with-body flag, set as asked.
func listCmdWithBody(conn *sql.DB, withBody bool) *cobra.Command {
	cmd := listCmdWithDB(conn)
	cmd.Flags().Bool("with-body", withBody, "")
	return cmd
}

// boardCmdWithDB registers the flags runBoard reads.
func boardCmdWithDB(conn *sql.DB, withBody bool) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().String("assignee", "", "")
	cmd.Flags().Bool("expand", false, "")
	cmd.Flags().Bool("with-body", withBody, "")
	return cmd
}

// issueDescription is a stand-in for a real issue body: the claim, remedy and
// acceptance criteria a planned issue actually carries. Size is what makes the
// corpus a reproduction of DKT-1053 rather than a shape test, so it is long.
func issueDescription(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Claim %d\n\n", n)
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "Criterion %d: the conductor dispatched the wave, the executor "+
			"reported, and the gate seated a panel of three judges.\n", i)
	}
	return b.String()
}

// seedDescribedIssues creates 11 todo feature issues with real descriptions,
// plus one bug so the type filter has something to exclude and one done issue
// so the default listing has something to hide. It returns the 11
// descriptions in id order.
func seedDescribedIssues(t *testing.T, conn *sql.DB) []string {
	t.Helper()

	descriptions := make([]string, 0, 11)
	for i := 1; i <= 11; i++ {
		desc := issueDescription(i)
		descriptions = append(descriptions, desc)
		_, err := db.CreateIssue(conn, &model.Issue{
			Title:       fmt.Sprintf("Planned issue %d", i),
			Description: desc,
			Status:      model.StatusTodo,
			Priority:    model.PriorityHigh,
			Kind:        model.IssueKindFeature,
		}, nil, nil)
		testsupport.Must(t, err, "CreateIssue(%d): %v", i, err)
	}

	_, err := db.CreateIssue(conn, &model.Issue{
		Title:       "Not a feature",
		Description: issueDescription(98),
		Status:      model.StatusTodo,
		Priority:    model.PriorityLow,
		Kind:        model.IssueKindBug,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue(bug): %v", err)

	_, err = db.CreateIssue(conn, &model.Issue{
		Title:       "Already finished",
		Description: issueDescription(99),
		Status:      model.StatusDone,
		Priority:    model.PriorityLow,
		Kind:        model.IssueKindFeature,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue(done): %v", err)

	return descriptions
}

// summaryRowKeys is every key a summary row must carry: the frozen v1 keys
// minus `description`, plus `description_bytes`.
var summaryRowKeys = []string{
	"id", "issue", "title", "description_bytes", "status", "priority", "kind",
	"assignee", "labels", "files", "docs", "created_at", "updated_at",
}

// assertSummaryRows checks that every row is a summary row whose
// description_bytes matches the description it does not carry.
func assertSummaryRows(t *testing.T, rows []map[string]json.RawMessage, descriptions []string) {
	t.Helper()
	if len(rows) != len(descriptions) {
		t.Fatalf("rows = %d, want %d", len(rows), len(descriptions))
	}
	for i, row := range rows {
		if _, ok := row["description"]; ok {
			t.Errorf("row %d carries a description key; listings must be description-free", i)
		}
		for _, key := range summaryRowKeys {
			if _, ok := row[key]; !ok {
				t.Errorf("row %d is missing %q; got keys %v", i, key, keysOf(row))
			}
		}
		var descBytes int
		if err := json.Unmarshal(row["description_bytes"], &descBytes); err != nil {
			t.Fatalf("row %d description_bytes: %v", i, err)
		}
		if descBytes != len(descriptions[i]) {
			t.Errorf("row %d description_bytes = %d, want %d", i, descBytes, len(descriptions[i]))
		}
	}
}

// assertFullRows checks that every row is the pre-DKT-1053 shape: the whole
// description present, and no description_bytes standing in for it.
func assertFullRows(t *testing.T, rows []map[string]json.RawMessage, descriptions []string) {
	t.Helper()
	if len(rows) != len(descriptions) {
		t.Fatalf("rows = %d, want %d", len(rows), len(descriptions))
	}
	for i, row := range rows {
		if _, ok := row["description_bytes"]; ok {
			t.Errorf("row %d carries description_bytes under --with-body; the full shape has no such key", i)
		}
		var desc string
		if err := json.Unmarshal(row["description"], &desc); err != nil {
			t.Fatalf("row %d description: %v", i, err)
		}
		if desc != descriptions[i] {
			t.Errorf("row %d description = %d bytes, want the full %d-byte description",
				i, len(desc), len(descriptions[i]))
		}
	}
}

// TestIssueListJSON_IsSummaryRows is DKT-1053's acceptance criterion,
// measured: a type-filtered `issue list --json` over eleven issues with real
// descriptions stays under DKT-1045's 4KB bar, and no row carries a
// description. Before the fix the same corpus produced tens of kilobytes.
func TestIssueListJSON_IsSummaryRows(t *testing.T) {
	conn := newTestDB(t)
	descriptions := seedDescribedIssues(t, conn)

	cmd := listCmdWithBody(conn, false)
	setFlags(t, cmd, map[string]string{"type": "feature", "status": "todo", "sort": "id:asc"})
	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList: %v", err)
	}

	if buf.Len() >= 4096 {
		t.Errorf("issue list -T feature -s todo --json = %d bytes, want < 4096", buf.Len())
	}

	var env struct {
		Data struct {
			Issues []map[string]json.RawMessage `json:"issues"`
			Total  int                          `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Total != 11 {
		t.Errorf("total = %d, want 11", env.Data.Total)
	}
	assertSummaryRows(t, env.Data.Issues, descriptions)

	// The description is absent, not emptied: no row may smuggle it back.
	if strings.Contains(buf.String(), "the conductor dispatched the wave") {
		t.Error("listing output contains description prose; the description must live in issue show")
	}
}

// TestIssueListJSON_WithBodyRestoresDescriptions pins the escape hatch:
// --with-body re-emits the pre-DKT-1053 row, description and all, for the
// caller that genuinely wants every description in one call.
func TestIssueListJSON_WithBodyRestoresDescriptions(t *testing.T) {
	conn := newTestDB(t)
	descriptions := seedDescribedIssues(t, conn)

	cmd := listCmdWithBody(conn, true)
	setFlags(t, cmd, map[string]string{"type": "feature", "status": "todo", "sort": "id:asc"})
	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList --with-body: %v", err)
	}

	var env struct {
		Data struct {
			Issues []map[string]json.RawMessage `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	assertFullRows(t, env.Data.Issues, descriptions)
	if buf.Len() < 4096 {
		t.Errorf("--with-body output = %d bytes; the descriptions did not come back", buf.Len())
	}

	// The full row is model.Issue's own marshaler, byte for byte: what this
	// verb printed by default before DKT-1053.
	issues, _, err := db.ListIssues(conn, db.ListOptions{
		ProjectID: db.DefaultProjectID, Types: []string{"feature"},
		Statuses: []string{"todo"}, Sort: "id", SortDir: "asc", Limit: 50,
	})
	testsupport.Must(t, err, "ListIssues: %v", err)
	testsupport.Must(t, db.HydrateDocs(conn, issues), "HydrateDocs: %v", err)
	want, err := json.Marshal(issues)
	testsupport.Must(t, err, "Marshal: %v", err)
	var got struct {
		Data struct {
			Issues json.RawMessage `json:"issues"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &got), "unmarshal: %v", nil)
	if !bytes.Equal(bytes.TrimSpace(got.Data.Issues), want) {
		t.Errorf("--with-body rows are not model.Issue's v1 marshaling:\n got %s\nwant %s",
			got.Data.Issues, want)
	}
}

// TestIssueListJSON_SummaryRowKeepsConditionalKeys: the row drops exactly one
// key. `parent_id`, and `scope` when declared, survive the projection — a
// consumer selecting either off a list row (test_zd_jsonv2.sh does, for
// scope) reads what it read before.
func TestIssueListJSON_SummaryRowKeepsConditionalKeys(t *testing.T) {
	conn := newTestDB(t)
	parent := createIssue(t, conn, "parent", model.StatusTodo, model.PriorityHigh)
	child, err := db.CreateIssue(conn, &model.Issue{
		Title:    "scoped child",
		ParentID: &parent,
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindTask,
	}, []string{"engine"}, nil)
	testsupport.Must(t, err, "CreateIssue(child): %v", err)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, child, `["internal/db/**"]`),
		"SetIssueScopeGlobs: %v", nil)

	cmd := listCmdWithBody(conn, false)
	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList: %v", err)
	}

	var env struct {
		Data struct {
			Issues []struct {
				ID       string          `json:"id"`
				ParentID *string         `json:"parent_id"`
				Labels   []string        `json:"labels"`
				Scope    json.RawMessage `json:"scope"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	var found bool
	for _, row := range env.Data.Issues {
		switch row.ID {
		case model.FormatID(child):
			found = true
			if row.ParentID == nil || *row.ParentID != model.FormatID(parent) {
				t.Errorf("child parent_id = %v, want %s", row.ParentID, model.FormatID(parent))
			}
			if string(row.Scope) != `["internal/db/**"]` {
				t.Errorf("child scope = %s, want [\"internal/db/**\"]", row.Scope)
			}
			if len(row.Labels) != 1 || row.Labels[0] != "engine" {
				t.Errorf("child labels = %v, want [engine]", row.Labels)
			}
		case model.FormatID(parent):
			if row.Scope != nil {
				t.Errorf("parent declares no scope but its row carries scope = %s", row.Scope)
			}
			if row.ParentID != nil {
				t.Errorf("root row carries parent_id = %q", *row.ParentID)
			}
		}
	}
	if !found {
		t.Fatalf("child %s not listed:\n%s", model.FormatID(child), buf.String())
	}
}

// TestIssueListJSON_EmptyIsAnArray guards the nil-slice trap on the summary
// path: a listing that matches nothing must emit `[]`, not `null`.
func TestIssueListJSON_EmptyIsAnArray(t *testing.T) {
	conn := newTestDB(t)

	for _, withBody := range []bool{false, true} {
		cmd := listCmdWithBody(conn, withBody)
		w, buf := bufWriter(true)
		if err := runIssueList(cmd, nil, w); err != nil {
			t.Fatalf("runIssueList (with-body=%v): %v", withBody, err)
		}
		var env struct {
			Data struct {
				Issues json.RawMessage `json:"issues"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		if got := string(env.Data.Issues); got != "[]" {
			t.Errorf("issues (with-body=%v) = %s, want []", withBody, got)
		}
	}
}

// TestIssueListJSONV2_SummaryRowsCarryVersion is the v2 half: the collection
// envelope still counts and truncates across the payload change, and each
// summary item carries the CAS `version` a VersionedIssue carried — the one
// thing a v2 list consumer reads that a v1 row does not have.
func TestIssueListJSONV2_SummaryRowsCarryVersion(t *testing.T) {
	for _, withBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("with-body=%v", withBody), func(t *testing.T) {
			conn := newTestDB(t)
			descriptions := seedDescribedIssues(t, conn)

			cmd := listCmdWithBody(conn, withBody)
			setFlags(t, cmd, map[string]string{
				"type": "feature", "status": "todo", "sort": "id:asc", "limit": "5",
			})
			w, buf := bufWriter(true)
			w.JSONVersion = output.JSONV2
			if err := runIssueList(cmd, nil, w); err != nil {
				t.Fatalf("runIssueList --json=v2 --limit 5: %v", err)
			}

			var env struct {
				Data struct {
					Items     []map[string]json.RawMessage `json:"items"`
					Total     int                          `json:"total"`
					Truncated bool                         `json:"truncated"`
				} `json:"data"`
			}
			if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, buf.String())
			}
			if len(env.Data.Items) != 5 || env.Data.Total != 11 || !env.Data.Truncated {
				t.Fatalf("v2 envelope = %d items, total %d, truncated %v; want 5, 11, true",
					len(env.Data.Items), env.Data.Total, env.Data.Truncated)
			}
			if withBody {
				assertFullRows(t, env.Data.Items, descriptions[:5])
			} else {
				assertSummaryRows(t, env.Data.Items, descriptions[:5])
			}
			for i, item := range env.Data.Items {
				var version int
				raw, ok := item["version"]
				if !ok {
					t.Fatalf("v2 item %d has no version key; keys = %v", i, keysOf(item))
				}
				if err := json.Unmarshal(raw, &version); err != nil || version < 1 {
					t.Errorf("v2 item %d version = %s, want a positive CAS version", i, raw)
				}
				if _, ok := item["lease"]; ok {
					t.Errorf("v2 item %d carries a lease key while unclaimed", i)
				}
			}
		})
	}
}

// TestNextJSON_IsSummaryRows: `next` lists the same rows `issue list` does,
// with the same escape hatch.
func TestNextJSON_IsSummaryRows(t *testing.T) {
	conn := newTestDB(t)
	descriptions := seedDescribedIssues(t, conn)

	for _, withBody := range []bool{false, true} {
		cmd := nextCmdWithDB(conn, 50)
		cmd.Flags().Bool("with-body", withBody, "")
		setFlags(t, cmd, map[string]string{"type": "feature"})
		w, buf := bufWriter(true)
		if err := runNext(cmd, nil, w); err != nil {
			t.Fatalf("runNext (with-body=%v): %v", withBody, err)
		}
		var env struct {
			Data struct {
				Issues []map[string]json.RawMessage `json:"issues"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		if withBody {
			assertFullRows(t, env.Data.Issues, descriptions)
			continue
		}
		assertSummaryRows(t, env.Data.Issues, descriptions)
		if buf.Len() >= 4096 {
			t.Errorf("next -T feature --json = %d bytes, want < 4096", buf.Len())
		}
	}
}

// TestPlanJSON_IsSummaryRows: a plan row is the shared summary row plus
// `blocked_by`; --with-body re-emits the pre-DKT-1053 plan row.
func TestPlanJSON_IsSummaryRows(t *testing.T) {
	conn := newTestDB(t)
	descriptions := seedDescribedIssues(t, conn)

	for _, withBody := range []bool{false, true} {
		cmd := planCmdWithDB(conn)
		cmd.Flags().Bool("with-body", withBody, "")
		setFlags(t, cmd, map[string]string{"type": "feature"})
		w, buf := bufWriter(true)
		if err := runPlan(cmd, nil, w); err != nil {
			t.Fatalf("runPlan (with-body=%v): %v", withBody, err)
		}
		var env struct {
			Data struct {
				Phases []struct {
					Issues []map[string]json.RawMessage `json:"issues"`
				} `json:"phases"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		var rows []map[string]json.RawMessage
		for _, phase := range env.Data.Phases {
			rows = append(rows, phase.Issues...)
		}
		for i, row := range rows {
			if got := string(row["blocked_by"]); got != "[]" {
				t.Errorf("row %d blocked_by = %s, want [] (with-body=%v)", i, got, withBody)
			}
		}
		if withBody {
			assertFullRows(t, rows, descriptions)
			continue
		}
		assertSummaryRows(t, rows, descriptions)
		if buf.Len() >= 4096 {
			t.Errorf("plan -T feature --json = %d bytes, want < 4096", buf.Len())
		}
	}
}

// TestBoardJSON_IsSummaryRows: every column's rows are summary rows; empty
// columns stay `[]`; --with-body restores the full shape.
func TestBoardJSON_IsSummaryRows(t *testing.T) {
	conn := newTestDB(t)
	descriptions := seedDescribedIssues(t, conn)

	for _, withBody := range []bool{false, true} {
		cmd := boardCmdWithDB(conn, withBody)
		setFlags(t, cmd, map[string]string{"priority": "high"})
		w, buf := bufWriter(true)
		if err := runBoard(cmd, nil, w); err != nil {
			t.Fatalf("runBoard (with-body=%v): %v", withBody, err)
		}
		var env struct {
			Data struct {
				Columns []struct {
					Status string          `json:"status"`
					Count  int             `json:"count"`
					Issues json.RawMessage `json:"issues"`
				} `json:"columns"`
			} `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		var todo []map[string]json.RawMessage
		for _, col := range env.Data.Columns {
			if col.Status == string(model.StatusTodo) {
				if err := json.Unmarshal(col.Issues, &todo); err != nil {
					t.Fatalf("todo column: %v", err)
				}
				continue
			}
			if got := string(col.Issues); got != "[]" {
				t.Errorf("%s column issues = %s, want [] (with-body=%v)", col.Status, got, withBody)
			}
		}
		if withBody {
			assertFullRows(t, todo, descriptions)
			continue
		}
		assertSummaryRows(t, todo, descriptions)
		if buf.Len() >= 4096 {
			t.Errorf("board -p high --json = %d bytes, want < 4096", buf.Len())
		}
	}
}

// TestIssueShowJSON_StillCarriesTheDescription is the other half of DKT-1053:
// the description moved OUT of the listings on the promise that `issue show`
// still has it. This test is what makes that promise checkable.
func TestIssueShowJSON_StillCarriesTheDescription(t *testing.T) {
	conn := newTestDB(t)
	desc := issueDescription(1)
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:       "Planned issue 1",
		Description: desc,
		Status:      model.StatusTodo,
		Priority:    model.PriorityHigh,
		Kind:        model.IssueKindFeature,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)

	for _, version := range []output.JSONVersion{output.JSONV1, output.JSONV2} {
		w, buf := bufWriter(true)
		w.JSONVersion = version
		if err := runIssueShow(cmdWithDB(conn), []string{model.FormatID(id)}, w); err != nil {
			t.Fatalf("runIssueShow: %v", err)
		}
		var env struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, buf.String())
		}
		var got string
		if err := json.Unmarshal(env.Data["description"], &got); err != nil {
			t.Fatalf("issue show description: %v", err)
		}
		if got != desc {
			t.Errorf("issue show description = %d bytes, want the full %d-byte description",
				len(got), len(desc))
		}
		if _, ok := env.Data["description_bytes"]; ok {
			t.Error("issue show grew a description_bytes key; the detail view is unchanged")
		}
	}
}
