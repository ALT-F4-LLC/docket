package verb

import (
	"context"
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

// WorkflowListInput selects the registered workflows to list.
type WorkflowListInput struct {
	// Name filters to one workflow's versions; empty lists every name.
	Name string
	// Limit caps the rows returned; zero means no limit.
	Limit int
	// IncludeDeprecated adds retired versions, which are hidden by default.
	// It has no effect under Orphans.
	IncludeDeprecated bool
	// Orphans narrows the list to names no file in any instance-config root
	// on this host declares, deprecated versions included.
	Orphans bool
	// Project names another project's registry to read, by any key
	// ResolveProjectRef accepts; empty reads the caller's.
	Project string
}

// WorkflowListResult is one page of workflows. Total counts every match,
// ignoring Limit, so a caller can tell the page was truncated.
type WorkflowListResult struct {
	Workflows []*model.Workflow
	Total     int
}

// WorkflowList lists the workflows registered in one project. It is a read,
// so it never registers the caller's project.
func WorkflowList(ctx context.Context, env Env, in WorkflowListInput) (WorkflowListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return WorkflowListResult{}, err
	}
	if projectID, err = registryProject(env, in.Project, projectID); err != nil {
		return WorkflowListResult{}, err
	}

	// The orphan verdict is applied over the query's rows, so the query runs
	// unlimited under Orphans: a SQL LIMIT would cut the rows before the
	// filter saw them, and Total would count workflows rather than orphans.
	queryLimit := in.Limit
	if in.Orphans {
		queryLimit = 0
	}
	workflows, total, err := env.Store.ListWorkflows(store.WorkflowListOptions{
		ProjectID:         projectID,
		Name:              in.Name,
		Limit:             queryLimit,
		ExcludeDeprecated: !in.IncludeDeprecated && !in.Orphans,
	})
	if err != nil {
		return WorkflowListResult{}, newError(fmt.Errorf("listing workflows: %w", err), output.ErrGeneral)
	}

	if in.Orphans {
		if workflows, total, err = keepOrphans(workflows, in.Limit); err != nil {
			return WorkflowListResult{}, err
		}
	}
	return WorkflowListResult{Workflows: workflows, Total: total}, nil
}

