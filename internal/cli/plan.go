package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	// withBody selects the full pre-DKT-1053 row (planIssueJSON) over the
	// summary row (planIssueRowJSON). Unexported: it is a rendering choice,
	// never a wire field.
	withBody bool
}

// planIssueRowJSON is `plan`'s default row: the summary row every
// list-shaped verb emits (issue_row.go), plus `blocked_by` (DKT-1053).
//
// Sharing issueRow means a plan row now also carries the keys the shared row
// has and planIssueJSON never gained — `issue` (DKT-452), and `scope` and
// `resolution` when set (DKT-55, DKT-245). Those are additions a consumer of
// the existing keys cannot notice; `description` is the only key that moved.
type planIssueRowJSON struct {
	issueRow
	BlockedBy []string `json:"blocked_by"`
}

// planIssueJSON is the explicit JSON wire format for a `--with-body` plan
// row, avoiding the fragile marshal-unmarshal-remarshal pattern (mirrors
// showResultJSON). It is the shape `plan` emitted before DKT-1053, kept
// byte-identical for the caller that wants every description in one call.
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

// MarshalJSON implements custom JSON serialization for planIssue: the summary
// row by default, the full row under --with-body.
func (p planIssue) MarshalJSON() ([]byte, error) {
	i := p.Issue

	blockedBy := p.BlockedBy
	if blockedBy == nil {
		blockedBy = []string{}
	}

	if !p.withBody {
		return json.Marshal(planIssueRowJSON{
			issueRow:  summarizeIssue(i),
			BlockedBy: blockedBy,
		})
	}

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
	Use:         "plan",
	Short:       "Show execution plan with phased grouping",
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runPlan)
	},
}

func runPlan(cmd *cobra.Command, args []string, w *output.Writer) error {
	statuses, _ := cmd.Flags().GetStringSlice("status")
	labels, _ := cmd.Flags().GetStringSlice("label")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	sizes, _ := cmd.Flags().GetStringSlice("size")
	types, _ := cmd.Flags().GetStringSlice("type")
	assignee, _ := cmd.Flags().GetString("assignee")
	rootFlag, _ := cmd.Flags().GetString("root")
	withBody, _ := cmd.Flags().GetBool("with-body")

	planned, err := verb.Plan(cmd.Context(), verbEnv(cmd), verb.PlanInput{
		Statuses:   statuses,
		Labels:     labels,
		Priorities: priorities,
		Sizes:      sizes,
		Types:      types,
		Assignee:   assignee,
		Root:       rootFlag,
	})
	if err != nil {
		return verbErr(err)
	}
	plan, blockedBy := planned.Plan, planned.BlockedBy

	// Build JSON result.
	phases := make([]planPhaseJSON, len(plan.Phases))
	for i, phase := range plan.Phases {
		planIssues := make([]planIssue, len(phase.Issues))
		for j, issue := range phase.Issues {
			planIssues[j] = planIssue{
				Issue:     issue,
				BlockedBy: blockedBy[issue.ID],
				withBody:  withBody,
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
		message = renderPlanHuman(plan, blockedBy)
	}
	w.Success(result, message)

	return nil
}

// renderPlanHuman renders the execution plan as human-readable text.
// blockedBy maps an issue's id to the formatted ids of its blockers.
func renderPlanHuman(plan *planner.Plan, blockedBy map[int][]string) string {
	if plan.TotalIssues == 0 {
		return render.EmptyState("No issues to plan.", "Create issues first with: docket issue create", false)
	}

	if !render.ColorsEnabled() {
		return renderPlanPlain(plan, blockedBy)
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

			deps := blockedBy[issue.ID]
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
func renderPlanPlain(plan *planner.Plan, blockedBy map[int][]string) string {
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
			deps := blockedBy[issue.ID]
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

func init() {
	planCmd.Flags().String("root", "", "Scope to a parent issue and its descendants")
	planCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable; default: every non-done status)")
	planCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	planCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	planCmd.Flags().StringSlice("size", nil, "Filter by size (repeatable)")
	planCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	planCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	planCmd.Flags().Bool("with-body", false, withBodyHelp)
	rootCmd.AddCommand(planCmd)
}
