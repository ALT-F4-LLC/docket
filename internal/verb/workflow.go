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

	def, err := loadDefinition(in.Body, in.SourcePath)
	if err != nil {
		return WorkflowLintResult{}, err
	}
	result := WorkflowLintResult{
		Name:    def.Pipeline.Name,
		Version: def.Pipeline.Version,
		SHA256:  workflow.SHA256(in.Body),
	}

	if in.AllProjects {
		projects, err := allRegistryProjects(env.Store)
		if err != nil {
			return WorkflowLintResult{}, err
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
	if err := validateWorkflowEnvironment(st, def, projectID); err != nil {
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

// allRegistryProjects is every project in the store, the --all-projects target
// set. An empty store is refused: a sweep over zero targets would read as a
// success that did nothing.
func allRegistryProjects(st store.Store) ([]*model.Project, error) {
	projects, err := st.ListProjects()
	if err != nil {
		return nil, newError(fmt.Errorf("listing projects: %w", err), output.ErrGeneral)
	}
	if len(projects) == 0 {
		return nil, newError(fmt.Errorf(
			"--all-projects found no projects in this store"), output.ErrNotFound)
	}
	return projects, nil
}

// registryTargets answers which projects a registry write names: the one
// project ref names, or every project under all. Nil means it named none, and
// the write lands in the caller's own project.
func registryTargets(env Env, ref string, all bool) ([]*model.Project, error) {
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
		return allRegistryProjects(env.Store)
	default:
		return nil, nil
	}
}

// WorkflowTargetOutcome is one named project's outcome of a registry write:
// the row the write reached and whether it changed that row, or Err, coded
// as the single-project write would code it, when that project refused.
type WorkflowTargetOutcome struct {
	Project  *model.Project
	Workflow *model.Workflow
	Changed  bool
	Err      error
}

// WorkflowRegisterInput is a definition to register.
type WorkflowRegisterInput struct {
	// Source returns the definition's bytes. WorkflowRegister calls it once,
	// after it has resolved the caller's project, so a transport that reads a
	// file reports an unreadable one only where it always did. Its error is
	// returned unchanged; the verb never opens a path.
	Source func() ([]byte, error)
	// SourcePath is recorded as the row's provenance and names the file in a
	// grammar error; empty or "-" names none.
	SourcePath string
	// Project registers into one other project instead of the caller's, by
	// any key ResolveProjectRef accepts.
	Project string
	// AllProjects registers into every project in the store.
	AllProjects bool
}

// WorkflowRegisterResult is the registration. Workflow and Created answer for
// the caller's project; under Project or AllProjects, Targets holds each
// named project's outcome instead, Changed meaning the row was inserted.
type WorkflowRegisterResult struct {
	Name     string
	Version  int
	Workflow *model.Workflow
	Created  bool
	Targets  []WorkflowTargetOutcome
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when the call also returns an error: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// WorkflowRegister validates a definition and registers it. The grammar is
// checked once, since a definition that does not parse is wrong in every
// project; the vote-rule and schema checks and the insert then run per
// project, and one project's refusal neither cancels nor hides another's
// registration. It writes into the caller's project, so it may register it.
func WorkflowRegister(ctx context.Context, env Env, in WorkflowRegisterInput) (WorkflowRegisterResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "workflow register")
	res := WorkflowRegisterResult{Registered: registered}
	if err != nil {
		return res, err
	}

	src, err := in.Source()
	if err != nil {
		return res, err
	}
	def, err := loadDefinition(src, in.SourcePath)
	if err != nil {
		return res, err
	}
	res.Name, res.Version = def.Pipeline.Name, def.Pipeline.Version

	targets, err := registryTargets(env, in.Project, in.AllProjects)
	if err != nil {
		return res, err
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		return res, newError(err, output.ErrGeneral)
	}
	row := func(projectID int) *model.Workflow {
		return workflowRow(def, src, in.SourcePath, parsed, projectID)
	}

	for _, target := range targets {
		outcome := WorkflowTargetOutcome{Project: target}
		outcome.Workflow, outcome.Changed, err = registerWorkflowIn(env.Store, def, row(target.ID))
		if err != nil {
			outcome.Err = workflowError(err)
		}
		res.Targets = append(res.Targets, outcome)
	}
	if targets != nil {
		return res, nil
	}

	res.Workflow, res.Created, err = registerWorkflowIn(env.Store, def, row(projectID))
	if err != nil {
		return res, workflowError(err)
	}
	return res, nil
}

// registerWorkflowIn runs one project's environment checks and inserts the
// row there.
func registerWorkflowIn(st store.Store, def *workflow.Definition, row *model.Workflow) (*model.Workflow, bool, error) {
	if err := validateWorkflowEnvironment(st, def, row.ProjectID); err != nil {
		return nil, false, err
	}
	return st.InsertWorkflow(row, model.NowMS())
}

// workflowRow builds the row a register inserts, so every target stores the
// same columns for the same bytes. The source path is provenance only and is
// never re-read; stdin has none to record.
func workflowRow(def *workflow.Definition, src []byte, sourcePath string, parsed []byte, projectID int) *model.Workflow {
	if sourcePath == "-" {
		sourcePath = ""
	}
	return &model.Workflow{
		ProjectID:    projectID,
		Name:         def.Pipeline.Name,
		Version:      def.Pipeline.Version,
		Description:  def.Pipeline.Description,
		SourcePath:   sourcePath,
		SourceSHA256: workflow.SHA256(src),
		Body:         string(src),
		Parsed:       string(parsed),
	}
}

// WorkflowDeprecateInput names the version to retire from binding, or to
// restore to it.
type WorkflowDeprecateInput struct {
	// Ref is name@version. The version is required: retirement applies to one
	// registered version.
	Ref     string
	Restore bool
	// Project and AllProjects name the targets as on WorkflowRegisterInput.
	Project     string
	AllProjects bool
}

// WorkflowDeprecateResult is the retired or restored row in the caller's
// project; under Project or AllProjects, Targets holds each named project's
// outcome instead. A retirement always changes the row; a restore changes it
// only when the version was retired.
type WorkflowDeprecateResult struct {
	Workflow *model.Workflow
	Targets  []WorkflowTargetOutcome
	// Registered is as on WorkflowRegisterResult.
	Registered *engine.ProjectRegistration
}

// WorkflowDeprecate retires one registered version from binding, or restores
// it. Retirement never deletes: the row stays readable and runs that pinned it
// still resolve it. Restore is idempotent where a second retirement is a
// conflict. It writes into the caller's project, so it may register it.
func WorkflowDeprecate(ctx context.Context, env Env, in WorkflowDeprecateInput) (WorkflowDeprecateResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "workflow deprecate")
	res := WorkflowDeprecateResult{Registered: registered}
	if err != nil {
		return res, err
	}

	name, version, err := model.ParseWorkflowRef(in.Ref)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}
	// A bare name would make the verb act on whichever version is highest
	// today, a different row tomorrow.
	if version == 0 {
		return res, newError(fmt.Errorf(
			"deprecate needs an explicit version, as %s@<version>: "+
				"retirement applies to one registered version, and a bare name "+
				"would silently mean whichever is highest today", name),
			output.ErrValidation)
	}

	targets, err := registryTargets(env, in.Project, in.AllProjects)
	if err != nil {
		return res, err
	}
	for _, target := range targets {
		res.Targets = append(res.Targets,
			retireWorkflowIn(env.Store, target, name, version, in))
	}
	if targets != nil {
		return res, nil
	}

	if in.Restore {
		wf, err := env.Store.RestoreWorkflow(projectID, name, version)
		if err != nil {
			return res, workflowError(DescribeMissingWorkflow(err, in.Ref))
		}
		res.Workflow = wf
		return res, nil
	}
	wf, err := env.Store.DeprecateWorkflow(projectID, name, version, model.NowMS())
	if err != nil {
		if errors.Is(err, store.ErrWorkflowAlreadyDeprecated) {
			return res, newError(fmt.Errorf("%s is already deprecated", in.Ref),
				output.ErrConflict)
		}
		return res, workflowError(DescribeMissingWorkflow(err, in.Ref))
	}
	res.Workflow = wf
	return res, nil
}

