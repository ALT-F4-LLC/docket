package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

var schemaRegisterCmd = &cobra.Command{
	Use:   "register <name@version> <file.json>",
	Short: "Register a payload schema",
	Long: `Read, validate, and register a payload schema document.

The document is compiled as JSON Schema here, at registration, rather than at
first use: a schema that does not compile should be refused while an author is
looking at it, not hours into a run on a step whose work is already done.

` + "`ordered_enum: true`" + ` on a property that also declares ` + "`enum`" + `
declares that the enum array's order, as written, runs from first to last. There
is no second list to disagree with it.

Registering the same bytes at an existing name@version again is a success and
changes nothing. Registering DIFFERENT bytes is a CONFLICT: a schema decides
whether a worker's payload is accepted, so a mutable one would change a run's
acceptance criteria mid-flight. Bump the version.

A registry is PER PROJECT. By default the schema lands in the project the
working directory resolves to; --project registers it into one other project,
and --all-projects registers it into every project in the store. Both report
each project's own outcome, and the frozen-bytes rule is decided per project —
a conflict in one project neither cancels nor hides another project's
registration. Registering schemas store-wide is the step that comes BEFORE
sweeping a workflow corpus, since a workflow's 'payload' references resolve
against the registry of the project it is being registered into.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaRegister(cmd, args, getWriter(cmd))
	},
}

func runSchemaRegister(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	ref, path := args[0], args[1]

	// `name@version` is an ARGUMENT, not two flags — §1's surface line is
	// `docket schema register name@v schema.json`. The grammar is
	// internal/workflow's own PayloadShape, shared rather than restated, so what
	// a workflow may REFERENCE and what the registry ACCEPTS cannot drift.
	name, version, err := workflow.ParsePayloadRef(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cmdErr(fmt.Errorf("schema file %s not found", path), output.ErrNotFound)
		}
		return cmdErr(
			fmt.Errorf("reading schema file %s: %w", path, err), output.ErrNotFound)
	}

	// Compile checks the ordered_enum annotation rules and then compiles the
	// document as ordinary JSON Schema. Both refusals name the property path.
	registered, err := schema.Compile(name, version, src)
	if err != nil {
		return schemaErr(fmt.Errorf("%s: %w", path, err))
	}

	// The derived index is stored NEXT TO the bytes it came from, so nothing
	// re-walks a schema document during evaluation. It is a cache of a pure
	// function of those bytes, never a second source of truth.
	ordered, err := json.Marshal(registered.Ordered)
	if err != nil {
		return cmdErr(fmt.Errorf("encoding the ordered index: %w", err), output.ErrGeneral)
	}

	// The DOCUMENT is judged before the targets are: bytes that do not compile
	// are wrong in every project, and naming a project in that refusal would
	// suggest the document might have been right somewhere else.
	targets, fannedOut, err := resolveRegistryTargets(cmd, conn)
	if err != nil {
		return err
	}
	if fannedOut {
		return registerSchemaAcrossProjects(
			cmd, w, conn, name, version, path, src, string(ordered), targets)
	}

	stored, created, err := db.InsertSchema(
		conn, schemaRow(name, version, path, src, string(ordered), getProjectID(cmd)),
		model.NowMS())
	if err != nil {
		return schemaErr(err)
	}

	message := fmt.Sprintf("Registered %s", stored.Ref())
	if fields := stored.OrderedFields(); len(fields) > 0 {
		message += fmt.Sprintf(" (ordered: %s)", joinFields(fields))
	}
	if !created {
		message = fmt.Sprintf("%s is already registered with these bytes", stored.Ref())
	}
	w.Success(stored, message)
	return nil
}

// schemaRow builds the row to insert. Shared by the single-project path and the
// fan-out so the two can never store different columns for the same bytes.
func schemaRow(
	name string, version int, path string, src []byte, ordered string, projectID int,
) *model.Schema {
	return &model.Schema{
		// The registration lands in a NAMED project's registry (DKT-20):
		// without this the insert falls through to the column's DEFAULT 1, so
		// every project's registrations pile up under whichever repo holds the
		// default row — invisible to the very project that registered them.
		ProjectID: projectID,
		Name:      name,
		Version:   version,
		// source_path is provenance only — it records where the bytes came from
		// and is never re-read.
		SourcePath:   path,
		SourceSHA256: workflow.SHA256(src),
		Body:         string(src),
		Ordered:      ordered,
	}
}

// registerSchemaAcrossProjects is the --project / --all-projects path.
//
// A schema has no environment to validate against — the document compiled
// once, above, and compiling is a pure function of the bytes — so the loop is
// insert-only. A conflicting project is recorded and the sweep continues, for
// the reason the flag exists: one command is supposed to cover the store, and
// stopping at the first conflict would leave the operator to work out by hand
// which projects it had reached.
func registerSchemaAcrossProjects(
	cmd *cobra.Command, w *output.Writer, conn *sql.DB,
	name string, version int, path string, src []byte, ordered string,
	targets []*model.Project,
) error {
	report := &registryFanoutReport{
		Operation: "schema register",
		Subject:   fmt.Sprintf("%s@%d", name, version),
		Scope:     fanoutScope(cmd),
	}

	for _, target := range targets {
		stored, created, err := db.InsertSchema(
			conn, schemaRow(name, version, path, src, ordered, target.ID), model.NowMS())
		if err != nil {
			report.Results = append(report.Results,
				registryFailureResult(target, err, schemaErr))
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

func init() {
	addRegistryTargetFlags(schemaRegisterCmd, "Register")
	schemaCmd.AddCommand(schemaRegisterCmd)
}
