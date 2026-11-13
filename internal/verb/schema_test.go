package verb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// callerProject registers a project that is not the default one, so a verb
// that ignored its Env and fell back to store.DefaultProjectID would miss rows
// written there. EnsureProject claims the unclaimed default row first, so the
// second registration is the one that gets a fresh id.
func callerProject(t *testing.T, st store.Store) int {
	t.Helper()
	if _, err := st.EnsureProject("/repos/default", "default", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	id, err := st.EnsureProject("/repos/caller", "caller", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if id == store.DefaultProjectID {
		t.Fatalf("caller project = %d, want a non-default id", id)
	}
	return id
}

func insertSchema(t *testing.T, st store.Store, projectID int, name string, version int) {
	t.Helper()
	_, _, err := st.InsertSchema(&model.Schema{
		ProjectID:    projectID,
		Name:         name,
		Version:      version,
		SourceSHA256: "sha-" + name,
		Body:         `{"type":"object"}`,
		Ordered:      "{}",
	}, 1)
	if err != nil {
		t.Fatalf("InsertSchema %s@%d: %v", name, version, err)
	}
}

func schemaNames(schemas []*model.Schema) []string {
	names := make([]string, 0, len(schemas))
	for _, s := range schemas {
		names = append(names, s.Ref())
	}
	return names
}

func TestSchemaListReadsTheCallersProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)

	got, err := verb.SchemaList(context.Background(),
		projectEnv(st, "/repos/caller", true), verb.SchemaListInput{Name: "findings"})
	if err != nil {
		t.Fatalf("SchemaList: %v", err)
	}
	if got.Total != 1 || len(got.Schemas) != 1 || got.Schemas[0].Ref() != "findings@1" {
		t.Errorf("listed %v (total %d), want [findings@1]", schemaNames(got.Schemas), got.Total)
	}
}

// A list is a read: an anchored identity with no project row sees nothing of
// another project's and is not registered by looking.
func TestSchemaListNeverRegistersTheCaller(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)

	got, err := verb.SchemaList(context.Background(),
		projectEnv(st, "/repos/stranger", true), verb.SchemaListInput{Name: "findings"})
	if err != nil {
		t.Fatalf("SchemaList: %v", err)
	}
	if got.Total != 0 || len(got.Schemas) != 0 {
		t.Errorf("an unregistered caller listed %v (total %d), want none",
			schemaNames(got.Schemas), got.Total)
	}
	requireUnregistered(t, st, "/repos/stranger")
}

func TestSchemaListHidesRetiredVersionsUnlessAsked(t *testing.T) {
	st := openStore(t)
	insertSchema(t, st, store.DefaultProjectID, "findings", 1)
	insertSchema(t, st, store.DefaultProjectID, "findings", 2)
	if _, err := st.DeprecateSchema(store.DefaultProjectID, "findings", 1, 2); err != nil {
		t.Fatalf("DeprecateSchema: %v", err)
	}
	env := projectEnv(st, "", false)

	inService, err := verb.SchemaList(context.Background(), env,
		verb.SchemaListInput{Name: "findings"})
	if err != nil {
		t.Fatalf("SchemaList: %v", err)
	}
	if names := schemaNames(inService.Schemas); len(names) != 1 || names[0] != "findings@2" {
		t.Errorf("in-service listing = %v, want [findings@2]", names)
	}

	all, err := verb.SchemaList(context.Background(), env,
		verb.SchemaListInput{Name: "findings", IncludeDeprecated: true})
	if err != nil {
		t.Fatalf("SchemaList --deprecated: %v", err)
	}
	if all.Total != 2 {
		t.Errorf("listing with retired versions = %v (total %d), want both",
			schemaNames(all.Schemas), all.Total)
	}
}

func TestSchemaShowReadsTheCallersProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)

	got, err := verb.SchemaShow(context.Background(),
		projectEnv(st, "/repos/caller", true), verb.SchemaShowInput{Ref: "findings"})
	if err != nil {
		t.Fatalf("SchemaShow: %v", err)
	}
	if got.Schema == nil || got.Schema.Ref() != "findings@1" {
		t.Errorf("showed %v, want findings@1", got.Schema)
	}
}