// keepOrphans reduces the listed rows to the orphaned ones, stamping each with
// the verdict that put it there, and returns the true pre-limit total of that
// population.
//
// It refuses rather than reporting an unchecked answer: with no
// instance-config root to scan, every registration trivially has "no file in
// any root". The two ways that happens, no root and a root that would not
// scan, are separate messages because they send an operator to different
// places.
func keepOrphans(workflows []*model.Workflow, limit int) ([]*model.Workflow, int, error) {
	index, err := engine.ScanWorkflowOrigins()
	if err != nil {
		return nil, 0, newError(err, output.ErrValidation)
	}
	if !index.Scanned() {
		return nil, 0, newError(fmt.Errorf(
			"no instance-config root exists on this machine, so no registration "+
				"can be classified: with nothing to scan, every registered name "+
				"would look orphaned"), output.ErrValidation)
	}
	if err := index.Err(); err != nil {
		return nil, 0, newError(fmt.Errorf(
			"scanning the instance config for workflow definitions: %w", err),
			output.ErrGeneral)
	}

	out := make([]*model.Workflow, 0, len(workflows))
	for _, wf := range workflows {
		status := index.Status(wf.Name)
		if !status.Orphaned() {
			continue
		}
		wf.Origin = status
		out = append(out, wf)
	}

	total := len(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// WorkflowShowInput names the workflow to show.
type WorkflowShowInput struct {
	// Ref is name@version, or a bare name for the highest version that is
	// not deprecated, the one a new run would bind.
	Ref string
	// Source asks for the stored bytes only, so the source-drift check is
	// skipped.
	Source bool
}

// WorkflowShowResult is the registered workflow. Unless the input asked for
// Source, its SourceStatus says whether the file at its recorded path still
// holds the registered bytes.
type WorkflowShowResult struct {
	Workflow *model.Workflow
}

// WorkflowShow reads one workflow from the caller's project. It is a read, so
// it never registers the caller's project.
func WorkflowShow(ctx context.Context, env Env, in WorkflowShowInput) (WorkflowShowResult, error) {
	name, version, err := model.ParseWorkflowRef(in.Ref)
	if err != nil {
		return WorkflowShowResult{}, newError(err, output.ErrValidation)
	}

	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return WorkflowShowResult{}, err
	}

	wf, err := env.Store.GetWorkflow(projectID, name, version)
	if err != nil {
		return WorkflowShowResult{}, workflowError(DescribeMissingWorkflow(err, in.Ref))
	}
	if !in.Source {
		wf.SourceStatus = engine.CheckWorkflowSource(wf.SourcePath, wf.SourceSHA256)
	}
	return WorkflowShowResult{Workflow: wf}, nil
}

// DescribeMissingWorkflow turns the storage sentinel into an operator-facing
// message naming what was asked for.
func DescribeMissingWorkflow(err error, ref string) error {
	if err == store.ErrWorkflowNotFound {
		return fmt.Errorf("%w: %s is not registered", store.ErrWorkflowNotFound, ref)
	}
	return err
}

// The registrations a lint reports: what a real register would do.
const (
	// WorkflowLintNew is a free slot; a register would insert.
	WorkflowLintNew = "new"
	// WorkflowLintUnchanged is identical bytes already registered.
	WorkflowLintUnchanged = "unchanged"
)

// WorkflowLintInput is the definition to lint.
type WorkflowLintInput struct {
	Body []byte
	// SourcePath names where Body came from, for error messages; empty or
	// "-" names none.
	SourcePath string
	// Project names another project's registry to judge against, by any key
	// ResolveProjectRef accepts; empty judges against the caller's.
	Project string
	// AllProjects judges against every project in the store instead, and
	// takes precedence over Project.
	AllProjects bool
}

// WorkflowLintResult is the definition's identity and its verdict:
// Registration for one project, or Projects under AllProjects.
type WorkflowLintResult struct {
	Name         string
	Version      int
	SHA256       string
	Registration string
	Projects     []WorkflowLintVerdict
}

// WorkflowLintVerdict is one project's verdict under AllProjects: its
// Registration, or Err when a register there would refuse.
type WorkflowLintVerdict struct {
	Project      *model.Project
	Registration string
	Err          error
}

// WorkflowLint runs the validation a register runs and writes nothing. The
// grammar is checked once, since a definition that does not parse is wrong in
// every project; the vote-rule and schema checks and the registration probe
// then run against one project's registry, or each project's in turn. It is a
// read, so it never registers the caller's project.
func WorkflowLint(ctx context.Context, env Env, in WorkflowLintInput) (WorkflowLintResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return WorkflowLintResult{}, err
	}

	def, err := workflow.Load(in.Body)
	if err != nil {
		if in.SourcePath != "" && in.SourcePath != "-" {
			err = fmt.Errorf("%s: %w", in.SourcePath, err)
		}
		return WorkflowLintResult{}, workflowError(err)
	}
	result := WorkflowLintResult{
		Name:    def.Pipeline.Name,
		Version: def.Pipeline.Version,
		SHA256:  workflow.SHA256(in.Body),
	}

	if in.AllProjects {
		projects, err := env.Store.ListProjects()
		if err != nil {
			return WorkflowLintResult{}, newError(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
		}
		if len(projects) == 0 {
			return WorkflowLintResult{}, newError(fmt.Errorf(
				"--all-projects found no projects in this store"), output.ErrNotFound)
		}
		for _, p := range projects {
			verdict := WorkflowLintVerdict{Project: p}
			verdict.Registration, err = lintVerdict(env.Store, def, in.Body, p.ID)
			if err != nil {
				verdict.Err = workflowError(err)
			}
			result.Projects = append(result.Projects, verdict)
		}
		return result, nil
	}

	// One project for every question: the vote-rule check, the schema check
	// and the registration probe must consult the same registry, or the
	// verdict would be a composite no single register could reach.
	if projectID, err = registryProject(env, in.Project, projectID); err != nil {
		return WorkflowLintResult{}, err
	}
	if result.Registration, err = lintVerdict(env.Store, def, in.Body, projectID); err != nil {
		return WorkflowLintResult{}, workflowError(err)
	}
	return result, nil
}

// lintConflictError is lint's CONFLICT: the message an author reads,
// unwrapping to store.ErrWorkflowConflict so it is classified as a register
// conflict.
type lintConflictError struct{ msg string }

func (e *lintConflictError) Error() string { return e.msg }
func (e *lintConflictError) Unwrap() error { return store.ErrWorkflowConflict }

// lintVerdict is one project's answer: the environment checks, then what a
// real register there would do, or the refusal.
//
// The registry probe starts from the bytes handed in and looks up the row
// they declare, so an edited file at a frozen name@version is caught here,
// while the author is still looking at it, rather than at activation. That is
// also why lint reports no source-drift verdict as WorkflowShow does: drift
// starts from a registered row's recorded path, and a bumped file always
// leaves the superseded version's path holding different bytes, so it would
// fire on every ordinary version bump.
func lintVerdict(st store.Store, def *workflow.Definition, src []byte, projectID int) (string, error) {
	if err := workflow.ValidateVoteRules(def, voteRuleResolver{st, projectID}); err != nil {
		return "", err
	}
	if err := workflow.ValidateSchemas(def, schemaResolver{st, projectID}); err != nil {
		return "", err
	}

	sum := workflow.SHA256(src)
	existing, err := st.GetWorkflow(projectID, def.Pipeline.Name, def.Pipeline.Version)
	switch {
	case errors.Is(err, store.ErrWorkflowNotFound):
		return WorkflowLintNew, nil
	case err != nil:
		return "", err
	case existing.SourceSHA256 == sum:
		return WorkflowLintUnchanged, nil
	default:
		return "", &lintConflictError{msg: fmt.Sprintf(
			"%s@%d is registered with different bytes\n\n"+
				"  registered  sha256:%s\n  this file   sha256:%s\n\n"+
				"A registered name@version is frozen so that a run which pinned it "+
				"can reproduce. To adopt these changes, bump [pipeline].version to "+
				"%d and lint again",
			def.Pipeline.Name, def.Pipeline.Version,
			existing.SourceSHA256, sum, def.Pipeline.Version+1)}
	}
}

// registryProject answers which one project a registry read consults: the
// project ref names when it is set, else the caller's own.
func registryProject(env Env, ref string, callers int) (int, error) {
	if ref = strings.TrimSpace(ref); ref == "" {
		return callers, nil
	}
	target, err := ResolveProjectRef(env.Store, ref)
	if err != nil {
		return 0, err
	}
	return target.ID, nil
}

// workflowError codes a workflow failure per the register taxonomy: a
// definition error is a validation failure, and the registry's conflict and
// not-found sentinels keep their own codes.
func workflowError(err error) error {
	var we *workflow.Error
	switch {
	case errors.As(err, &we):
		return newError(err, output.ErrValidation)
	case errors.Is(err, store.ErrWorkflowConflict):
		return newError(err, output.ErrConflict)
	case errors.Is(err, store.ErrWorkflowNotFound):
		return newError(err, output.ErrNotFound)
	default:
		return newError(err, output.ErrGeneral)
	}
}

// voteRuleResolver adapts the engine-config registry to the vote-rule
// validator, so the validator never holds a store. A project sees its own
// rules plus the store-wide ones.
type voteRuleResolver struct {
	st        store.Store
	projectID int
}

func (r voteRuleResolver) VoteRuleExists(rule string) (bool, error) {
	return r.st.VoteRuleExists(r.projectID, rule)
}

func (r voteRuleResolver) RuleSetElsewhere(rule string) (int, string, error) {
	return r.st.VoteRuleSetElsewhere(r.projectID, rule)
}

func (r voteRuleResolver) RegisteredVoteRules() ([]string, error) {
	return r.st.RegisteredVoteRules(r.projectID)
}

// schemaResolver adapts the schema registry to the schema validator, so the
// validator never holds a store. It compiles the stored bytes, the only
// authority on a schema's declared fields, and refuses a retired version for
// a new reference.
type schemaResolver struct {
	st        store.Store
	projectID int
}

func (r schemaResolver) Schema(name string, version int) (*workflow.Registered, error) {
	row, err := r.st.GetSchema(r.projectID, name, version)
	if errors.Is(err, store.ErrSchemaNotFound) {
		return nil, fmt.Errorf("%w: %s@%d", workflow.ErrNotRegistered, name, version)
	}
	if err != nil {
		return nil, err
	}
	if row.Deprecated() {
		return nil, fmt.Errorf("%w: %s@%d", workflow.ErrRetired, name, version)
	}
	return schema.Compile(row.Name, row.Version, []byte(row.Body))
}
