package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
)

// planPhaseJSON is the JSON wire format for a single execution phase.
type planPhaseJSON struct {
	Phase  int         `json:"phase"`
	Level  int         `json:"level"`
	Issues []planIssue `json:"issues"`
}

// planIssue pairs an issue with its blocker IDs for plan JSON output.
type planIssue struct {
	Issue     *model.Issue
	BlockedBy []string
}

// planIssueJSON is the explicit JSON wire format for planIssue, avoiding the
// fragile marshal-unmarshal-remarshal pattern (mirrors showResultJSON).
type planIssueJSON struct {
	ID          string         `json:"id"`
	ParentID    *string        `json:"parent_id,omitempty"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Status      string         `json:"status"`
	Priority    string         `json:"priority"`
	Kind        string         `json:"kind"`
	Assignee    string         `json:"assignee"`
	Labels      []string       `json:"labels"`
	Files       []string       `json:"files"`
	Docs        []model.DocRef `json:"docs"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	BlockedBy   []string       `json:"blocked_by"`
}

// MarshalJSON implements custom JSON serialization for planIssue.
func (p planIssue) MarshalJSON() ([]byte, error) {
	i := p.Issue

	labels := i.Labels
	if labels == nil {
		labels = []string{}
	}
	files := i.Files
	if files == nil {
		files = []string{}
	}
	docs := i.Docs
	if docs == nil {
		docs = []model.DocRef{}
	}
	blockedBy := p.BlockedBy
	if blockedBy == nil {
		blockedBy = []string{}
	}

	j := planIssueJSON{
		ID:          model.FormatID(i.ID),
		Title:       i.Title,
		Description: i.Description,
		Status:      string(i.Status),
		Priority:    string(i.Priority),
		Kind:        string(i.Kind),
		Assignee:    i.Assignee,
		Labels:      labels,
		Files:       files,
		Docs:        docs,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   i.UpdatedAt.UTC().Format(time.RFC3339),
		BlockedBy:   blockedBy,
	}

	if i.ParentID != nil {
		pid := model.FormatID(*i.ParentID)
		j.ParentID = &pid
	}

	return json.Marshal(j)
}

// planResult is the JSON wire format for the plan command output.
type planResult struct {
	Phases         []planPhaseJSON `json:"phases"`
	TotalIssues    int             `json:"total_issues"`
	TotalPhases    int             `json:"total_phases"`
	TotalLevels    int             `json:"total_levels"`
	MaxParallelism int             `json:"max_parallelism"`
}

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show execution plan with phased grouping",
	RunE: func(cmd *cobra.Command, args []string) error {
		watchMode, _ := cmd.Flags().GetBool("watch")
		if watchMode {
			interval, _ := cmd.Flags().GetDuration("interval")
			jsonMode, _ := cmd.Flags().GetBool("json")
			quietMode, _ := cmd.Flags().GetBool("quiet")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return watch.RunWatch(ctx, watch.Options{
				Interval:  interval,
				JSONMode:  jsonMode,
				QuietMode: quietMode,
				IsTTY:     term.IsTerminal(int(os.Stdout.Fd())),
				Stdout:    os.Stdout,
				Stderr:    os.Stderr,
			}, func(ctx context.Context, w *output.Writer) error {
				return runPlan(cmd, args, w)
			})
		}
		return runPlan(cmd, args, getWriter(cmd))
	},
}