func TestSchemaShowRefusesAMalformedRef(t *testing.T) {
	st := openStore(t)
	_, err := verb.SchemaShow(context.Background(),
		projectEnv(st, "", false), verb.SchemaShowInput{Ref: "findings@x"})
	var ve *verb.Error
	if !errors.As(err, &ve) || ve.Code != output.ErrValidation {
		t.Fatalf("error = %v, want a *verb.Error coded %s", err, output.ErrValidation)
	}
}

// A schema another project registered is NOT_FOUND to an unregistered
// caller, the message names the ref asked for, and looking registers nothing.
func TestSchemaShowReportsAMissingSchemaAsNotFound(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)

	_, err := verb.SchemaShow(context.Background(),
		projectEnv(st, "/repos/stranger", true), verb.SchemaShowInput{Ref: "findings@1"})
	var ve *verb.Error
	if !errors.As(err, &ve) || ve.Code != output.ErrNotFound {
		t.Fatalf("error = %v, want a *verb.Error coded %s", err, output.ErrNotFound)
	}
	if !errors.Is(err, store.ErrSchemaNotFound) {
		t.Errorf("error = %v, want it to wrap store.ErrSchemaNotFound", err)
	}
	if !strings.Contains(err.Error(), "findings@1 is not registered") {
		t.Errorf("error = %q, want it to name findings@1", err)
	}
	requireUnregistered(t, st, "/repos/stranger")
}

const orderedDocument = `{
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {"type": "string", "enum": ["low", "high"], "ordered_enum": true}
    }
  }
}`

func documentOf(body string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(body), nil }
}

func TestSchemaRegisterWritesTheCallersProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	env := projectEnv(st, "/repos/caller", true)

	got, err := verb.SchemaRegister(context.Background(), env, verb.SchemaRegisterInput{
		Ref: "findings@1", Document: documentOf(orderedDocument), SourcePath: "f.json"})
	if err != nil {
		t.Fatalf("SchemaRegister: %v", err)
	}
	if !got.Created || got.Schema == nil || got.Schema.Ref() != "findings@1" {
		t.Fatalf("result = %+v, want findings@1 created", got)
	}
	if fields := got.Schema.OrderedFields(); len(fields) != 1 || fields[0] != "severity" {
		t.Errorf("ordered fields = %v, want [severity]", fields)
	}
	if _, err := st.GetSchema(caller, "findings", 1); err != nil {
		t.Errorf("GetSchema in the caller's project: %v", err)
	}
	if _, err := st.GetSchema(store.DefaultProjectID, "findings", 1); !errors.Is(err, store.ErrSchemaNotFound) {
		t.Errorf("GetSchema in the default project = %v, want ErrSchemaNotFound", err)
	}

	again, err := verb.SchemaRegister(context.Background(), env, verb.SchemaRegisterInput{
		Ref: "findings@1", Document: documentOf(orderedDocument)})
	if err != nil || again.Created {
		t.Errorf("identical bytes again = (%+v, %v), want an unchanged success", again, err)
	}
}

// A register writes into the caller's project, so an anchored caller with no
// row is registered and the registration is reported for its event.
func TestSchemaRegisterRegistersAnAnchoredCaller(t *testing.T) {
	st := openStore(t)
	got, err := verb.SchemaRegister(context.Background(), projectEnv(st, "/repos/fresh", true),
		verb.SchemaRegisterInput{Ref: "findings@1", Document: documentOf(orderedDocument)})
	if err != nil {
		t.Fatalf("SchemaRegister: %v", err)
	}
	projectID, found, err := st.LookupProject("/repos/fresh")
	if err != nil || !found {
		t.Fatalf("LookupProject = (%d, %v, %v), want the registered project", projectID, found, err)
	}
	if reg := got.Registered; reg == nil || reg.ID != projectID || reg.Verb != "schema register" {
		t.Errorf("Registered = %+v, want project %d registered by schema register", reg, projectID)
	}
	if _, err := st.GetSchema(projectID, "findings", 1); err != nil {
		t.Errorf("GetSchema in the registered project: %v", err)
	}
}

