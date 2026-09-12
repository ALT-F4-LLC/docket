package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
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
labels, files, docs, created_at, updated_at, and scope and resolution when
set — plus description_bytes, the length of the description it does not
carry. A listing is for picking; read the issue you picked with
docket issue show. Pass --with-body to have every row carry its full
description instead. next, plan and board emit the same rows and take the
same flag.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueList)
	},
}

func runIssueList(cmd *cobra.Command, args []string, w *output.Writer) error {
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
	runRef, _ := cmd.Flags().GetString("run")
	projectRef, _ := cmd.Flags().GetString("project")
	withBody, _ := cmd.Flags().GetBool("with-body")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	// Refused before either scope is resolved: --project moves the process-wide
	// display prefix, and a conflict discovered after that has already changed
	// how the failing command's neighbours would render.
	if runRef != "" && projectRef != "" {
		return cmdErr(fmt.Errorf(
			"--run and --project cannot be combined: a run belongs to one "+
				"project and --run already lists it"), output.ErrValidation)
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

	// `--project` scopes the listing to a NAMED project instead of the one the
	// working directory resolves to (DKT-72). Under a machine-global store
	// that is the flag three separate sessions reached for unprompted, and
	// without it reading another project's issues means changing directory
	// into it — which for a project you only want to LOOK at is a strange
	// price, and impossible for one whose checkout is not on this machine.
	projectID := getProjectID(cmd)
	if projectRef != "" {
		project, err := resolveProjectRef(conn, projectRef)
		if err != nil {
			return err
		}
		projectID = project.ID
		// Ids render in the NAMED project's voice, not the caller's: a listing
		// of another project's issues under this project's prefix is the same
		// defect DKT-67 fixed in the event feed, and for the same reason — the
		// prefix is the only thing on the row that says whose issue it is.
		model.SetDisplayPrefix(project.Prefix)
	}

	// `--run` scopes the listing to ONE RUN'S ROSTER (DKT-405): the issues that
	// run's activation bound. It is the spelling a conductor reaches for first
	// — `docket issue list --run RUN-14` — and before this it answered `unknown
	// flag: --run`, leaving `run status --json` as the only surface carrying a
	// roster at all, which is a JSON document to be parsed rather than a
	// listing to be read.
	runID := 0
	if runRef != "" {
		id, err := model.ParseRunID(runRef)
		if err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		run, err := db.GetRun(conn, id)
		if errors.Is(err, db.ErrRunNotFound) {
			return cmdErr(fmt.Errorf("run %s not found", model.FormatRunID(id)),
				output.ErrNotFound)
		}
		if err != nil {
			return cmdErr(fmt.Errorf("reading run: %w", err), output.ErrGeneral)
		}
		runID = run.ID
		// The RUN's project, in the run's own voice — the `--project` rule
		// (DKT-67/DKT-72) applied to a scope the caller named indirectly. A
		// roster listed under the working directory's prefix would be labelled
		// with a project that does not own the issues, and under a machine-global
		// store the run being read is routinely another project's.
		if run.ProjectID != 0 && run.ProjectID != projectID {
			project, err := db.GetProject(conn, run.ProjectID)
			if err != nil {
				return cmdErr(fmt.Errorf("reading the run's project: %w", err),
					output.ErrGeneral)
			}
			projectID = project.ID
			model.SetDisplayPrefix(project.Prefix)
		}
		// A ROSTER IS A CLOSED SET, so `--run` shows all of it. The default
		// listing hides done issues, and a post-mortem roster — the case this
		// flag exists for — is mostly done issues; hiding them would answer the
		// question "which issues did RUN-14 carry" with "the ones it did not
		// finish", which is a different question and a misleading answer.
		all = true
	}

	opts := db.ListOptions{
		ProjectID:   projectID,
		RunID:       runID,
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

	if err := db.HydrateDocs(conn, issues); err != nil {
		return cmdErr(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}

	result := listResult{
		Issues: issuesPayload(issues, withBody),
		Total:  total,
		limit:  limit,
		count:  len(issues),
	}

	// Fetch parent issues and sub-issue progress for the grouped display.
	// Only needed for human-readable output (JSON stays flat).
	var parentMap map[int]*model.Issue
	var progress map[int]render.SubIssueProgress
	if !w.JSONMode {
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

	var message string
	if !w.JSONMode {
		switch {
		case len(issues) == 0:
			message = emptyListState(conn, opts, all)
		case treeMode:
			message = render.RenderTable(issues, true)
		default:
			message = render.RenderGroupedTable(issues, parentMap, progress)
		}
	}
	w.Success(result, message)

	return nil
}

// emptyListState answers the question an empty listing actually raises.
//
// The blanket "No issues found. Create one with: docket issue create" is a
// claim about the store, and on a project with dozens of done issues it is a
// false one: the default listing hides done, so the only thing proven is that
// nothing is OPEN. Read as store damage, it cost a --all round trip to
// disprove (DKT-246). When done issues are being hidden, say so and name the
// flag that shows them.
func emptyListState(conn *sql.DB, opts db.ListOptions, all bool) string {
	// A roster that came back empty is a fact about the RUN, not about the
	// store, and "create one with: docket issue create" is advice for a
	// different situation entirely (DKT-405).
	if opts.RunID != 0 {
		return render.EmptyState(
			fmt.Sprintf("%s has no issues bound to it.",
				model.FormatRunID(opts.RunID)),
			"Attach one with: docket run issue add", false)
	}
	if !all {
		withDone := opts
		withDone.IncludeDone = true
		withDone.Limit = 1
		if _, total, err := db.ListIssues(conn, withDone); err == nil && total > 0 {
			return render.EmptyState(
				fmt.Sprintf("No open issues found (%s hidden).", pluralIssues(total)),
				"Include them with: docket issue list --all",
				false)
		}
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
