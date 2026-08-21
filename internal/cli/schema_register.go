package cli

import (
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
acceptance criteria mid-flight. Bump the version.`,
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

	stored, created, err := db.InsertSchema(conn, &model.Schema{
		// The registration lands in the INVOKING project's registry (DKT-20):
		// without this the insert falls through to the column's DEFAULT 1, so
		// every project's registrations pile up under whichever repo holds the
		// default row — invisible to the very project that registered them.
		ProjectID: getProjectID(cmd),
		Name:      name,
		Version:   version,
		// source_path is provenance only — it records where the bytes came from
		// and is never re-read.
		SourcePath:   path,
		SourceSHA256: workflow.SHA256(src),
		Body:         string(src),
		Ordered:      string(ordered),
	}, model.NowMS())
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

func init() {
	schemaCmd.AddCommand(schemaRegisterCmd)
}
