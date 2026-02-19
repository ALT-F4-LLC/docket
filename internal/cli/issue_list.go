package cli

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

		// Fetch parent issues and sub-issue progress for the grouped display.
		// Only needed for human-readable output (JSON stays flat).
		var parentMap map[int]*model.Issue
		var progress map[int]render.SubIssueProgress
		if !jsonMode {
			// Build a set of issue IDs in the result set for quick lookup.
			resultIDs := make(map[int]struct{}, len(issues))
			for _, issue := range issues {
				resultIDs[issue.ID] = struct{}{}
			}

			// Collect parent IDs that are referenced but not in the result set.
			missingParentIDs := make(map[int]struct{})
			for _, issue := range issues {
				if issue.ParentID != nil {
					pid := *issue.ParentID
					if _, inResult := resultIDs[pid]; !inResult {
						missingParentIDs[pid] = struct{}{}
					}
				}
			}

			// Batch-fetch missing parents if any exist.
			if len(missingParentIDs) > 0 {
				ids := make([]int, 0, len(missingParentIDs))
				for id := range missingParentIDs {
					ids = append(ids, id)
				}
				parentMap, err = db.GetIssuesByIDs(conn, ids)
				if err != nil {
					return cmdErr(fmt.Errorf("fetching parent issues: %w", err), output.ErrGeneral)
				}
			}

			// Collect IDs of all parent issues that have children in the
			// result set. This includes parents fetched into parentMap
			// (excluded by filters) and parents already in the result set.
			parentIDSet := make(map[int]struct{})
			for id := range parentMap {
				parentIDSet[id] = struct{}{}
			}
			for _, issue := range issues {
				if issue.ParentID != nil {
					pid := *issue.ParentID
					if _, inResult := resultIDs[pid]; inResult {
						parentIDSet[pid] = struct{}{}
					} else if _, inMap := parentMap[pid]; inMap {
						parentIDSet[pid] = struct{}{}
					}
				}
			}

			// Fetch sub-issue progress (done/total counts) for parent
			// issues in a single batch query.
			if len(parentIDSet) > 0 {
				parentIDs := make([]int, 0, len(parentIDSet))
				for id := range parentIDSet {
					parentIDs = append(parentIDs, id)
				}
				batchProgress, err := db.GetBatchSubIssueProgress(conn, parentIDs)
				if err != nil {
					return cmdErr(fmt.Errorf("fetching sub-issue progress: %w", err), output.ErrGeneral)
				}
				progress = make(map[int]render.SubIssueProgress, len(batchProgress))
				for id, counts := range batchProgress {
					if counts[1] > 0 {
						progress[id] = render.SubIssueProgress{Done: counts[0], Total: counts[1]}
					}
				}
			}
		}

		_ = parentMap // will be consumed by grouped renderer in a subsequent change
		_ = progress  // will be consumed by grouped renderer in a subsequent change

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
	issueCmd.AddCommand(listCmd)
}
