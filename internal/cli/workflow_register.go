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
cannot have the definition swapped underneath it.`,
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

	// V26 (gates-trust §8.2): every `vote_rule` names a REGISTERED threshold
	// configuration. It runs here rather than inside workflow.Validate because
	// it is the one rule that asks a question about the environment rather than
	// about the bytes, and Validate stays pure.
	if err := workflow.ValidateVoteRules(def, voteRuleResolver{conn, getProjectID(cmd)}); err != nil {
		return workflowErr(err)
	}

	// V21a-V21d and V25a (§4.9): threshold fields and literals cross-validated
	// against the REGISTERED schema, per §11.2's "validated against the
	// registered schema at `workflow register` time".
	//
	// The order — Validate, then vote rules, then schemas — is deliberate: an
	// author sees GRAMMAR errors before ENVIRONMENT errors, so a definition with
	// a typo in a step name is not first told that a schema is missing.
	if err := workflow.ValidateSchemas(def, schemaResolver{conn, getProjectID(cmd)}); err != nil {
		return workflowErr(err)
	}

	parsed, err := workflow.Canonical(def)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	// source_path is provenance only — it records where the bytes came from
	// and is never re-read. stdin has no path to record.
	sourcePath := path
	if path == "-" {
		sourcePath = ""
	}

	wf := &model.Workflow{
		ProjectID:    getProjectID(cmd),
		Name:         def.Pipeline.Name,
		Version:      def.Pipeline.Version,
		Description:  def.Pipeline.Description,
		SourcePath:   sourcePath,
		SourceSHA256: workflow.SHA256(src),
		Body:         string(src),
		Parsed:       string(parsed),
	}

	stored, created, err := db.InsertWorkflow(conn, wf, model.NowMS())
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
