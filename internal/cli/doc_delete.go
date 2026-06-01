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

var docDeleteCmd = &cobra.Command{
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
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("doc %s not found", model.FormatDocID(id)), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching doc: %w", err), output.ErrGeneral)
		}

		if !force && !w.JSONMode {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmdErr(fmt.Errorf("non-interactive environment detected; pass --force to delete %s or use --json", model.FormatDocID(id)), output.ErrValidation)
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
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("doc %s not found", model.FormatDocID(id)), output.ErrNotFound)
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

func init() {
	docDeleteCmd.Flags().Bool("cascade", false, "Also remove this document's issue/proposal links (the issues and proposals themselves are not deleted)")
	docDeleteCmd.Flags().BoolP("force", "f", false, "Skip the interactive confirmation prompt")
	docCmd.AddCommand(docDeleteCmd)
}
