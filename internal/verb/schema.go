package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
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

// ErrSchemaInUse is SchemaDeprecate's refusal of a version that a workflow
// still in service names as a step's payload.
var ErrSchemaInUse = errors.New("schema is referenced by a workflow still in service")

// SchemaProjectResult is one project's outcome when a schema verb targets
// other projects: the row it reached, or Err when that project refused.
type SchemaProjectResult struct {
	Project *model.Project
	Schema  *model.Schema
	// Changed reports that this call changed the project's row: inserted
	// it, retired it, or returned it to service.
	Changed bool
	Err     error
}

// SchemaRegisterInput is the schema document to register at Ref.
type SchemaRegisterInput struct {
	// Ref is name@version; the version is required.
	Ref string
	// Document returns the schema document's bytes. SchemaRegister calls it
	// once, after it has resolved the project and parsed Ref, so a transport
	// that reads a file reports an unreadable one only where it always did.
	// Its error is returned unchanged, for the transport to word and code.
	Document func() ([]byte, error)
	// SourcePath records where Document came from, as provenance, and
	// prefixes a refusal of the document.
	SourcePath string
	// Project registers into one other project, by any key
	// ResolveProjectRef accepts, instead of the caller's.
	Project string
	// AllProjects registers into every project in the store.
	AllProjects bool
}

// SchemaRegisterResult is the registered row and whether this call inserted
// it, or under Project or AllProjects each target's outcome in Projects.
type SchemaRegisterResult struct {
	// Name and Version are Ref as parsed.
	Name     string
	Version  int
	Schema   *model.Schema
	Created  bool
	Projects []SchemaProjectResult
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when SchemaRegister also returns an error: read it before the error.
	Registered *engine.ProjectRegistration
}

// SchemaRegister compiles a schema document and registers it. It writes into
// the caller's project, so it may register that project; Project and
// AllProjects instead target others, each judged on its own. The document is
// judged before the targets are, since bytes that do not compile are wrong in
// every project. Identical bytes at a registered name@version are an
// unchanged success, and different bytes are a CONFLICT.
func SchemaRegister(ctx context.Context, env Env, in SchemaRegisterInput) (SchemaRegisterResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "schema register")
	res := SchemaRegisterResult{Registered: registered}
	if err != nil {
		return res, err
	}

	name, version, err := workflow.ParsePayloadRef(in.Ref)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}
	res.Name, res.Version = name, version

	src, err := in.Document()
	if err != nil {
		return res, err
	}

	// Compile checks the ordered_enum annotation rules and then compiles the
	// document as ordinary JSON Schema. Both refusals name the property path.
	compiled, err := schema.Compile(name, version, src)
	if err != nil {
		return res, schemaError(fmt.Errorf("%s: %w", in.SourcePath, err))
	}

	// The derived index is stored next to the bytes it came from, so nothing
	// re-walks a schema document during evaluation.
	ordered, err := json.Marshal(compiled.Ordered)
	if err != nil {
		return res, newError(fmt.Errorf("encoding the ordered index: %w", err), output.ErrGeneral)
	}

	row := func(projectID int) *model.Schema {
		return &model.Schema{
			ProjectID: projectID,
			Name:      name,
			Version:   version,
			// SourcePath is provenance only and is never re-read.
			SourcePath:   in.SourcePath,
			SourceSHA256: workflow.SHA256(src),
			Body:         string(src),
			Ordered:      string(ordered),
		}
	}

	targets, err := registryWriteTargets(env, in.Project, in.AllProjects)
	if err != nil {
		return res, err
	}
	if targets != nil {
		for _, target := range targets {
			stored, created, err := env.Store.InsertSchema(row(target.ID), model.NowMS())
			outcome := SchemaProjectResult{Project: target, Schema: stored, Changed: created}
			if err != nil {
				outcome = SchemaProjectResult{Project: target, Err: schemaError(err)}
			}
			res.Projects = append(res.Projects, outcome)
		}
		return res, nil
	}

	res.Schema, res.Created, err = env.Store.InsertSchema(row(projectID), model.NowMS())
	if err != nil {
		return res, schemaError(err)
	}
	return res, nil
}

// SchemaDeprecateInput names the schema version to retire or restore.
type SchemaDeprecateInput struct {
	// Ref is name@version; the version is required.
	Ref string
	// Restore returns a retired version to service instead.
	Restore bool
	// Project acts in one other project, by any key ResolveProjectRef
	// accepts, instead of the caller's.
	Project string
	// AllProjects acts in every project in the store.
	AllProjects bool
}

// SchemaDeprecateResult is the retired or restored row, or under Project or
// AllProjects each target's outcome in Projects.
type SchemaDeprecateResult struct {
	Schema   *model.Schema
	Projects []SchemaProjectResult
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when SchemaDeprecate also returns an error: read it before the error.
	Registered *engine.ProjectRegistration
}