// An unanchored caller is refused before the document is read or any row is
// written, as the root hook refused it before the verb moved.
func TestSchemaRegisterRefusesAnUnanchoredCallerFirst(t *testing.T) {
	st := openStore(t)
	read := false
	_, err := verb.SchemaRegister(context.Background(), projectEnv(st, "/tmp/scratch", false),
		verb.SchemaRegisterInput{Ref: "findings", Document: func() ([]byte, error) {
			read = true
			return []byte(orderedDocument), nil
		}})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	if read {
		t.Error("the document was read before the project was resolved")
	}
	requireUnregistered(t, st, "/tmp/scratch")
}

func TestSchemaRegisterRefusalCodes(t *testing.T) {
	cases := []struct {
		name     string
		in       verb.SchemaRegisterInput
		want     output.ErrorCode
		contains string
	}{
		{"a malformed reference",
			verb.SchemaRegisterInput{Ref: "findings", Document: documentOf(orderedDocument)},
			output.ErrValidation, "name@version"},
		{"a document that is not JSON",
			verb.SchemaRegisterInput{Ref: "broken@1", Document: documentOf(`{"type": `), SourcePath: "b.json"},
			output.ErrValidation, "b.json: "},
		{"different bytes at a registered version",
			verb.SchemaRegisterInput{Ref: "findings@1", Document: documentOf(`{"type": "array"}`)},
			output.ErrConflict, "findings@1"},
		{"both target flags",
			verb.SchemaRegisterInput{Ref: "findings@1", Document: documentOf(orderedDocument),
				Project: "caller", AllProjects: true},
			output.ErrValidation, "--project and --all-projects"},
		{"an unknown project",
			verb.SchemaRegisterInput{Ref: "findings@1", Document: documentOf(orderedDocument),
				Project: "no-such-repo"},
			output.ErrNotFound, "no-such-repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			insertSchema(t, st, store.DefaultProjectID, "findings", 1)
			_, err := verb.SchemaRegister(context.Background(), projectEnv(st, "", false), tc.in)
			requireVerbCode(t, err, tc.want, tc.contains)
		})
	}
}

// --all-projects judges each project on its own: one project's conflict is
// that project's verdict, and every other project is still registered.
func TestSchemaRegisterAcrossProjectsReportsEachProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)

	got, err := verb.SchemaRegister(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.SchemaRegisterInput{Ref: "findings@1", Document: documentOf(orderedDocument), AllProjects: true})
	if err != nil {
		t.Fatalf("SchemaRegister --all-projects: %v", err)
	}
	if got.Schema != nil || len(got.Projects) != 2 {
		t.Fatalf("result = %+v, want a verdict for each of 2 projects and no single row", got)
	}
	for _, v := range got.Projects {
		if v.Project.ID == caller {
			if !errors.Is(v.Err, store.ErrSchemaConflict) {
				t.Errorf("caller's verdict = %v, want a schema conflict", v.Err)
			}
			continue
		}
		if v.Err != nil || !v.Changed || v.Schema.Ref() != "findings@1" {
			t.Errorf("project %d verdict = %+v, want findings@1 registered", v.Project.ID, v)
		}
	}
}

