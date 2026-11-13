package verb_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// exportDocument returns the JSON export of the default project of a fresh
// store holding one issue per title.
func exportDocument(t *testing.T, titles ...string) string {
	t.Helper()
	src := openStore(t)
	for _, title := range titles {
		createExportIssue(t, src, store.DefaultProjectID, title, model.StatusTodo, "")
	}
	result, err := verb.Export(context.Background(), projectEnv(src, "", false),
		verb.ExportInput{Format: "json"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return result.Document
}

func projectIssueTitles(t *testing.T, st store.Store, projectID int) []string {
	t.Helper()
	issues, err := st.ListAllIssues(projectID)
	if err != nil {
		t.Fatalf("ListAllIssues(%d): %v", projectID, err)
	}
	return issueTitles(issues)
}

func TestImportRefusesAnInvalidRequestBeforeTheStore(t *testing.T) {
	document := exportDocument(t, "imported")
	cases := []struct {
		name       string
		in         verb.ImportInput
		wantPrefix string
	}{
		{"merge with replace", verb.ImportInput{Document: document, Merge: true, Replace: true},
			"--merge and --replace are mutually exclusive"},
		{"malformed JSON", verb.ImportInput{Document: "{"}, "parsing JSON"},
		{"unsupported version", verb.ImportInput{Document: `{"version": 2}`},
			"validation failed with 1 error(s):\n  - unsupported version 2: expected 1"},
		{"replace without yes", verb.ImportInput{Document: document, Replace: true}, "--replace deletes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := openStore(t)
			createExportIssue(t, st, store.DefaultProjectID, "must survive", model.StatusTodo, "")

			_, err := verb.Import(context.Background(), projectEnv(st, "/repos/fresh", true), c.in)
			requireVerbError(t, err, output.ErrValidation, c.wantPrefix)
			requireUnregistered(t, st, "/repos/fresh")
			if got := projectIssueTitles(t, st, store.DefaultProjectID); !reflect.DeepEqual(got, []string{"must survive"}) {
				t.Errorf("issues = %v, want the refused import to leave [must survive]", got)
			}
		})
	}
}

func TestImportIntoANonEmptyProjectNeedsMergeOrReplace(t *testing.T) {
	st := openStore(t)
	createExportIssue(t, st, store.DefaultProjectID, "existing", model.StatusTodo, "")
	env := projectEnv(st, "", false)
	document := exportDocument(t, "imported")

	_, err := verb.Import(context.Background(), env, verb.ImportInput{Document: document})
	requireVerbError(t, err, output.ErrConflict, "database is not empty")

	res, err := verb.Import(context.Background(), env, verb.ImportInput{Document: document, Replace: true, Yes: true})
	if err != nil {
		t.Fatalf("Import --replace --yes: %v", err)
	}
	if res.Imported == 0 {
		t.Errorf("Imported = 0, want the replaced project's rows counted")
	}
	if got := projectIssueTitles(t, st, store.DefaultProjectID); !reflect.DeepEqual(got, []string{"imported"}) {
		t.Errorf("issues = %v, want [imported] after the replace", got)
	}
}

// Merge imports into a non-empty project and skips the rows the project
// already holds under the same ids, rather than refusing or overwriting them.
func TestImportMergeIntoANonEmptyProjectKeepsItsOwnRows(t *testing.T) {
	st := openStore(t)
	createExportIssue(t, st, store.DefaultProjectID, "existing", model.StatusTodo, "")
	env := projectEnv(st, "", false)

	res, err := verb.Import(context.Background(), env,
		verb.ImportInput{Document: exportDocument(t, "imported"), Merge: true})
	if err != nil {
		t.Fatalf("Import --merge: %v", err)
	}
	if res.Skipped == 0 {
		t.Errorf("Skipped = 0, want the issue the project already holds skipped")
	}
	if got := projectIssueTitles(t, st, store.DefaultProjectID); !reflect.DeepEqual(got, []string{"existing"}) {
		t.Errorf("issues = %v, want the merge to keep [existing]", got)
	}
}

// A project Import registers stays registered when the restore then fails, so
// the caller still learns of it and records its event.
func TestImportReportsTheRegistrationWhenTheRestoreFails(t *testing.T) {
	var data model.ExportData
	if err := json.Unmarshal([]byte(exportDocument(t, "imported")), &data); err != nil {
		t.Fatalf("decoding the export: %v", err)
	}
	data.DocIssueLinks = append(data.DocIssueLinks, model.DocIssueLink{
		DocID: 999998, IssueID: 999999, CreatedAt: "2026-01-01T00:00:00Z",
	})
	document, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encoding the export: %v", err)
	}

	st := openStore(t)
	res, err := verb.Import(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.ImportInput{Document: string(document), Replace: true, Yes: true})
	requireVerbError(t, err, output.ErrGeneral, "importing data")

	projectID, found, lookupErr := st.LookupProject("/repos/fresh")
	if lookupErr != nil || !found {
		t.Fatalf("LookupProject: found=%v err=%v, want the registered row", found, lookupErr)
	}
	if reg := res.Registered; reg == nil || reg.ID != projectID || reg.Verb != "import" {
		t.Errorf("Registered = %+v, want project %d registered by import alongside the error", reg, projectID)
	}
}

