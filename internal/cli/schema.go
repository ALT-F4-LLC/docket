package cli

import (
	"errors"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/spf13/cobra"
)

var schemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Register and inspect payload schemas",
	Long: `Register and inspect payload schemas.

A schema is JSON Schema plus one annotation: ` + "`ordered_enum`" + `. Marking a
property's ` + "`enum`" + ` as ordered declares that its values run from first to
last, and that declaration is what makes ordered threshold comparisons —
` + "`any(risk >= medium)`" + ` — mean something. Docket learns the ORDER from the
document; it never learns what the values mean.

A registered name@version is frozen, exactly as a workflow is: re-registering
the same bytes is a success that changes nothing, and re-registering different
bytes is a CONFLICT. A run pins the schema its payloads are validated against,
so the bytes cannot be swapped underneath it.`,
}

func init() {
	rootCmd.AddCommand(schemaCmd)
}

// schemaErr maps a registry failure to its error code, per §4.5's refusal
// matrix. No new codes: every situation maps to one that already exists.
func schemaErr(err error) error {
	// A schema-DOCUMENT refusal — the ordered_enum annotation rules, or a
	// document the library will not compile. Payload refusals never reach here:
	// they arise inside the saga and are mapped by the engine's own taxonomy.
	var se *schema.Error
	if errors.As(err, &se) {
		return cmdErr(err, output.ErrValidation)
	}
	switch {
	case errors.Is(err, db.ErrSchemaConflict):
		return cmdErr(err, output.ErrConflict)
	case errors.Is(err, db.ErrSchemaNotFound):
		return cmdErr(err, output.ErrNotFound)
	default:
		return cmdErr(err, output.ErrGeneral)
	}
}