// retireWorkflowIn is one named project's retirement or restore. A restore
// reads the row first, since RestoreWorkflow returns the same row whether or
// not it was retired.
func retireWorkflowIn(st store.Store, target *model.Project, name string, version int, in WorkflowDeprecateInput) WorkflowTargetOutcome {
	outcome := WorkflowTargetOutcome{Project: target, Changed: true}
	var (
		wf  *model.Workflow
		err error
	)
	if in.Restore {
		before, lookupErr := st.GetWorkflow(target.ID, name, version)
		if lookupErr != nil {
			outcome.Err = targetRetireError(lookupErr, in.Ref)
			return outcome
		}
		outcome.Changed = before.Deprecated()
		wf, err = st.RestoreWorkflow(target.ID, name, version)
	} else {
		wf, err = st.DeprecateWorkflow(target.ID, name, version, model.NowMS())
	}
	if err != nil {
		outcome.Err = targetRetireError(err, in.Ref)
		return outcome
	}
	outcome.Workflow = wf
	return outcome
}

// targetRetireError gives a named project's refusal the sentence the
// single-project write gives it, keeping the sentinel wrapped so a report can
// still tell a second retirement from a missing row.
func targetRetireError(err error, ref string) error {
	if errors.Is(err, store.ErrWorkflowAlreadyDeprecated) {
		return newError(fmt.Errorf("%s is already deprecated: %w", ref, err), output.ErrConflict)
	}
	return workflowError(DescribeMissingWorkflow(err, ref))
}

// loadDefinition parses and validates a definition's grammar, naming the
// source path in the error so an author registering several files knows
// which one failed.
func loadDefinition(src []byte, sourcePath string) (*workflow.Definition, error) {
	def, err := workflow.Load(src)
	if err != nil {
		if sourcePath != "" && sourcePath != "-" {
			err = fmt.Errorf("%s: %w", sourcePath, err)
		}
		return nil, workflowError(err)
	}
	return def, nil
}

// validateWorkflowEnvironment runs the checks that ask a question about a
// project rather than about the bytes: every vote rule names a registered
// threshold configuration, and every payload reference resolves to a schema
// in that project's registry. An author sees grammar errors before these.
func validateWorkflowEnvironment(st store.Store, def *workflow.Definition, projectID int) error {
	if err := workflow.ValidateVoteRules(def, voteRuleResolver{st, projectID}); err != nil {
		return err
	}
	return workflow.ValidateSchemas(def, schemaResolver{st, projectID})
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