// SchemaDeprecate retires a schema version from service, or with Restore
// returns it. It writes into the caller's project, so it may register that
// project; Project and AllProjects instead target others, each judged on its
// own. Retirement refuses a version a workflow still in service names as a
// payload, retiring twice is a CONFLICT, and restoring a version already in
// service is a success.
func SchemaDeprecate(ctx context.Context, env Env, in SchemaDeprecateInput) (SchemaDeprecateResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "schema deprecate")
	res := SchemaDeprecateResult{Registered: registered}
	if err != nil {
		return res, err
	}

	// The reference grammar is the registry's own payload shape, so an
	// explicit version is required: retirement applies to exactly one.
	name, version, err := workflow.ParsePayloadRef(in.Ref)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}

	targets, err := registryWriteTargets(env, in.Project, in.AllProjects)
	if err != nil {
		return res, err
	}
	if targets != nil {
		for _, target := range targets {
			outcome := SchemaProjectResult{Project: target, Changed: true}
			var err error
			if in.Restore {
				var before *model.Schema
				before, err = env.Store.GetSchema(target.ID, name, version)
				if err == nil {
					outcome.Changed = before.Deprecated()
					outcome.Schema, err = env.Store.RestoreSchema(target.ID, name, version)
				}
			} else {
				outcome.Schema, err = deprecateSchemaIn(env.Store, target.ID, name, version)
			}
			if err != nil {
				outcome = SchemaProjectResult{Project: target, Err: schemaDeprecateError(err, in.Ref)}
			}
			res.Projects = append(res.Projects, outcome)
		}
		return res, nil
	}

	if in.Restore {
		res.Schema, err = env.Store.RestoreSchema(projectID, name, version)
	} else {
		res.Schema, err = deprecateSchemaIn(env.Store, projectID, name, version)
	}
	if err != nil {
		return res, schemaDeprecateError(err, in.Ref)
	}
	return res, nil
}

// deprecateSchemaIn is one project's retirement: the reference check, then
// the write. The check runs outside the write's transaction, which is
// acceptable because a workflow registered between the two is refused by its
// own retired-schema check the moment the retirement lands: the race resolves
// to the same refusal, attributed to the other command.
func deprecateSchemaIn(st store.Store, projectID int, name string, version int) (*model.Schema, error) {
	referencing, err := engine.WorkflowsReferencingSchemaStore(st, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if len(referencing) > 0 {
		return nil, fmt.Errorf(
			"%w: %s@%d is named as `payload` by %s. Retire or re-version those "+
				"workflows first; their next registration or activation would "+
				"otherwise be refused",
			ErrSchemaInUse, name, version, strings.Join(referencing, ", "))
	}
	return st.DeprecateSchema(projectID, name, version, model.NowMS())
}

// schemaDeprecateError words a retirement failure the same way in one project
// or across many, keeping the sentinels wrapped, and codes it.
func schemaDeprecateError(err error, ref string) error {
	switch {
	case errors.Is(err, store.ErrSchemaAlreadyDeprecated):
		err = fmt.Errorf("%s is already deprecated: %w", ref, err)
	case errors.Is(err, store.ErrSchemaBuiltin):
		err = fmt.Errorf("%s ships with docket and is visible to every project; "+
			"it cannot be retired: %w", ref, err)
	default:
		err = DescribeMissingSchema(err, ref)
	}
	return schemaError(err)
}

// schemaError codes a registry failure: a schema-document refusal is a
// validation failure, a store state that contradicts the caller's premise
// (different bytes, retired twice, still referenced) is a CONFLICT, and the
// builtin cannot be retired.
func schemaError(err error) error {
	var se *schema.Error
	if errors.As(err, &se) {
		return newError(err, output.ErrValidation)
	}
	switch {
	case errors.Is(err, store.ErrSchemaConflict),
		errors.Is(err, store.ErrSchemaAlreadyDeprecated),
		errors.Is(err, ErrSchemaInUse):
		return newError(err, output.ErrConflict)
	case errors.Is(err, store.ErrSchemaNotFound):
		return newError(err, output.ErrNotFound)
	case errors.Is(err, store.ErrSchemaBuiltin):
		return newError(err, output.ErrValidation)
	default:
		return newError(err, output.ErrGeneral)
	}
}

// registryWriteTargets answers which other projects a registry write targets:
// the one project ref names, or every project under all. Nil means neither
// was asked for and the write goes to the caller's own project.
func registryWriteTargets(env Env, ref string, all bool) ([]*model.Project, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref != "" && all:
		return nil, newError(fmt.Errorf(
			"--project and --all-projects both name the targets and disagree: "+
				"pass one project's ref, or --all-projects for every project in "+
				"the store"), output.ErrValidation)
	case ref != "":
		target, err := ResolveProjectRef(env.Store, ref)
		if err != nil {
			return nil, err
		}
		return []*model.Project{target}, nil
	case all:
		projects, err := env.Store.ListProjects()
		if err != nil {
			return nil, newError(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
		}
		if len(projects) == 0 {
			// A fan-out that silently reported zero targets would look like
			// a success that did nothing.
			return nil, newError(fmt.Errorf(
				"--all-projects found no projects in this store"), output.ErrNotFound)
		}
		return projects, nil
	default:
		return nil, nil
	}
}
