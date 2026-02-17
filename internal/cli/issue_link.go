package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
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

var linkCmd = &cobra.Command{
	Use:   "link",
	Short: "Manage issue relations",
}

var linkAddCmd = &cobra.Command{
	Use:   "add <id> <relation> <target_id>",
	Short: "Create a relation between two issues",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		sourceID, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		relType, err := model.ParseRelationType(args[1])
		if err != nil {
			return cmdErr(fmt.Errorf("%w", err), output.ErrValidation)
		}

		targetID, err := model.ParseID(args[2])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid target ID: %w", err), output.ErrValidation)
		}

		rel := &model.Relation{
			SourceIssueID: sourceID,
			TargetIssueID: targetID,
			RelationType:  relType,
		}

		relID, err := db.CreateRelation(conn, rel)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue not found"), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrSelfRelation) {
				return cmdErr(fmt.Errorf("cannot link an issue to itself"), output.ErrValidation)
			}
			if errors.Is(err, db.ErrDuplicateRelation) {
				return cmdErr(fmt.Errorf("relation already exists"), output.ErrConflict)
			}
			if errors.Is(err, db.ErrCycleDetected) {
				return cmdErr(err, output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("creating relation: %w", err), output.ErrGeneral)
		}

		rel.ID = relID

		w.Success(rel, fmt.Sprintf("Linked %s %s %s",
			model.FormatID(sourceID), string(relType), model.FormatID(targetID)))
		return nil
	},
}

var linkRemoveCmd = &cobra.Command{
	Use:   "remove <id> <relation> <target_id>",
	Short: "Remove a relation between two issues",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		sourceID, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		relType, err := model.ParseRelationType(args[1])
		if err != nil {
			return cmdErr(fmt.Errorf("%w", err), output.ErrValidation)
		}

		targetID, err := model.ParseID(args[2])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid target ID: %w", err), output.ErrValidation)
		}

		if err := db.DeleteRelation(conn, sourceID, targetID, string(relType)); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("relation not found"), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("deleting relation: %w", err), output.ErrGeneral)
		}

		result := unlinkResult{
			SourceIssueID: model.FormatID(sourceID),
			TargetIssueID: model.FormatID(targetID),
			RelationType:  string(relType),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s %s %s",
			model.FormatID(sourceID), string(relType), model.FormatID(targetID)))
		return nil
	},
}

var linkListCmd = &cobra.Command{
	Use:   "list <id>",
	Short: "Show all relations for an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		exists, err := db.IssueExists(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("checking issue: %w", err), output.ErrGeneral)
		}
		if !exists {
			return cmdErr(fmt.Errorf("issue not found: %s", model.FormatID(id)), output.ErrNotFound)
		}

		relations, err := db.GetIssueRelations(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
		}

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
