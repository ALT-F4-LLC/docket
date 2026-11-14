package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// listResult is `issue list`'s payload. `Issues` holds SUMMARY rows
// (issueRowsPayload) by default and the full issue shape (issueListPayload)
// under `--with-body`, which is why it is typed `any`: the two shapes differ,
// and the default one is what keeps a filtered listing small enough to read
// (DKT-1053; see issue_row.go). Both payloads marshal the v1 array and
// implement output.Versioned, so the v2 envelope reaches each row's version
// either way.
type listResult struct {
	Issues any `json:"issues"`
	Total  int `json:"total"`
	// limit is the effective --limit, retained (unexported, so v1 output is
	// untouched) to compute truncation for the v2 envelope.
	limit int
	// count is the number of rows in Issues, kept alongside because Issues is
	// `any`.
	count int
}

// listResult implements output.Collection for the v2 envelope. Total comes
// from a COUNT(*) that ignores LIMIT, so it is already the true pre-limit
// count and truncation is directly computable.
func (r listResult) CollectionItems() any { return r.Issues }
func (r listResult) CollectionTotal() int { return r.Total }
func (r listResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, r.count)
}

var listCmd = &cobra.Command{
	Use:     "list",
	Short:   "List issues",
	Aliases: []string{"ls"},
	Long: `List issues as summary rows.

Under --json each row carries every issue field except description — id
(and its alias issue), parent_id, title, status, priority, kind, assignee,
labels, files, docs, created_at, updated_at, and scope, resolution, and size
when set — plus description_bytes, the length of the description it does not
carry. A listing is for picking; read the issue you picked with
docket issue show. Pass --with-body to have every row carry its full
description instead. next, plan and board emit the same rows and take the
same flag.`,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueList)
	},
}

func runIssueList(cmd *cobra.Command, args []string, w *output.Writer) error {
	statuses, _ := cmd.Flags().GetStringSlice("status")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	sizes, _ := cmd.Flags().GetStringSlice("size")
	labels, _ := cmd.Flags().GetStringSlice("label")
	types, _ := cmd.Flags().GetStringSlice("type")
	assignee, _ := cmd.Flags().GetString("assignee")
	parent, _ := cmd.Flags().GetString("parent")
	rootsOnly, _ := cmd.Flags().GetBool("roots")
	treeMode, _ := cmd.Flags().GetBool("tree")
	sortFlag, _ := cmd.Flags().GetString("sort")
	limit, _ := cmd.Flags().GetInt("limit")
	all, _ := cmd.Flags().GetBool("all")
	runRef, _ := cmd.Flags().GetString("run")
	projectRef, _ := cmd.Flags().GetString("project")
	withBody, _ := cmd.Flags().GetBool("with-body")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	// `--project` scopes the listing to a NAMED project instead of the one the
	// working directory resolves to (DKT-72), and `--run` to ONE RUN'S ROSTER
	// (DKT-405), done issues included.
	listed, err := verb.IssueList(cmd.Context(), verbEnv(cmd), verb.IssueListInput{
		Statuses:    statuses,
		Priorities:  priorities,
		Sizes:       sizes,
		Labels:      labels,
		Types:       types,
		Assignee:    assignee,
		Parent:      parent,
		RootsOnly:   rootsOnly,
		All:         all,
		Sort:        sortFlag,
		Limit:       limit,
		Run:         runRef,
		Project:     projectRef,
		WithParents: !w.JSONMode,
	})
	if err != nil {
		return verbErr(err)
	}
	// Ids render in the listed project's voice, not the caller's: the prefix is
	// the only thing on the row that says whose issue it is (DKT-67).
	if listed.Project != nil {
		model.SetDisplayPrefix(listed.Project.Prefix)
	}
	issues := listed.Issues

	result := listResult{
		Issues: issuesPayload(issues, withBody),
		Total:  listed.Total,
		limit:  limit,
		count:  len(issues),
	}

	var message string
	if !w.JSONMode {
		switch {
		case len(issues) == 0:
			message = emptyListMessage(listed.RunID, listed.HiddenDone)
		case treeMode:
			message = render.RenderTable(issues, true)
		default:
			message = render.RenderGroupedTable(issues, listed.Parents, subIssueProgress(listed.Progress))
		}
	}
	w.Success(result, message)

	return nil
}

// subIssueProgress is the grouped table's progress for each parent that has
// sub-issues.
func subIssueProgress(counts map[int][2]int) map[int]render.SubIssueProgress {
	if counts == nil {
		return nil
	}
	progress := make(map[int]render.SubIssueProgress, len(counts))
	for id, c := range counts {
		if c[1] > 0 {
			progress[id] = render.SubIssueProgress{Done: c[0], Total: c[1]}
		}
	}
	return progress
}

// emptyListState is emptyListMessage for a listing with opts that came back
// empty, counting the done issues it hid from st.
func emptyListState(st store.Store, opts store.ListOptions, all bool) string {
	hidden := 0
	if opts.RunID == 0 && !all {
		withDone := opts
		withDone.IncludeDone = true
		withDone.Limit = 1
		if _, total, err := st.ListIssues(withDone); err == nil {
			hidden = total
		}
	}
	return emptyListMessage(opts.RunID, hidden)
}

// emptyListMessage answers the question an empty listing actually raises.
//
// The blanket "No issues found. Create one with: docket issue create" is a
// claim about the store, and on a project with dozens of done issues it is a
// false one: the default listing hides done, so the only thing proven is that
// nothing is OPEN. Read as store damage, it cost a --all round trip to
// disprove (DKT-246). When done issues are being hidden, say so and name the
// flag that shows them.
func emptyListMessage(runID, hiddenDone int) string {
	// A roster that came back empty is a fact about the RUN, not about the
	// store, and "create one with: docket issue create" is advice for a
	// different situation entirely (DKT-405).
	if runID != 0 {
		return render.EmptyState(
			fmt.Sprintf("%s has no issues bound to it.", model.FormatRunID(runID)),
			"Attach one with: docket run issue add", false)
	}
	if hiddenDone > 0 {
		return render.EmptyState(
			fmt.Sprintf("No open issues found (%s hidden).", pluralIssues(hiddenDone)),
			"Include them with: docket issue list --all",
			false)
	}
	return render.EmptyState("No issues found.", "Create one with: docket issue create", false)
}

func pluralIssues(n int) string {
	if n == 1 {
		return "1 done issue"
	}
	return fmt.Sprintf("%d done issues", n)
}

func init() {
	listCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	listCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	listCmd.Flags().StringSlice("size", nil, "Filter by size (repeatable)")
	listCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	listCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	listCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	listCmd.Flags().String("parent", "", "Filter by parent issue ID")
	listCmd.Flags().Bool("roots", false, "Only show root issues (no parent)")
	listCmd.Flags().Bool("tree", false, "Display as indented hierarchy")
	listCmd.Flags().String("sort", "", "Sort by field:direction (e.g. priority:asc)")
	listCmd.Flags().String(
		"project", "",
		"List another project's issues, by prefix (FLX), name, identity path, "+
			"or row id (default: the project this directory belongs to)")
	listCmd.Flags().String(
		"run", "",
		"List the issues bound to a run, by ref (RUN-14) — the run's whole "+
			"roster, done ones included")
	listCmd.Flags().Int("limit", 50, "Maximum number of results")
	listCmd.Flags().Bool("all", false, "Include done issues")
	listCmd.Flags().Bool("with-body", false, withBodyHelp)
	issueCmd.AddCommand(listCmd)
}
