package main

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type listResult struct {
	Issues []*model.Issue `json:"issues"`
	Total  int            `json:"total"`
}

var listCmd = &cobra.Command{
	Use:     "list",
	Short:   "List issues",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		statuses, _ := cmd.Flags().GetStringSlice("status")
		priorities, _ := cmd.Flags().GetStringSlice("priority")
		labels, _ := cmd.Flags().GetStringSlice("label")
		types, _ := cmd.Flags().GetStringSlice("type")
		assignee, _ := cmd.Flags().GetString("assignee")
		parent, _ := cmd.Flags().GetString("parent")
		rootsOnly, _ := cmd.Flags().GetBool("roots")
		treeMode, _ := cmd.Flags().GetBool("tree")
		sortFlag, _ := cmd.Flags().GetString("sort")
		limit, _ := cmd.Flags().GetInt("limit")
		all, _ := cmd.Flags().GetBool("all")

		// Validate filter enum values.
		for _, s := range statuses {
			if err := model.ValidateStatus(model.Status(s)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}
		for _, p := range priorities {
			if err := model.ValidatePriority(model.Priority(p)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}
		for _, t := range types {
			if err := model.ValidateIssueKind(model.IssueKind(t)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		opts := db.ListOptions{
			Statuses:    statuses,
			Priorities:  priorities,
			Labels:      labels,
			Types:       types,
			Assignee:    assignee,
			RootsOnly:   rootsOnly,
			IncludeDone: all,
			Limit:       limit,
		}

		// Parse --parent flag.
		if parent != "" {
			pid, err := model.ParseID(parent)
			if err != nil {
				return cmdErr(fmt.Errorf("invalid parent ID: %w", err), output.ErrValidation)
			}
			opts.ParentID = &pid
		}

		// Parse --sort flag (field:direction).
		if sortFlag != "" {
			parts := strings.SplitN(sortFlag, ":", 2)
			opts.Sort = parts[0]
			if len(parts) > 1 {
				opts.SortDir = parts[1]
			}
		}

		issues, total, err := db.ListIssues(conn, opts)
		if err != nil {
			return cmdErr(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
		}

		result := listResult{Issues: issues, Total: total}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			message = render.RenderTable(issues, treeMode)
		}
		w.Success(result, message)

		return nil
	},
}

func init() {
	listCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	listCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	listCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	listCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	listCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	listCmd.Flags().String("parent", "", "Filter by parent issue ID")
	listCmd.Flags().Bool("roots", false, "Only show root issues (no parent)")
	listCmd.Flags().Bool("tree", false, "Display as indented hierarchy")
	listCmd.Flags().String("sort", "", "Sort by field:direction (e.g. priority:asc)")
	listCmd.Flags().Int("limit", 50, "Maximum number of results")
	listCmd.Flags().Bool("all", false, "Include done issues")
	rootCmd.AddCommand(listCmd)
}
