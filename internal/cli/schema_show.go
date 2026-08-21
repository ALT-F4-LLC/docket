package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var schemaShowCmd = &cobra.Command{
	Use:   "show <name>[@<version>]",
	Short: "Show a registered payload schema",
	Long: `Show a registered payload schema.

Omitting @version selects the highest registered version. --body emits the
stored document verbatim — the exact bytes that were registered and hashed, and
the exact bytes a run validates its payloads against.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaShow(cmd, args, getWriter(cmd))
	},
}

func runSchemaShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	// The reference grammar is `workflow show`'s: a bare name means "the highest
	// registered version", which is the read verb's convenience and never what a
	// `payload` declaration or a pin may mean.
	name, version, err := model.ParseWorkflowRef(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	s, err := db.GetSchema(conn, getProjectID(cmd), name, version)
	if err != nil {
		return schemaErr(describeMissingSchema(err, args[0]))
	}

	body, _ := cmd.Flags().GetBool("body")
	if body {
		// The stored document, verbatim. In JSON mode it rides in the envelope
		// as a string so the output stays a single parseable document; in human
		// mode it is printed as-is, which is what makes
		// `docket schema show findings@1 --body > findings.json` round-trip.
		if w.JSONMode {
			w.Success(map[string]string{"name": s.Name, "body": s.Body}, "")
			return nil
		}
		w.Success(nil, strings.TrimRight(s.Body, "\n"))
		return nil
	}

	var message string
	if !w.JSONMode {
		message = renderSchemaShow(s)
	}
	w.Success(s, message)
	return nil
}

// describeMissingSchema turns the storage sentinel into an operator-facing
// message naming what was asked for.
func describeMissingSchema(err error, ref string) error {
	if err == db.ErrSchemaNotFound {
		return fmt.Errorf("%w: %s is not registered", db.ErrSchemaNotFound, ref)
	}
	return err
}

func renderSchemaShow(s *model.Schema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", s.Ref())
	fmt.Fprintf(&b, "sha256: %s\n", s.SourceSHA256)
	if s.SourcePath != "" {
		fmt.Fprintf(&b, "source: %s\n", s.SourcePath)
	}
	if s.Builtin {
		b.WriteString("builtin: shipped with docket\n")
	}

	fields := s.OrderedFields()
	if len(fields) == 0 {
		b.WriteString("\nno ordered fields — only `==` and `!=` are defined over this payload\n")
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "\nordered fields (%d): %s\n", len(fields), joinFields(fields))
	b.WriteString("`--body` prints the declared order, which is each field's `enum` array as written.\n")
	return strings.TrimRight(b.String(), "\n")
}

func init() {
	schemaShowCmd.Flags().Bool("body", false, "Emit the stored schema document verbatim")
	schemaCmd.AddCommand(schemaShowCmd)
}
