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

// nextResult is `next`'s issue-mode payload. `Issues` is typed `any` for the
// same reason listResult's is: summary rows (issueRowsPayload) by default,
// the full issue shape (issueListPayload) under `--with-body` (DKT-1053; see
// issue_row.go).
type nextResult struct {
	Issues any `json:"issues"`
	Total  int `json:"total"`
	// readyTotal is the size of the ready set BEFORE --limit truncated it, and
	// limit the effective limit. Both are unexported so the v1 payload is
	// untouched: v1's Total is len(Issues) — a post-limit count that cannot
	// distinguish "exactly N ready" from "N returned, many more dropped".
	// That is the silent drop the v2 envelope exists to close.
	readyTotal int
	limit      int
	// count is the number of rows in Issues, kept alongside because Issues is
	// `any`.
	count int
}

// nextResult implements output.Collection for the v2 envelope, reporting the
// honest pre-limit total rather than v1's len(Issues).
func (r nextResult) CollectionItems() any { return r.Issues }
func (r nextResult) CollectionTotal() int { return r.readyTotal }
func (r nextResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.readyTotal, r.count)
}

var nextCmd = &cobra.Command{
	Use:   "next",
	Short: "Show work-ready issues",
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runNext)
	},
}

// runNext dispatches between the two modes of `next` (TDD §6.3.1).
//
// `docket next` WITHOUT --run is the existing issue-mode verb, BYTE-IDENTICAL:
// runNextIssues below is the pre-phase-3 body, MOVED and not edited, so the
// diff on that code is a pure relocation a reviewer can verify at a glance.
// That is a code-structure requirement rather than an intention — it is what
// makes QA section X (test_x_next.sh) pass untouched, which is the strongest
// available evidence that behavior did not move.
func runNext(cmd *cobra.Command, args []string, w *output.Writer) error {
	if runRef, _ := cmd.Flags().GetString("run"); runRef != "" {
		return runNextSteps(cmd, runRef, w)
	}
	return runNextIssues(cmd, args, w)
}

func runNextIssues(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	statuses, _ := cmd.Flags().GetStringSlice("status")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	labels, _ := cmd.Flags().GetStringSlice("label")
	types, _ := cmd.Flags().GetStringSlice("type")
	limit, _ := cmd.Flags().GetInt("limit")
	withBody, _ := cmd.Flags().GetBool("with-body")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

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
		ProjectID:   getProjectID(cmd),
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

	// Capture the true size of the ready set before truncating, so the v2
	// envelope can report it and flag the drop.
	readyTotal := len(ready)

	// Apply limit.
	if limit > 0 && len(ready) > limit {
		ready = ready[:limit]
	}

	if err := db.HydrateDocs(conn, ready); err != nil {
		return cmdErr(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}

	// FindReady/filterReady return a nil slice when nothing is ready. Both row
	// payloads marshal a nil slice as `[]`, but the human table is handed
	// `ready` directly, so it is normalized here once for every consumer.
	if ready == nil {
		ready = []*model.Issue{}
	}

	// Total stays len(ready) — the v1 field is frozen. The honest pre-limit
	// count rides in readyTotal and surfaces only under --json=v2.
	result := nextResult{
		Issues:     issuesPayload(ready, withBody),
		Total:      len(ready),
		readyTotal: readyTotal,
		limit:      limit,
		count:      len(ready),
	}

	var message string
	if !w.JSONMode {
		message = render.RenderTable(ready, false)
		// Name the OTHER mode. Bare `next` is the issue-mode verb and its
		// output is frozen, but a conductor who wanted a run's ready STEPS
		// reads this issue list as a broken ready-set rather than as a
		// different question answered correctly.
		//
		// A hint on the human channel only: the JSON payload is a wire format
		// and must not grow prose. Errors are wrong here too — bare `next` is
		// a supported verb with its own QA section, not a mistake.
		message += nextModeHint
	}
	w.Success(result, message)

	return nil
}

// nextModeHint disambiguates the two modes of `next` for a human reader.
const nextModeHint = "\nShowing work-ready ISSUES. " +
	"For a run's ready steps, use `docket next --run RUN-N`.\n"

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
	nextCmd.Flags().Int("limit", 10, "Maximum number of results (issue mode; with --run the full ready set is returned unless --limit is passed)")
	// --run switches `next` into STEP mode (TDD §6.3.1). Without it the verb
	// is exactly what it was: a workflow-free repo never passes this flag and
	// never leaves the issue-mode path.
	nextCmd.Flags().String("run", "", "Show ready STEPS of this run instead of ready issues")
	nextCmd.Flags().Bool("with-body", false, withBodyHelp)
	rootCmd.AddCommand(nextCmd)
}
