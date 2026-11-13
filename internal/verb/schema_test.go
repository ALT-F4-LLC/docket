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
