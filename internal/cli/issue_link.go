package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// relationDisplay is the JSON-friendly structure returned by the links command.
type relationDisplay struct {
	ID           int    `json:"id"`
	RelationType string `json:"relation_type"`
	IssueID      string `json:"issue_id"`
	Direction    string `json:"direction"`
}

// unlinkResult is the JSON-friendly structure returned by the unlink command.
type unlinkResult struct {
	SourceIssueID string `json:"source_issue_id"`
	TargetIssueID string `json:"target_issue_id"`
	RelationType  string `json:"relation_type"`
}

// parseRelationTypeArg parses the <relation> argument typed at `link add` /
// `link remove`. It is model.ParseRelationType plus one CLI-only courtesy:
// "related_to" / "related-to" — the natural mis-typing of "relates_to", and the
// normalized form the refusal message already echoes back — resolves to the
// canonical type (DKT-1073). The tolerance stops at the command line on
// purpose: the JSON wire format (model.Relation.UnmarshalJSON) and the workflow
// `issue.linked.<relation>.<kind>` vocabulary stay exactly the canonical token
// set (DKT-1077).
func parseRelationTypeArg(arg string) (model.RelationType, error) {
	if strings.ReplaceAll(strings.TrimSpace(arg), "-", "_") == "related_to" {
		return model.RelationRelatesTo, nil
	}
	return model.ParseRelationType(arg)
}

var linkCmd = &cobra.Command{
	Use:   "link",
	Short: "Manage issue relations",
}

// relationArg is the <relation> argument with parseRelationTypeArg's courtesy
// applied: a spelling it accepts becomes the canonical type, and any other
// passes through for the verb to refuse in its own argument order.
func relationArg(arg string) string {
	if relType, err := parseRelationTypeArg(arg); err == nil {
		return string(relType)
	}
	return arg
}

var linkAddCmd = &cobra.Command{
	Use:   "add <id> <relation> <target_id>",
	Short: "Create a relation between two issues",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		res, err := verb.IssueLinkAdd(cmd.Context(), verbEnv(cmd), verb.IssueLinkAddInput{
			Issue:    args[0],
			Relation: relationArg(args[1]),
			Target:   args[2],
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		rel := res.Relation
		w.Success(rel, fmt.Sprintf("Linked %s %s %s",
			model.FormatID(rel.SourceIssueID), string(rel.RelationType), model.FormatID(rel.TargetIssueID)))
		return nil
	},
}

var linkRemoveCmd = &cobra.Command{
	Use:   "remove <id> <relation> <target_id>",
	Short: "Remove a relation between two issues",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		res, err := verb.IssueLinkRemove(cmd.Context(), verbEnv(cmd), verb.IssueLinkRemoveInput{
			Issue:    args[0],
			Relation: relationArg(args[1]),
			Target:   args[2],
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		rel := res.Relation
		result := unlinkResult{
			SourceIssueID: model.FormatID(rel.SourceIssueID),
			TargetIssueID: model.FormatID(rel.TargetIssueID),
			RelationType:  string(rel.RelationType),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s %s %s",
			result.SourceIssueID, result.RelationType, result.TargetIssueID))
		return nil
	},
}

var linkListCmd = &cobra.Command{
	Use:   "list <id>",
	Short: "Show all relations for an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		listed, err := verb.IssueLinkList(cmd.Context(), verbEnv(cmd), verb.IssueLinkListInput{Issue: args[0]})
		if err != nil {
			return verbErr(err)
		}
		id, relations := listed.IssueID, listed.Relations

		if len(relations) == 0 {
			quiet, _ := cmd.Flags().GetBool("quiet")
			msg := render.EmptyState(
				fmt.Sprintf("No relations found for %s", model.FormatID(id)),
				fmt.Sprintf("Add one with: docket issue link add %s <relation> <target>", model.FormatID(id)),
				quiet,
			)
			w.Success([]relationDisplay{}, msg)
			return nil
		}

		var displays []relationDisplay
		for _, rel := range relations {
			var d relationDisplay
			d.ID = rel.ID
			if rel.SourceIssueID == id {
				d.RelationType = string(rel.RelationType)
				d.IssueID = model.FormatID(rel.TargetIssueID)
				d.Direction = "outgoing"
			} else {
				d.RelationType = rel.RelationType.Inverse()
				d.IssueID = model.FormatID(rel.SourceIssueID)
				d.Direction = "incoming"
			}
			displays = append(displays, d)
		}

		if w.JSONMode {
			w.Success(displays, "")
			return nil
		}

		var sb strings.Builder
		if render.ColorsEnabled() {
			sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
			boldStyle := lipgloss.NewStyle().Bold(true)
			fmt.Fprintf(&sb, "%s\n", sectionStyle.Render(fmt.Sprintf("Relations for %s", model.FormatID(id))))
			for _, d := range displays {
				relType := model.RelationType(d.RelationType)
				typeStyle := lipgloss.NewStyle().Foreground(render.ColorFromName(render.RelationColor(relType)))
				var arrow string
				if d.Direction == "outgoing" {
					arrow = render.RelationArrow(relType, true)
				} else {
					arrow = render.RelationArrow(relType, false)
				}
				fmt.Fprintf(&sb, "  %s %s %s\n", arrow, typeStyle.Render(d.RelationType), boldStyle.Render(d.IssueID))
			}
		} else {
			fmt.Fprintf(&sb, "Relations for %s:\n", model.FormatID(id))
			for _, d := range displays {
				fmt.Fprintf(&sb, "  %s %s\n", d.RelationType, d.IssueID)
			}
		}

		w.Success(displays, sb.String())
		return nil
	},
}

func init() {
	linkCmd.AddCommand(linkAddCmd)
	linkCmd.AddCommand(linkRemoveCmd)
	linkCmd.AddCommand(linkListCmd)
	issueCmd.AddCommand(linkCmd)
}
