package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/filter"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type nextResult struct {
	Issues []*model.Issue `json:"issues"`
	Total  int            `json:"total"`
}

var nextCmd = &cobra.Command{
	Use:   "next",
	Short: "Show work-ready issues",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		statuses, _ := cmd.Flags().GetStringSlice("status")
		priorities, _ := cmd.Flags().GetStringSlice("priority")
		labels, _ := cmd.Flags().GetStringSlice("label")
		types, _ := cmd.Flags().GetStringSlice("type")
		limit, _ := cmd.Flags().GetInt("limit")

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

		// Fetch all non-done issues for DAG construction.
		allIssues, _, err := db.ListIssues(conn, db.ListOptions{
			IncludeDone: false,
			Limit:       0, // no limit — need all for DAG
		})
		if err != nil {
			return cmdErr(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
		}

		// Fetch all directional relations (blocks / depends_on).
		relations, err := db.GetAllDirectionalRelations(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("loading relations: %w", err), output.ErrGeneral)
		}

		// Build DAG and find work-ready issues.
		dag := planner.BuildDAG(allIssues, relations)

		// Default statuses for FindReady: backlog, todo.
		readyStatuses := statuses
		if len(readyStatuses) == 0 {
			readyStatuses = []string{string(model.StatusBacklog), string(model.StatusTodo)}
		}
		ready := planner.FindReady(dag, readyStatuses)

		// Apply additional filters (priority, label, type) on the ready set.
		ready = filterReady(ready, priorities, labels, types)

		// Apply limit.
		if limit > 0 && len(ready) > limit {
			ready = ready[:limit]
		}

		result := nextResult{Issues: ready, Total: len(ready)}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			message = render.RenderTable(ready, false)
		}
		w.Success(result, message)

		return nil
	},
}

// filterReady applies priority, label, and type filters to a slice of ready issues.
func filterReady(issues []*model.Issue, priorities, labels, types []string) []*model.Issue {
	if len(priorities) == 0 && len(labels) == 0 && len(types) == 0 {
		return issues
	}

	prioritySet := filter.ToStringSet(priorities)
	labelSet := filter.ToStringSet(labels)
	typeSet := filter.ToStringSet(types)

	var filtered []*model.Issue
	for _, issue := range issues {
		if len(prioritySet) > 0 {
			if _, ok := prioritySet[string(issue.Priority)]; !ok {
				continue
			}
		}
		if len(typeSet) > 0 {
			if _, ok := typeSet[string(issue.Kind)]; !ok {
				continue
			}
		}
		if len(labelSet) > 0 && !filter.HasAllLabels(issue, labelSet) {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

func init() {
	nextCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (default: backlog,todo)")
	nextCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	nextCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	nextCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	nextCmd.Flags().Int("limit", 10, "Maximum number of results")
	rootCmd.AddCommand(nextCmd)
}