func runPlan(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	statuses, _ := cmd.Flags().GetStringSlice("status")
	labels, _ := cmd.Flags().GetStringSlice("label")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	types, _ := cmd.Flags().GetStringSlice("type")
	assignee, _ := cmd.Flags().GetString("assignee")
	rootFlag, _ := cmd.Flags().GetString("root")

	// Validate status filter values.
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

	// Fetch all non-done issues.
	issues, _, err := db.ListIssues(conn, db.ListOptions{
		IncludeDone: false,
		Limit:       0,
	})
	if err != nil {
		return cmdErr(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
	}

	if err := db.HydrateDocs(conn, issues); err != nil {
		return cmdErr(fmt.Errorf("hydrating docs: %w", err), output.ErrGeneral)
	}

	// Fetch all directional relations.
	relations, err := db.GetAllDirectionalRelations(conn)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}

	// Build the DAG.
	dag := planner.BuildDAG(issues, relations)

	// Build plan filters.
	filters := planner.PlanFilters{
		Statuses:   statuses,
		Labels:     labels,
		Priorities: priorities,
		Types:      types,
		Assignee:   assignee,
	}

	// Parse --root flag.
	if rootFlag != "" {
		rootID, err := model.ParseID(rootFlag)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid root ID: %w", err), output.ErrValidation)
		}
		filters.RootID = &rootID
	}

	// Generate the plan (includes cycle detection via TopoSort).
	plan, err := planner.GeneratePlan(dag, filters)
	if err != nil {
		var cycleErr *planner.CycleError
		if errors.As(err, &cycleErr) {
			return cmdErr(err, output.ErrConflict)
		}
		return cmdErr(fmt.Errorf("generating plan: %w", err), output.ErrGeneral)
	}

	// Build JSON result.
	phases := make([]planPhaseJSON, len(plan.Phases))
	for i, phase := range plan.Phases {
		planIssues := make([]planIssue, len(phase.Issues))
		for j, issue := range phase.Issues {
			planIssues[j] = planIssue{
				Issue:     issue,
				BlockedBy: collectDeps(issue.ID, dag),
			}
		}
		phases[i] = planPhaseJSON{
			Phase:  phase.Number,
			Level:  phase.Level,
			Issues: planIssues,
		}
	}

	result := planResult{
		Phases:         phases,
		TotalIssues:    plan.TotalIssues,
		TotalPhases:    plan.TotalPhases,
		TotalLevels:    plan.TotalLevels,
		MaxParallelism: plan.MaxParallelism,
	}

	var message string
	if !w.JSONMode {
		message = renderPlanHuman(plan, dag)
	}
	w.Success(result, message)

	return nil
}

