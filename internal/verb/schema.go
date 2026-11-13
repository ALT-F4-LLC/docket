package verb

import (
	"context"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// SchemaListInput selects the registered schemas to list.
type SchemaListInput struct {
	// Name filters to one schema's versions; empty lists every name.
	Name string
	// Limit caps the rows returned; zero means no limit.
	Limit int
	// IncludeDeprecated adds retired versions, which are hidden by default.
	IncludeDeprecated bool
}

// SchemaListResult is one page of schemas. Total counts every match,
// ignoring Limit, so a caller can tell the page was truncated.
type SchemaListResult struct {
	Schemas []*model.Schema
	Total   int
}

// SchemaList lists the schemas registered in the caller's project, builtins
// included. It is a read, so it never registers the caller's project.
func SchemaList(ctx context.Context, env Env, in SchemaListInput) (SchemaListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return SchemaListResult{}, err
	}

	schemas, total, err := env.Store.ListSchemas(store.SchemaListOptions{
		ProjectID:         projectID,
		Name:              in.Name,
		Limit:             in.Limit,
		ExcludeDeprecated: !in.IncludeDeprecated,
	})
	if err != nil {
		return SchemaListResult{}, newError(fmt.Errorf("listing schemas: %w", err), output.ErrGeneral)
	}
	return SchemaListResult{Schemas: schemas, Total: total}, nil
}

// SchemaShowInput names the schema to show.
type SchemaShowInput struct {
	// Ref is name@version, or a bare name for the highest version still in
	// service. A bare name is the read verb's convenience and never what a
	// payload declaration or a pin may mean.
	Ref string
}

// SchemaShowResult is the registered schema, its document verbatim in Body.
type SchemaShowResult struct {
	Schema *model.Schema
}

// SchemaShow reads one schema from the caller's project. It is a read, so it
// never registers the caller's project.
func SchemaShow(ctx context.Context, env Env, in SchemaShowInput) (SchemaShowResult, error) {
	name, version, err := model.ParseWorkflowRef(in.Ref)
	if err != nil {
		return SchemaShowResult{}, newError(err, output.ErrValidation)
	}

	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return SchemaShowResult{}, err
	}

	s, err := env.Store.GetSchema(projectID, name, version)
	if errors.Is(err, store.ErrSchemaNotFound) {
		return SchemaShowResult{}, newError(DescribeMissingSchema(err, in.Ref), output.ErrNotFound)
	}
	if err != nil {
		return SchemaShowResult{}, newError(err, output.ErrGeneral)
	}
	return SchemaShowResult{Schema: s}, nil
}

// DescribeMissingSchema turns the storage sentinel into an operator-facing
// message naming the ref that was asked for. Any other error passes through.
func DescribeMissingSchema(err error, ref string) error {
	if err == store.ErrSchemaNotFound {
		return fmt.Errorf("%w: %s is not registered", store.ErrSchemaNotFound, ref)
	}
	return err
}
