package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Use:         "add <id> --issue <issue_id>",
	Short:       "Link a document to an issue",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		issueFlag, _ := cmd.Flags().GetString("issue")

		env := verbEnv(cmd)
		res, err := verb.DocLinkAdd(cmd.Context(), env, verb.DocLinkAddInput{
			Doc:   args[0],
			Issue: issueFlag,
		})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		result := docLinkResult{
			DocID:   model.FormatDocID(res.DocID),
			IssueID: model.FormatID(res.IssueID),
		}

		w.Success(result, fmt.Sprintf("Linked %s to %s", result.DocID, result.IssueID))
		return nil
	},
}

var docLinkRemoveCmd = &cobra.Command{
	Use:         "remove <id> --issue <issue_id>",
	Short:       "Remove a link between a document and an issue",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		issueFlag, _ := cmd.Flags().GetString("issue")

		env := verbEnv(cmd)
		res, err := verb.DocLinkRemove(cmd.Context(), env, verb.DocLinkRemoveInput{
			Doc:   args[0],
			Issue: issueFlag,
		})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		result := docLinkResult{
			DocID:   model.FormatDocID(res.DocID),
			IssueID: model.FormatID(res.IssueID),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s from %s", result.DocID, result.IssueID))
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
