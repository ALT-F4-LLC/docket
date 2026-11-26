package verb_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// createIssue writes one issue into projectID with the fields an export
// renders filled in.
func createExportIssue(t *testing.T, st store.Store, projectID int, title string, status model.Status, assignee string) int {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: projectID,
		Title:     title,
		Status:    status,
		Priority:  model.PriorityMedium,
		Kind:      model.IssueKindFeature,
		Assignee:  assignee,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", title, err)
	}
	return id
}

func exportJSON(t *testing.T, env verb.Env, in verb.ExportInput) model.ExportData {
	t.Helper()
	in.Format = "json"
	result, err := verb.Export(context.Background(), env, in)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	var data model.ExportData
	if err := json.Unmarshal([]byte(result.Document), &data); err != nil {
		t.Fatalf("unmarshalling the export document: %v", err)
	}
	return data
}

func issueTitles(issues []*model.Issue) []string {
	titles := make([]string, 0, len(issues))
	for _, issue := range issues {
		titles = append(titles, issue.Title)
	}
	return titles
}

func TestExportRefusesAnUnknownFormat(t *testing.T) {
	_, err := verb.Export(context.Background(), projectEnv(openStore(t), "", false),
		verb.ExportInput{Format: "xml"})
	requireVerbError(t, err, output.ErrValidation, `invalid format "xml"`)
}

func TestExportRefusesAnUnknownStatusFilter(t *testing.T) {
	_, err := verb.Export(context.Background(), projectEnv(openStore(t), "", false),
		verb.ExportInput{Format: "json", Statuses: []string{"shipped"}})
	requireVerbError(t, err, output.ErrValidation, `invalid status "shipped"`)
}

// Every collection on the export document is read, and an empty one is written
// as [] rather than null, so an import of the document reads it back.
func TestExportJSONWritesEveryCollectionAsAnArray(t *testing.T) {
	result, err := verb.Export(context.Background(), projectEnv(openStore(t), "", false),
		verb.ExportInput{Format: "json"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Format != "json" {
		t.Errorf("result format = %q, want json", result.Format)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.Document), &document); err != nil {
		t.Fatalf("unmarshalling the export document: %v", err)
	}
	shape := reflect.TypeOf(model.ExportData{})
	for i := 0; i < shape.NumField(); i++ {
		field := shape.Field(i)
		if field.Type.Kind() != reflect.Slice {
			continue
		}
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		if got := string(document[key]); got != "[]" {
			t.Errorf("collection %s = %s, want []", key, got)
		}
	}
}

func TestExportReadsOnlyTheCallersProject(t *testing.T) {
	st := openStore(t)
	register := func(identity string) int {
		t.Helper()
		id, _, err := st.EnsureProjectCreated(identity, filepath.Base(identity), time.Now().UnixMilli())
		if err != nil {
			t.Fatalf("EnsureProjectCreated(%q): %v", identity, err)
		}
		return id
	}
	// The first identity registered claims the default project's row.
	createExportIssue(t, st, register("/repo/beta"), "in beta", model.StatusTodo, "")
	createExportIssue(t, st, register("/repo/alpha"), "in alpha", model.StatusTodo, "")

	data := exportJSON(t, projectEnv(st, "/repo/alpha", true), verb.ExportInput{})
	if got := issueTitles(data.Issues); !reflect.DeepEqual(got, []string{"in alpha"}) {
		t.Errorf("exported issues = %v, want [in alpha]", got)
	}
}

// Export is a read: an identity no project holds exports nothing and is not
// registered, even when it is anchored.
func TestExportOfAnUnregisteredProjectIsEmptyAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	createExportIssue(t, st, store.DefaultProjectID, "in the default project", model.StatusTodo, "")

	data := exportJSON(t, projectEnv(st, "/repo/unknown", true), verb.ExportInput{})
	if len(data.Issues) != 0 {
		t.Errorf("exported issues = %v, want none", issueTitles(data.Issues))
	}
	requireUnregistered(t, st, "/repo/unknown")
}

func TestExportFiltersByStatus(t *testing.T) {
	st := openStore(t)
	createExportIssue(t, st, store.DefaultProjectID, "open work", model.StatusTodo, "")
	createExportIssue(t, st, store.DefaultProjectID, "finished work", model.StatusDone, "")

	data := exportJSON(t, projectEnv(st, "", false), verb.ExportInput{Statuses: []string{"done"}})
	if got := issueTitles(data.Issues); !reflect.DeepEqual(got, []string{"finished work"}) {
		t.Errorf("exported issues = %v, want [finished work]", got)
	}
}

// A cell a spreadsheet would evaluate as a formula is prefixed with a quote;
// anything else, including a trigger character inside the text, is left alone.
func TestExportCSVNeutralizesFormulaInjection(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"equals", "=HYPERLINK(\"evil\")", "'=HYPERLINK(\"evil\")"},
		{"plus", "+1234", "'+1234"},
		{"minus", "-cmd", "'-cmd"},
		{"at", "@SUM(A1)", "'@SUM(A1)"},
		{"tab", "\t=cmd", "'\t=cmd"},
		{"carriage return", "\r=cmd", "'\r=cmd"},
		{"benign", "normal title", "normal title"},
		{"interior trigger", "a=b", "a=b"},
	}

	st := openStore(t)
	wantByID := make(map[string]string, len(cases))
	for _, tc := range cases {
		id := createExportIssue(t, st, store.DefaultProjectID, tc.title, model.StatusTodo, "")
		wantByID[model.FormatID(id)] = tc.want
	}
	assigned := createExportIssue(t, st, store.DefaultProjectID, "assigned", model.StatusTodo, "@alice")

	result, err := verb.Export(context.Background(), projectEnv(st, "", false),
		verb.ExportInput{Format: "csv"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	records, err := csv.NewReader(strings.NewReader(result.Document)).ReadAll()
	if err != nil {
		t.Fatalf("parsing the CSV: %v", err)
	}
	if len(records) != len(cases)+2 {
		t.Fatalf("got %d records, want %d (header + one row per issue)", len(records), len(cases)+2)
	}

	for _, row := range records[1:] {
		id, title, assignee := row[0], row[2], row[7]
		if id == model.FormatID(assigned) {
			if assignee != "'@alice" {
				t.Errorf("assignee cell = %q, want %q", assignee, "'@alice")
			}
			continue
		}
		if want := wantByID[id]; title != want {
			t.Errorf("title cell of %s = %q, want %q", id, title, want)
		}
		if assignee != "" {
			t.Errorf("empty assignee of %s rendered as %q", id, assignee)
		}
	}
}
