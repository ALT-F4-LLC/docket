package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

type docLinkResult struct {
	DocID   string `json:"doc_id"`
	IssueID string `json:"issue_id"`
}

var docLinkCmd = &cobra.Command{
	Use:   "link",
	Short: "Manage document links",
}

var docLinkAddCmd = &cobra.Command{
	Use:   "add <id> --issue <issue_id>",
	Short: "Link a document to an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		docID, err := model.ParseDocID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
		}

		issueArg, _ := cmd.Flags().GetString("issue")
		issueID, err := model.ParseID(issueArg)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		if err := db.LinkDocIssue(conn, docID, issueID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("doc or issue not found"), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrConflict) {
				return cmdErr(fmt.Errorf("link already exists"), output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("linking doc to issue: %w", err), output.ErrGeneral)
		}

		result := docLinkResult{
			DocID:   model.FormatDocID(docID),
			IssueID: model.FormatID(issueID),
		}

		w.Success(result, fmt.Sprintf("Linked %s to %s",
			model.FormatDocID(docID), model.FormatID(issueID)))
		return nil
	},
}

var docLinkRemoveCmd = &cobra.Command{
	Use:   "remove <id> --issue <issue_id>",
	Short: "Remove a link between a document and an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		docID, err := model.ParseDocID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
		}

		issueArg, _ := cmd.Flags().GetString("issue")
		issueID, err := model.ParseID(issueArg)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		if err := db.UnlinkDocIssue(conn, docID, issueID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("link not found"), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("unlinking doc from issue: %w", err), output.ErrGeneral)
		}

		result := docLinkResult{
			DocID:   model.FormatDocID(docID),
			IssueID: model.FormatID(issueID),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s from %s",
			model.FormatDocID(docID), model.FormatID(issueID)))
		return nil
	},
}

func init() {
	docLinkAddCmd.Flags().String("issue", "", "Issue ID to link (e.g. DKT-5)")
	_ = docLinkAddCmd.MarkFlagRequired("issue")
	docLinkRemoveCmd.Flags().String("issue", "", "Issue ID to unlink (e.g. DKT-5)")
	_ = docLinkRemoveCmd.MarkFlagRequired("issue")
	docLinkCmd.AddCommand(docLinkAddCmd)
	docLinkCmd.AddCommand(docLinkRemoveCmd)
	docCmd.AddCommand(docLinkCmd)
}
