package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type docDeleteResult struct {
	ID string `json:"id"`
}

// newDocDeleteCmd builds the command fresh, per newImportCmd's pattern: tests
// drive an instance carrying the same flag registration a real invocation
// parses.
func newDocDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := getWriter(cmd)
			conn := getDB(cmd)

			cascade, _ := cmd.Flags().GetBool("cascade")
			force, _ := cmd.Flags().GetBool("force")

			id, err := model.ParseDocID(args[0])
			if err != nil {
				return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
			}

			doc, err := db.GetDoc(conn, id)
			if err != nil {
				if e := notFound(err, fmt.Sprintf("doc %s", model.FormatDocID(id))); e != nil {
					return e
				}
				return cmdErr(fmt.Errorf("fetching doc: %w", err), output.ErrGeneral)
			}

			if !force {
				// An output-format flag must never double as consent for a
				// destructive verb (DKT-15's rule; see events_prune.go's P6
				// comment): without --force, JSON mode and non-interactive human
				// mode both refuse, and only a real terminal may confirm.
				if w.JSONMode || !term.IsTerminal(int(os.Stdin.Fd())) {
					return cmdErr(fmt.Errorf("deleting %s is destructive; pass --force to confirm", model.FormatDocID(id)), output.ErrValidation)
				}
				var confirmed bool
				form := huh.NewForm(
					huh.NewGroup(
						huh.NewConfirm().
							Title(fmt.Sprintf("Delete %s: %s?", model.FormatDocID(id), doc.Title)).
							Value(&confirmed),
					),
				)
				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						w.Info("Cancelled.")
						return nil
					}
					return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
				}
				if !confirmed {
					w.Info("Cancelled.")
					return nil
				}
			}

			if err := db.DeleteDoc(conn, id, cascade); err != nil {
				if e := notFound(err, fmt.Sprintf("doc %s", model.FormatDocID(id))); e != nil {
					return e
				}
				if errors.Is(err, db.ErrConflict) {
					return cmdErr(err, output.ErrConflict)
				}
				return cmdErr(fmt.Errorf("deleting doc: %w", err), output.ErrGeneral)
			}

			w.Success(docDeleteResult{ID: model.FormatDocID(id)}, fmt.Sprintf("Deleted %s: %s", model.FormatDocID(id), doc.Title))

			return nil
		},
	}
	cmd.Flags().Bool("cascade", false, "Also remove this document's issue/proposal links (the issues and proposals themselves are not deleted)")
	cmd.Flags().BoolP("force", "f", false, "Confirm the deletion (required in JSON and non-interactive mode; skips the terminal confirmation)")
	return cmd
}

var docDeleteCmd = newDocDeleteCmd()

func init() {
	docCmd.AddCommand(docDeleteCmd)
}