// Import writes into the caller's project, so an anchored identity is
// registered on first contact and the registration is reported for its event.
func TestImportRegistersTheCallersProject(t *testing.T) {
	st := openStore(t)
	if _, err := st.EnsureProject("/repos/other", "other", 1); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	res, err := verb.Import(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.ImportInput{Document: exportDocument(t, "imported")})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	projectID, found, err := st.LookupProject("/repos/fresh")
	if err != nil || !found {
		t.Fatalf("LookupProject: found=%v err=%v, want the registered row", found, err)
	}
	if reg := res.Registered; reg == nil || reg.ID != projectID || reg.Verb != "import" {
		t.Errorf("Registered = %+v, want project %d registered by import", reg, projectID)
	}
	if got := projectIssueTitles(t, st, projectID); !reflect.DeepEqual(got, []string{"imported"}) {
		t.Errorf("issues in the caller's project = %v, want [imported]", got)
	}
}

func TestImportFromAnUnanchoredIdentityIsRefused(t *testing.T) {
	st := openStore(t)
	_, err := verb.Import(context.Background(), projectEnv(st, "/tmp/scratch", false),
		verb.ImportInput{Document: exportDocument(t, "imported")})
	requireVerbError(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/tmp/scratch")
}

// A source id another project already holds is reassigned rather than
// skipped, so consolidating two stores drops nothing.
func TestImportRemapsIDsAnotherProjectHolds(t *testing.T) {
	st := openStore(t)
	// The first identity registered claims the default project's row, so the
	// importing identity gets a project of its own.
	first, _, err := st.EnsureProjectCreated("/repos/first", "first", 1)
	if err != nil {
		t.Fatalf("EnsureProjectCreated: %v", err)
	}
	createExportIssue(t, st, first, "already here", model.StatusTodo, "")
	document := exportDocument(t, "from elsewhere")
	if !strings.Contains(document, `"id": 1`) {
		t.Fatalf("test premise broken: the document does not reuse id 1:\n%s", document)
	}

	res, err := verb.Import(context.Background(), projectEnv(st, "/repos/second", true),
		verb.ImportInput{Document: document})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Remapped == 0 || res.Skipped != 0 {
		t.Errorf("Remapped = %d, Skipped = %d, want a remap and nothing skipped", res.Remapped, res.Skipped)
	}
	second, _, err := st.LookupProject("/repos/second")
	if err != nil || second == first {
		t.Fatalf("LookupProject = %d, %v, want a project other than %d", second, err, first)
	}
	if got := projectIssueTitles(t, st, second); !reflect.DeepEqual(got, []string{"from elsewhere"}) {
		t.Errorf("issues in the importing project = %v, want [from elsewhere]", got)
	}
}