func TestSchemaDeprecateRetiresInTheCallersProjectAndRestores(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertSchema(t, st, caller, "findings", 1)
	insertSchema(t, st, store.DefaultProjectID, "findings", 1)
	env := projectEnv(st, "/repos/caller", true)

	got, err := verb.SchemaDeprecate(context.Background(), env, verb.SchemaDeprecateInput{Ref: "findings@1"})
	if err != nil {
		t.Fatalf("SchemaDeprecate: %v", err)
	}
	if got.Schema == nil || !got.Schema.Deprecated() {
		t.Errorf("result = %+v, want the retired row", got)
	}
	if s, err := st.GetSchema(store.DefaultProjectID, "findings", 1); err != nil || s.Deprecated() {
		t.Errorf("default project's row = (%v, %v), want it still in service", s, err)
	}

	_, err = verb.SchemaDeprecate(context.Background(), env, verb.SchemaDeprecateInput{Ref: "findings@1"})
	requireVerbCode(t, err, output.ErrConflict, "findings@1 is already deprecated")
	if !errors.Is(err, store.ErrSchemaAlreadyDeprecated) {
		t.Errorf("second deprecate = %v, want it to wrap store.ErrSchemaAlreadyDeprecated", err)
	}

	restored, err := verb.SchemaDeprecate(context.Background(), env,
		verb.SchemaDeprecateInput{Ref: "findings@1", Restore: true})
	if err != nil || restored.Schema == nil || restored.Schema.Deprecated() {
		t.Errorf("restore = (%+v, %v), want the row back in service", restored, err)
	}
}

// Restoring across projects is idempotent per project: a retired row is
// restored, one already in service is an unchanged success, and a project
// without the version reports its own NOT_FOUND.
func TestSchemaRestoreAcrossProjectsReportsEachProject(t *testing.T) {
	st := openStore(t)
	retired := callerProject(t, st)
	inService := store.DefaultProjectID
	missing := ensureProject(t, st, "/repos/missing", "missing")
	insertSchema(t, st, retired, "findings", 1)
	insertSchema(t, st, inService, "findings", 1)
	if _, err := st.DeprecateSchema(retired, "findings", 1, 2); err != nil {
		t.Fatalf("DeprecateSchema: %v", err)
	}

	got, err := verb.SchemaDeprecate(context.Background(), projectEnv(st, "", false),
		verb.SchemaDeprecateInput{Ref: "findings@1", Restore: true, AllProjects: true})
	if err != nil {
		t.Fatalf("SchemaDeprecate --restore --all-projects: %v", err)
	}
	if got.Schema != nil || len(got.Projects) != 3 {
		t.Fatalf("result = %+v, want a verdict for each of 3 projects and no single row", got)
	}
	for _, v := range got.Projects {
		switch v.Project.ID {
		case retired:
			if v.Err != nil || !v.Changed || v.Schema.Deprecated() {
				t.Errorf("retired project's verdict = %+v, want it restored", v)
			}
		case inService:
			if v.Err != nil || v.Changed {
				t.Errorf("in-service project's verdict = %+v, want an unchanged success", v)
			}
		case missing:
			requireVerbCode(t, v.Err, output.ErrNotFound, "findings@1 is not registered")
		}
	}
}

func TestSchemaDeprecateRefusalCodes(t *testing.T) {
	cases := []struct {
		name     string
		in       verb.SchemaDeprecateInput
		want     output.ErrorCode
		contains string
	}{
		{"a bare name", verb.SchemaDeprecateInput{Ref: "findings"},
			output.ErrValidation, "name@version"},
		{"an unregistered version", verb.SchemaDeprecateInput{Ref: "nope@1"},
			output.ErrNotFound, "nope@1 is not registered"},
		{"an unknown project", verb.SchemaDeprecateInput{Ref: "findings@1", Project: "no-such-repo"},
			output.ErrNotFound, "no-such-repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			_, err := verb.SchemaDeprecate(context.Background(), projectEnv(st, "", false), tc.in)
			requireVerbCode(t, err, tc.want, tc.contains)
		})
	}
}

func TestSchemaDeprecateRefusesAnUnanchoredCaller(t *testing.T) {
	st := openStore(t)
	_, err := verb.SchemaDeprecate(context.Background(), projectEnv(st, "/tmp/scratch", false),
		verb.SchemaDeprecateInput{Ref: "findings@1"})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/tmp/scratch")
}