// renderPlanHuman renders the execution plan as human-readable text.
func renderPlanHuman(plan *planner.Plan, dag *planner.DAG) string {
	if plan.TotalIssues == 0 {
		return render.EmptyState("No issues to plan.", "Create issues first with: docket issue create", false)
	}

	if !render.ColorsEnabled() {
		return renderPlanPlain(plan, dag)
	}

	var b strings.Builder

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	phaseStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	idStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	titleStyle := lipgloss.NewStyle().Bold(true)
	depStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	separatorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	boldMetric := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	b.WriteString(headerStyle.Render("Execution Plan:"))
	b.WriteString("\n")

	for i, phase := range plan.Phases {
		if i > 0 {
			b.WriteString(separatorStyle.Render("  ────────────────────────────────"))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		switch {
		case phase.Number == 1:
			b.WriteString(phaseStyle.Render(fmt.Sprintf("Phase %d (start):", phase.Number)))
		case phase.Level == plan.Phases[i-1].Level:
			b.WriteString(phaseStyle.Render(fmt.Sprintf("Phase %d (same dependency level as Phase %d, split by file collision):", phase.Number, phase.Number-1)))
		default:
			b.WriteString(phaseStyle.Render(fmt.Sprintf("Phase %d (parallel, after Phase %d):", phase.Number, phase.Number-1)))
		}
		b.WriteString("\n")

		for _, issue := range phase.Issues {
			priStyle := lipgloss.NewStyle().Foreground(render.ColorFromName(issue.Priority.Color()))
			statusIcon := lipgloss.NewStyle().Foreground(render.ColorFromName(issue.Status.Color())).Render(issue.Status.Icon())
			kindIcon := lipgloss.NewStyle().Foreground(render.ColorFromName(issue.Kind.Color())).Render(issue.Kind.Icon())

			deps := collectDeps(issue.ID, dag)
			if len(deps) > 0 {
				fmt.Fprintf(&b, "  %s %s %s %s %s  %s\n",
					statusIcon,
					kindIcon,
					idStyle.Render(fmt.Sprintf("%-6s", model.FormatID(issue.ID))),
					priStyle.Render(fmt.Sprintf("[%-8s]", string(issue.Priority))),
					titleStyle.Render(issue.Title),
					depStyle.Render(fmt.Sprintf("(depends on %s)", strings.Join(deps, ", "))),
				)
			} else {
				fmt.Fprintf(&b, "  %s %s %s %s %s\n",
					statusIcon,
					kindIcon,
					idStyle.Render(fmt.Sprintf("%-6s", model.FormatID(issue.ID))),
					priStyle.Render(fmt.Sprintf("[%-8s]", string(issue.Priority))),
					titleStyle.Render(issue.Title),
				)
			}
		}
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "Summary: %s issues, %s phases, max parallelism: %s",
		boldMetric.Render(fmt.Sprintf("%d", plan.TotalIssues)),
		boldMetric.Render(fmt.Sprintf("%d", plan.TotalPhases)),
		boldMetric.Render(fmt.Sprintf("%d", plan.MaxParallelism)),
	)

	return b.String()
}

// renderPlanPlain renders the execution plan without colors.
func renderPlanPlain(plan *planner.Plan, dag *planner.DAG) string {
	var b strings.Builder

	b.WriteString("Execution Plan:\n")

	for i, phase := range plan.Phases {
		b.WriteString("\n")
		switch {
		case phase.Number == 1:
			fmt.Fprintf(&b, "Phase %d (start):\n", phase.Number)
		case phase.Level == plan.Phases[i-1].Level:
			fmt.Fprintf(&b, "Phase %d (same dependency level as Phase %d, split by file collision):\n", phase.Number, phase.Number-1)
		default:
			fmt.Fprintf(&b, "Phase %d (parallel, after Phase %d):\n", phase.Number, phase.Number-1)
		}

		for _, issue := range phase.Issues {
			deps := collectDeps(issue.ID, dag)
			if len(deps) > 0 {
				fmt.Fprintf(&b, "  %-6s [%-8s] %s  (depends on %s)\n",
					model.FormatID(issue.ID),
					string(issue.Priority),
					issue.Title,
					strings.Join(deps, ", "),
				)
			} else {
				fmt.Fprintf(&b, "  %-6s [%-8s] %s\n",
					model.FormatID(issue.ID),
					string(issue.Priority),
					issue.Title,
				)
			}
		}
	}

	fmt.Fprintf(&b, "\nSummary: %d issues, %d phases, max parallelism: %d",
		plan.TotalIssues, plan.TotalPhases, plan.MaxParallelism)

	return b.String()
}

// collectDeps returns formatted IDs of issues that block the given issue.
func collectDeps(issueID int, dag *planner.DAG) []string {
	node, ok := dag.Nodes[issueID]
	if !ok {
		return nil
	}
	if len(node.Reverse) == 0 {
		return nil
	}
	deps := make([]string, 0, len(node.Reverse))
	for blockerID := range node.Reverse {
		deps = append(deps, model.FormatID(blockerID))
	}
	// Sort for deterministic output.
	sortDeps(deps)
	return deps
}

// sortDeps sorts formatted IDs (e.g. "DKT-7", "DKT-12") by their numeric value.
func sortDeps(deps []string) {
	sort.Slice(deps, func(i, j int) bool {
		a, errA := model.ParseID(deps[i])
		b, errB := model.ParseID(deps[j])
		if errA != nil || errB != nil {
			return deps[i] < deps[j]
		}
		return a < b
	})
}

func init() {
	planCmd.Flags().String("root", "", "Scope to a parent issue and its descendants")
	planCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable; default: backlog, todo, in-progress)")
	planCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	planCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	planCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	planCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	rootCmd.AddCommand(planCmd)
}
