package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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
	Use:   "link <id> <relation> <target_id>",
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

var unlinkCmd = &cobra.Command{
	Use:   "unlink <id> <relation> <target_id>",
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

var linksCmd = &cobra.Command{
	Use:   "links <id>",
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
			w.Success([]relationDisplay{}, fmt.Sprintf("No relations found for %s", model.FormatID(id)))
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
		fmt.Fprintf(&sb, "Relations for %s:\n", model.FormatID(id))
		for _, d := range displays {
			fmt.Fprintf(&sb, "  %s %s\n", d.RelationType, d.IssueID)
		}

		w.Success(displays, sb.String())
		return nil
	},
}

func init() {
	rootCmd.AddCommand(linkCmd)
	rootCmd.AddCommand(unlinkCmd)
	rootCmd.AddCommand(linksCmd)
}
