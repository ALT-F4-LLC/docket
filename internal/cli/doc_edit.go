package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var docEditCmd = &cobra.Command{
	Use:         "edit <id>",
	Short:       "Edit an existing document",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		in := verb.DocEditInput{Doc: args[0]}

		if cmd.Flags().Changed("title") {
			title, _ := cmd.Flags().GetString("title")
			in.Title = &title
		}

		if cmd.Flags().Changed("type") {
			docType, _ := cmd.Flags().GetString("type")
			in.Type = &docType
		}

		if cmd.Flags().Changed("status") {
			status, _ := cmd.Flags().GetString("status")
			in.Status = &status
		}

		if cmd.Flags().Changed("description") {
			body, _ := cmd.Flags().GetString("description")
			loadedBody, err := loadDocBody(body)
			if err != nil {
				return err
			}
			in.Body = &loadedBody
		}

		env := verbEnv(cmd)
		res, err := verb.DocEdit(cmd.Context(), env, in)
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		doc := res.Doc
		if !res.Changed {
			if w.JSONMode {
				w.Success(doc, "")
			} else {
				w.Info("No changes specified")
			}
			return nil
		}

		w.Success(doc, fmt.Sprintf("Updated %s: %s", model.FormatDocID(doc.ID), doc.Title))

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
