package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

var workflowRegisterCmd = &cobra.Command{
	Use:   "register <file.toml>",
	Short: "Register a workflow definition",
	Long: `Parse, validate, and register a workflow definition.

Pass - as the file to read from stdin, so configuration generated in a pipeline
needs no temporary file.

Registering the same bytes at an existing name@version again is a success and
changes nothing. Registering DIFFERENT bytes at an existing name@version is a
CONFLICT: a registered name@version is frozen, so that a run which pinned it
cannot have the definition swapped underneath it.

A registry is PER PROJECT. By default the definition lands in the project the
working directory resolves to; --project registers it into one other project,
and --all-projects registers it into every project in the store. Both report
each project's own outcome, and each project's idempotency and conflict rules
are decided there — a conflict in one project neither cancels nor hides another
project's registration.

VALIDATION IS PER TARGET, not once for the invocation. A definition's
'vote_rule' and 'payload' references resolve against the registry of the project
being written to, so the SAME bytes can be valid in one project and reference a
schema that does not exist in the next. The project that cannot resolve them is
refused there and reported as invalid; the rest still register. Register the
schemas first — 'docket schema register <ref> <file> --all-projects' — when
sweeping a corpus into a store that has not seen it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowRegister(cmd, args, getWriter(cmd))
	},
}

func runWorkflowRegister(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	path := args[0]

	src, err := readWorkflowSource(cmd, path)
	if err != nil {
		return err
	}

	def, err := loadWorkflowSource(src, path)
	if err != nil {
		return workflowErr(err)
	}

	// The GRAMMAR is decided before the targets are: a definition that does not
	// parse is wrong everywhere, and telling an operator which of thirteen
	// projects rejected a typo would be noise. Everything past this point is
	// environment, and environment is per project.
	targets, fannedOut, err := resolveRegistryTargets(cmd, conn)
	if err != nil {
		return err
	}
	if fannedOut {
		return registerWorkflowAcrossProjects(cmd, w, conn, def, src, path, targets)
	}

	// V26 (gates-trust §8.2) and V21a-V21d/V25a (§4.9): vote rules and schemas,
	// resolved against THIS project's registry. See validateWorkflowEnvironment.
	if err := validateWorkflowEnvironment(conn, def, getProjectID(cmd)); err != nil {
		return workflowErr(err)
	}

	parsed, err := workflow.Canonical(def)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	stored, created, err := db.InsertWorkflow(
		conn, workflowRow(def, src, path, parsed, getProjectID(cmd)), model.NowMS())
	if err != nil {
		return workflowErr(err)
	}

	message := fmt.Sprintf("Registered %s", stored.Ref())
	if !created {
		message = fmt.Sprintf("%s is already registered with these bytes", stored.Ref())
	}
	w.Success(stored, message)
	return nil
}

// workflowRow builds the row to insert. Shared by the single-project path and
// the fan-out so the two can never store different columns for the same bytes.
func workflowRow(
	def *workflow.Definition, src []byte, path string, parsed []byte, projectID int,
) *model.Workflow {
	// source_path is provenance only — it records where the bytes came from
	// and is never re-read. stdin has no path to record.
	sourcePath := path
	if path == "-" {
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

// validateWorkflowEnvironment runs the two checks that ask a question about a
// PROJECT rather than about the bytes.
//
// V26 (gates-trust §8.2): every `vote_rule` names a REGISTERED threshold
// configuration. V21a-V21d and V25a (§4.9): threshold fields and literals
// cross-validated against the REGISTERED schema, per §11.2's "validated against
// the registered schema at `workflow register` time". Both run here rather than
// inside workflow.Validate, which stays pure and holds no database handle.
//
// The order — Validate, then vote rules, then schemas — is deliberate: an
// author sees GRAMMAR errors before ENVIRONMENT errors, so a definition with a
// typo in a step name is not first told that a schema is missing.
//
// Both resolve against a project's own registry, which is why the fan-out runs
// them once per target instead of once per invocation: a `payload` reference
// that resolves in the invoking project may name nothing in the next one, and
// registering there anyway would store a definition guaranteed to fail at
// activation — the exact failure this validation exists to move forward in time.
func validateWorkflowEnvironment(
	conn *sql.DB, def *workflow.Definition, projectID int,
) error {
	if err := workflow.ValidateVoteRules(def, voteRuleResolver{conn, projectID}); err != nil {
		return err
	}
	return workflow.ValidateSchemas(def, schemaResolver{conn, projectID})
}

// registerWorkflowAcrossProjects is the --project / --all-projects path.
//
// The bytes are canonicalized ONCE — that is a pure function of the definition
// and cannot differ per project — and then each target gets its own validation
// and its own insert. A failure is recorded and the loop continues: the whole
// point of the flag is that one command covers a store, and stopping at the
// first conflict would leave the operator to work out by hand which projects
// the sweep had reached.
func registerWorkflowAcrossProjects(
	cmd *cobra.Command, w *output.Writer, conn *sql.DB,
	def *workflow.Definition, src []byte, path string, targets []*model.Project,
) error {
	parsed, err := workflow.Canonical(def)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	report := &registryFanoutReport{
		Operation: "workflow register",
		Subject:   fmt.Sprintf("%s@%d", def.Pipeline.Name, def.Pipeline.Version),
		Scope:     fanoutScope(cmd),
	}

	for _, target := range targets {
		if err := validateWorkflowEnvironment(conn, def, target.ID); err != nil {
			report.Results = append(report.Results,
				registryFailureResult(target, err, workflowErr))
			continue
		}

		stored, created, err := db.InsertWorkflow(
			conn, workflowRow(def, src, path, parsed, target.ID), model.NowMS())
		if err != nil {
			report.Results = append(report.Results,
				registryFailureResult(target, err, workflowErr))
			continue
		}

		outcome := outcomeUnchanged
		if created {
			outcome = outcomeRegistered
		}
		report.Results = append(report.Results,
			registrySuccessResult(target, outcome, stored.Ref()))
	}

	return finishRegistryFanout(w, report)
}

// readWorkflowSource reads the definition bytes from a file or, for "-", from
// stdin.
func readWorkflowSource(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		src, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, cmdErr(fmt.Errorf("reading workflow from stdin: %w", err), output.ErrGeneral)
		}
		if len(src) == 0 {
			return nil, cmdErr(
				fmt.Errorf("no workflow definition on stdin"), output.ErrValidation)
		}
		return src, nil
	}

	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, cmdErr(fmt.Errorf("workflow file %s not found", path), output.ErrNotFound)
		}
		return nil, cmdErr(
			fmt.Errorf("reading workflow file %s: %w", path, err), output.ErrNotFound)
	}
	return src, nil
}

func init() {
	addRegistryTargetFlags(workflowRegisterCmd, "Register")
	workflowCmd.AddCommand(workflowRegisterCmd)
}

// schemaResolver adapts the schema registry to V21a-V21d/V25a's resolver, so
// the validator never holds a database handle.
//
// It COMPILES the stored bytes rather than reading a cached parse, because the
// rules ask about the document's declared fields and their orders, and the
// registered bytes are the only authority on those.
type schemaResolver struct {
	conn *sql.DB
	// projectID scopes resolution (v12): a `payload` reference resolves in
	// the invoking project's registry, plus builtins.
	projectID int
}

func (r schemaResolver) Schema(name string, version int) (*workflow.Registered, error) {
	row, err := db.GetSchema(r.conn, r.projectID, name, version)
	if errors.Is(err, db.ErrSchemaNotFound) {
		return nil, fmt.Errorf("%w: %s@%d", workflow.ErrNotRegistered, name, version)
	}
	if err != nil {
		return nil, err
	}
	return schema.Compile(row.Name, row.Version, []byte(row.Body))
}

// voteRuleResolver adapts the engine-config registry to V26's resolver, so the
// validator never holds a database handle.
type voteRuleResolver struct {
	conn *sql.DB
	// projectID scopes rule resolution (v12): a project sees its own rules
	// plus the store-wide ones.
	projectID int
}

func (r voteRuleResolver) VoteRuleExists(rule string) (bool, error) {
	return db.VoteRuleExists(r.conn, r.projectID, rule)
}

func (r voteRuleResolver) RuleSetElsewhere(rule string) (int, string, error) {
	return db.VoteRuleSetElsewhere(r.conn, r.projectID, rule)
}

func (r voteRuleResolver) RegisteredVoteRules() ([]string, error) {
	return db.RegisteredVoteRules(r.conn, r.projectID)
}
