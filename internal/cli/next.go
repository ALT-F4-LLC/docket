package cli

import (
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Use:         "next",
	Short:       "Show work-ready issues",
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runNext)
	},
}

// runNext dispatches between the two modes of `next`.
//
// `docket next` WITHOUT --run is the issue-mode verb, and its output is
// frozen: QA section X (test_x_next.sh) pins it byte for byte.
func runNext(cmd *cobra.Command, args []string, w *output.Writer) error {
	if runRef, _ := cmd.Flags().GetString("run"); runRef != "" {
		return runNextSteps(cmd, runRef, w)
	}
	return runNextIssues(cmd, args, w)
}

func runNextIssues(cmd *cobra.Command, args []string, w *output.Writer) error {
	statuses, _ := cmd.Flags().GetStringSlice("status")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	sizes, _ := cmd.Flags().GetStringSlice("size")
	labels, _ := cmd.Flags().GetStringSlice("label")
	types, _ := cmd.Flags().GetStringSlice("type")
	limit, _ := cmd.Flags().GetInt("limit")
	withBody, _ := cmd.Flags().GetBool("with-body")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	next, err := verb.Next(cmd.Context(), verbEnv(cmd), verb.NextInput{
		Statuses:   statuses,
		Priorities: priorities,
		Sizes:      sizes,
		Labels:     labels,
		Types:      types,
		Limit:      limit,
	})
	if err != nil {
		return verbErr(err)
	}
	ready := next.Issues

	// Total stays len(ready) — the v1 field is frozen. The honest pre-limit
	// count rides in readyTotal and surfaces only under --json=v2.
	result := nextResult{
		Issues:     issuesPayload(ready, withBody),
		Total:      len(ready),
		readyTotal: next.Total,
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

func init() {
	nextCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (default: backlog,todo)")
	nextCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	nextCmd.Flags().StringSlice("size", nil, "Filter by size (repeatable)")
	nextCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	nextCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	nextCmd.Flags().Int("limit", 10, "Maximum number of results (issue mode; with --run the full ready set is returned unless --limit is passed)")
	// --run switches `next` into STEP mode. Without it the verb
	// is exactly what it was: a workflow-free repo never passes this flag and
	// never leaves the issue-mode path.
	nextCmd.Flags().String("run", "", "Show ready STEPS of this run instead of ready issues")
	nextCmd.Flags().Bool("with-body", false, withBodyHelp)
	rootCmd.AddCommand(nextCmd)
}
