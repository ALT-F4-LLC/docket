package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// schemaListResult is a Collection (reliability-delta §4.1), so v2 renders
// {items, total, truncated}. Total comes from a COUNT(*) that ignores LIMIT.
type schemaListResult struct {
	Schemas []*model.Schema `json:"schemas"`
	Total   int             `json:"total"`
	limit   int
}

func (r schemaListResult) CollectionItems() any {
	return schemaListPayload{schemas: r.Schemas}
}
func (r schemaListResult) CollectionTotal() int { return r.Total }
func (r schemaListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Schemas))
}

// schemaListPayload wraps the items so the v2 envelope can add each row's CAS
// version. The envelope consults output.Versioned on the items CONTAINER rather
// than on every element, so a bare slice would render v1 items inside a v2
// envelope — same shape as workflowListPayload, for the same reason.
type schemaListPayload struct{ schemas []*model.Schema }

// MarshalJSON emits the v1 array shape.
func (p schemaListPayload) MarshalJSON() ([]byte, error) {
	if p.schemas == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.schemas)
}

// VersionedPayload implements output.Versioned for a list of schemas.
func (p schemaListPayload) VersionedPayload() any {
	return model.SchemasWithVersion(p.schemas)
}

var schemaListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List registered payload schemas",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaList(cmd, args, getWriter(cmd))
	},
}

func runSchemaList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	name, _ := cmd.Flags().GetString("name")
	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	schemas, total, err := db.ListSchemas(conn, db.SchemaListOptions{ProjectID: getProjectID(cmd), Name: name, Limit: limit})
	if err != nil {
		return cmdErr(fmt.Errorf("listing schemas: %w", err), output.ErrGeneral)
	}

	result := schemaListResult{Schemas: schemas, Total: total, limit: limit}

	var message string
	if !w.JSONMode {
		message = renderSchemaList(schemas)
	}
	w.Success(result, message)
	return nil
}

func renderSchemaList(schemas []*model.Schema) string {
	if len(schemas) == 0 {
		return "No schemas registered."
	}

	var b strings.Builder
	for _, s := range schemas {
		fmt.Fprintf(&b, "%-28s %s", s.Ref(), shortSHA(s.SourceSHA256))
		if s.Builtin {
			b.WriteString("  builtin")
		}
		if fields := s.OrderedFields(); len(fields) > 0 {
			fmt.Fprintf(&b, "  ordered: %s", joinFields(fields))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// joinFields renders an ordered-field list. NAMES ONLY: what a threshold may
// compare is a matter for the schema document, which `schema show --body`
// emits verbatim — a list verb that printed every declared order would be
// unreadable by the third schema.
func joinFields(fields []string) string {
	return strings.Join(fields, ", ")
}

func init() {
	schemaListCmd.Flags().String("name", "", "Filter by schema name")
	schemaListCmd.Flags().Int("limit", 50, "Maximum number of results")
	schemaCmd.AddCommand(schemaListCmd)
}
