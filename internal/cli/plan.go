package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// planPhaseJSON is the JSON wire format for a single execution phase.
type planPhaseJSON struct {
	Phase  int            `json:"phase"`
	Issues []*model.Issue `json:"issues"`
}

// planResult is the JSON wire format for the plan command output.
type planResult struct {
	Phases         []planPhaseJSON `json:"phases"`
	TotalIssues    int             `json:"total_issues"`
	TotalPhases    int             `json:"total_phases"`
	MaxParallelism int             `json:"max_parallelism"`
}

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show execution plan with phased grouping",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		statuses, _ := cmd.Flags().GetStringSlice("status")
		labels, _ := cmd.Flags().GetStringSlice("label")
		rootFlag, _ := cmd.Flags().GetString("root")

		// Validate status filter values.
		for _, s := range statuses {
			if err := model.ValidateStatus(model.Status(s)); err != nil {
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

		// Hydrate file attachments so the planner can detect file collisions.
		if err := db.HydrateFiles(conn, issues); err != nil {
			return cmdErr(fmt.Errorf("hydrating files: %w", err), output.ErrGeneral)
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
			Statuses: statuses,
			Labels:   labels,
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
			phases[i] = planPhaseJSON{
				Phase:  phase.Number,
				Issues: phase.Issues,
			}
		}

		result := planResult{
			Phases:         phases,
			TotalIssues:    plan.TotalIssues,
			TotalPhases:    plan.TotalPhases,
			MaxParallelism: plan.MaxParallelism,
		}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			message = renderPlanHuman(plan, dag)
		}
		w.Success(result, message)

		return nil
	},
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
		if phase.Number == 1 {
			b.WriteString(phaseStyle.Render(fmt.Sprintf("Phase %d (start):", phase.Number)))
		} else {
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

	for _, phase := range plan.Phases {
		b.WriteString("\n")
		if phase.Number == 1 {
			fmt.Fprintf(&b, "Phase %d (start):\n", phase.Number)
		} else {
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
	rootCmd.AddCommand(planCmd)
}
