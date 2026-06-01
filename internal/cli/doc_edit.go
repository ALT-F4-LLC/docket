package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var docEditCmd = &cobra.Command{
	Use:   "edit <id>",
	Short: "Edit an existing document",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseDocID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
		}

		if _, err := db.GetDoc(conn, id); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("doc %s not found", model.FormatDocID(id)), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching doc: %w", err), output.ErrGeneral)
		}

		upd := db.DocUpdate{Author: config.DefaultAuthor()}

		if cmd.Flags().Changed("title") {
			title, _ := cmd.Flags().GetString("title")
			upd.Title = &title
		}

		if cmd.Flags().Changed("type") {
			docType, _ := cmd.Flags().GetString("type")
			upd.Type = &docType
		}

		if cmd.Flags().Changed("status") {
			status, _ := cmd.Flags().GetString("status")
			upd.Status = &status
		}

		if cmd.Flags().Changed("description") {
			body, _ := cmd.Flags().GetString("description")
			loadedBody, err := loadDocBody(body)
			if err != nil {
				return err
			}
			upd.Body = &loadedBody
		}

		if upd.Title == nil && upd.Type == nil && upd.Status == nil && upd.Body == nil {
			doc, err := db.GetDoc(conn, id)
			if err != nil {
				return cmdErr(fmt.Errorf("fetching doc: %w", err), output.ErrGeneral)
			}
			if w.JSONMode {
				w.Success(doc, "")
			} else {
				w.Info("No changes specified")
			}
			return nil
		}

		rev, err := db.UpdateDoc(conn, id, upd)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("doc %s not found", model.FormatDocID(id)), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("updating doc: %w", err), output.ErrGeneral)
		}

		doc, err := db.GetDoc(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated doc: %w", err), output.ErrGeneral)
		}

		if rev == 0 {
			if w.JSONMode {
				w.Success(doc, "")
			} else {
				w.Info("No changes specified")
			}
			return nil
		}

		w.Success(doc, fmt.Sprintf("Updated %s: %s", model.FormatDocID(id), doc.Title))

		return nil
	},
}

func init() {
	docEditCmd.Flags().StringP("title", "t", "", "Document title")
	docEditCmd.Flags().StringP("description", "d", "", "Document body (use \"@path\" for a file or \"-\" for stdin)")
	docEditCmd.Flags().StringP("type", "T", "", "Document type")
	docEditCmd.Flags().StringP("status", "s", "", "Document status")
	docCmd.AddCommand(docEditCmd)
}
