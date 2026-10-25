package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"runtime"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func nextCmdWithDB(conn *sql.DB, limit int) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().StringSlice("status", nil, "")
	cmd.Flags().StringSlice("priority", nil, "")
	cmd.Flags().StringSlice("label", nil, "")
	cmd.Flags().StringSlice("type", nil, "")
	cmd.Flags().Int("limit", limit, "")
	// --run is the phase-3 mode switch. It is registered here so
	// runNext's dispatch resolves; every issue-mode test leaves it UNSET, which
	// is exactly the state a workflow-free repo is always in.
	cmd.Flags().String("run", "", "")
	return cmd
}

type nextJSON struct {
	Data struct {
		Items []struct {
			ID    string   `json:"id"`
			Files []string `json:"files"`
			Docs  []struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"docs"`
		} `json:"items"`
		Total int `json:"total"`
	} `json:"data"`
}

func runNextJSON(t *testing.T, conn *sql.DB, limit int) nextJSON {
	t.Helper()
	cmd := nextCmdWithDB(conn, limit)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)
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
	if len(nj.Data.Items) != 1 {
		t.Fatalf("issues = %d, want 1", len(nj.Data.Items))
	}
	iss := nj.Data.Items[0]
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
	w.JSONVersion = output.JSONV2
	err := runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)

	var env struct {
		Data struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if len(env.Data.Items) != 1 {
		t.Fatalf("issues = %d, want 1", len(env.Data.Items))
	}
	for _, key := range []string{"files", "docs"} {
		if got := string(env.Data.Items[0][key]); got != "[]" {
			t.Errorf("%s = %s, want []", key, got)
		}
	}
}

// TestNextJSON_NoReadyIssuesIsAnEmptyArray pins the same "[] never null"
// contract as TestNextJSON_FilesAndDocsEmptyAreArrays, one level up: FindReady
// and filterReady return a nil slice when nothing is ready (planner/plan.go's
// `var ready []*model.Issue`), and nextResult has no custom MarshalJSON like
// issueListPayload does — an unguarded nil would serialize `.data.items` as
// JSON null, breaking any consumer iterating it.
func TestNextJSON_NoReadyIssuesIsAnEmptyArray(t *testing.T) {
	conn := newTestDB(t)
	createIssue(t, conn, "already done", model.StatusDone, model.PriorityHigh)

	cmd := nextCmdWithDB(conn, 10)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)

	var env struct {
		Data struct {
			Items json.RawMessage `json:"items"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if got := string(env.Data.Items); got != "[]" {
		t.Errorf("issues = %s, want []", got)
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

	// THE CLOCK IS PINNED (E-7). The table renders `updated_at` as a RELATIVE
	// time (`humanize.Time`, render/table.go), and the two databases are built
	// a few milliseconds apart — so a run that straddles a second boundary
	// rendered "now" for one and "1 second ago" for the other, failing on a
	// difference this test is not about.
	//
	// Both sides are stamped to the same instant, which makes the comparison
	// measure what it claims to: whether linked docs and files change the
	// table.
	pinTimestamps(t, withContext)
	pinTimestamps(t, without)

	gotWith := runNextHuman(t, withContext)
	gotWithout := runNextHuman(t, without)

	if gotWith != gotWithout {
		t.Errorf("next human table changed by linked docs/files.\n--- with context ---\n%q\n--- without ---\n%q", gotWith, gotWithout)
	}
	if strings.Contains(gotWith, "Some Doc") || strings.Contains(gotWith, "doc_links.go") {
		t.Errorf("next human table leaked doc/file data:\n%s", gotWith)
	}
}

// pinTimestamps stamps every issue in a test database to one fixed instant, so
// a renderer that formats times RELATIVE to now produces identical output no
// matter when the test runs or how long it took to get here.
//
// The instant is a literal rather than a time.Now() offset: two databases
// pinned to "an hour ago" computed a few milliseconds apart are still two
// different instants, which is the flake this exists to remove.
func pinTimestamps(t *testing.T, conn *sql.DB) {
	t.Helper()
	const pinned = "2026-01-01T00:00:00Z"
	if _, err := conn.Exec(
		`UPDATE issues SET created_at = ?, updated_at = ?`, pinned, pinned,
	); err != nil {
		t.Fatalf("pinning timestamps: %v", err)
	}
}

func runNextHuman(t *testing.T, conn *sql.DB) string {
	t.Helper()
	cmd := nextCmdWithDB(conn, 10)
	w, buf := bufWriter(false)
	err := runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)
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
	if len(nj.Data.Items) != 1 {
		t.Fatalf("issues = %d, want 1 (limit)", len(nj.Data.Items))
	}
	emitted := nj.Data.Items[0]
	if emitted.ID != model.FormatID(first) {
		t.Fatalf("emitted = %s, want %s (priority order)", emitted.ID, model.FormatID(first))
	}
	if len(emitted.Docs) != 1 || emitted.Docs[0].Title != "First Doc" {
		t.Errorf("emitted issue not hydrated with its doc: %+v", emitted.Docs)
	}

	cmd := nextCmdWithDB(conn, 1)
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err := runNext(cmd, nil, w)
	testsupport.Must(t, err, "runNext: %v", err)
	if strings.Contains(buf.String(), "Second Doc") {
		t.Errorf("beyond-limit issue's doc leaked into output:\n%s", buf.String())
	}
}

// classCapWorkflow offers one root step per class: three bounded by
// `[limits]` and one, free, that nothing bounds.
const classCapWorkflow = `
[pipeline]
name = "class-caps"
version = 1
[match]
kind = ["task"]
[limits]
write = { max = 2 }
judge = { max = 3 }
read = { max = 5 }
[[step]]
name = "w"
after = []
executor = "x"
class = "write"
emits = "out"
[[step]]
name = "j"
after = []
executor = "x"
class = "judge"
emits = "out"
[[step]]
name = "r"
after = []
executor = "x"
class = "read"
emits = "out"
[[step]]
name = "f"
after = []
executor = "x"
class = "free"
emits = "out"
`

// TestNextStepModeReportsClassCapsAndSources: `next --run` under v2 reports,
// beside items/total/truncated, the cap the scheduler enforces for each
// bounded class it offers and what set it: a numeric override, auto at the
// integer it resolves to, and the workflow max. An unbounded class is absent.
func TestNextStepModeReportsClassCapsAndSources(t *testing.T) {
	conn := newTestDB(t)
	registerForRun(t, conn, classCapWorkflow)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "caps", Description: "body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	err = db.AddRunIssue(conn, run.ID, issueID)
	testsupport.Must(t, err, "adding issue: %v", err)
	activateHolding(t, conn, run.ID, model.NowMS())
	err = db.SetConfig(conn, 0, db.KeyClassMaxPrefix+"write", "4")
	testsupport.Must(t, err, "setting the numeric override: %v", err)
	err = db.SetConfig(conn, 0, db.KeyClassMaxPrefix+"read", db.ClassMaxAuto)
	testsupport.Must(t, err, "setting auto: %v", err)

	data := stepModeEnvelope(t, nextCmdWithDB(conn, 10), run.ID)

	offered := map[string]bool{}
	for _, row := range data.Items {
		offered[fmt.Sprint(row["class"])] = true
	}
	for _, class := range []string{"write", "judge", "read", "free"} {
		if !offered[class] {
			t.Fatalf("premise: the offer carries no %s row: %v", class, data.Items)
		}
	}

	wantLimits := map[string]int{
		"write": 4,
		"read":  max(1, runtime.NumCPU()/2),
		"judge": 3,
	}
	wantSources := map[string]string{
		"write": "override",
		"read":  "auto",
		"judge": "workflow",
	}
	if !maps.Equal(data.Limits, wantLimits) {
		t.Errorf("limits = %v, want %v", data.Limits, wantLimits)
	}
	if !maps.Equal(data.LimitSources, wantSources) {
		t.Errorf("limit_sources = %v, want %v", data.LimitSources, wantSources)
	}
}
