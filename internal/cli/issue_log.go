package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// logResult is the JSON wire format for the log command output.
type logResult struct {
	IssueID string           `json:"issue_id"`
	Entries []model.Activity `json:"entries"`
	Total   int              `json:"total"`
	// entryTotal is the full activity count BEFORE --limit, and limit the
	// effective limit. Both unexported so the v1 payload is untouched: v1's
	// Total is len(Entries), a post-limit count that hides how many entries
	// were dropped.
	entryTotal int
	limit      int
}

// logResult implements output.Collection for the v2 envelope, reporting the
// honest pre-limit entry count rather than v1's len(Entries).
func (r logResult) CollectionItems() any { return r.Entries }
func (r logResult) CollectionTotal() int { return r.entryTotal }
func (r logResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.entryTotal, len(r.Entries))
}

var logCmd = &cobra.Command{
	Use:   "log [id]",
	Short: "Show activity history for an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueLog)
	},
}

func runIssueLog(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := issueArg(args[0])
	if err != nil {
		return err
	}

	if _, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0])); err != nil {
		return err
	}

	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}
	// v1 silently clamps a non-positive limit to 1. Preserved exactly:
	// validateLimit has already rejected negatives under v2.
	limit = max(limit, 1)

	activity, err := db.GetActivity(conn, id, limit)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
	}

	entryTotal, err := db.CountActivity(conn, id)
	if err != nil {
		return cmdErr(fmt.Errorf("counting activity: %w", err), output.ErrGeneral)
	}

	entries := activity
	if entries == nil {
		entries = []model.Activity{}
	}

	// Total stays len(entries) — the v1 field is frozen. The honest pre-limit
	// count rides in entryTotal and surfaces only under --json=v2.
	result := logResult{
		IssueID:    model.FormatID(id),
		Entries:    entries,
		Total:      len(entries),
		entryTotal: entryTotal,
		limit:      limit,
	}

	if w.JSONMode {
		w.Success(result, "")
		return nil
	}

	if len(activity) == 0 {
		msg := render.EmptyState(
			fmt.Sprintf("No activity for %s", model.FormatID(id)),
			"",
			w.QuietMode,
		)
		w.Success(result, msg)
		return nil
	}

	message := formatActivityLog(model.FormatID(id), activity)
	w.Success(result, message)
	return nil
}

func formatActivityLog(issueID string, activity []model.Activity) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Activity for %s:", issueID))
	lines = append(lines, "")

	useColors := render.ColorsEnabled()

	var timeStyle, fieldStyle lipgloss.Style
	if useColors {
		timeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		fieldStyle = lipgloss.NewStyle().Bold(true)
	}

	// Pre-compute column widths from the data.
	timeW, actorW, fieldW := 0, 0, 0
	type row struct {
		ts, actor, field string
	}
	rows := make([]row, len(activity))
	for i, a := range activity {
		rows[i].ts = humanize.Time(a.CreatedAt)
		rows[i].actor = a.ChangedBy
		if rows[i].actor == "" {
			rows[i].actor = "system"
		}
		switch {
		case a.FieldChanged == "created":
			rows[i].field = "created"
		case a.OldValue != "" && a.NewValue != "":
			rows[i].field = fmt.Sprintf("%-14s %s -> %s", a.FieldChanged, a.OldValue, a.NewValue)
		case a.NewValue != "":
			rows[i].field = fmt.Sprintf("%-14s %q", a.FieldChanged, a.NewValue)
		case a.OldValue != "":
			rows[i].field = fmt.Sprintf("%-14s removed %q", a.FieldChanged, a.OldValue)
		default:
			rows[i].field = a.FieldChanged
		}
		if len(rows[i].ts) > timeW {
			timeW = len(rows[i].ts)
		}
		if len(rows[i].actor) > actorW {
			actorW = len(rows[i].actor)
		}
		if len(rows[i].field) > fieldW {
			fieldW = len(rows[i].field)
		}
	}

	timeFmt := fmt.Sprintf("%%-%ds", timeW)
	actorFmt := fmt.Sprintf("%%-%ds", actorW)
	fieldFmt := fmt.Sprintf("%%-%ds", fieldW)

	for _, r := range rows {
		var line string
		if useColors {
			line = fmt.Sprintf("  %s %s %s",
				timeStyle.Render(fmt.Sprintf(timeFmt, r.ts)),
				fmt.Sprintf(actorFmt, r.actor),
				fieldStyle.Render(fmt.Sprintf(fieldFmt, r.field)),
			)
		} else {
			line = fmt.Sprintf("  "+timeFmt+" "+actorFmt+" "+fieldFmt, r.ts, r.actor, r.field)
		}
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

func init() {
	logCmd.Flags().Int("limit", 20, "Maximum number of entries to show")
	issueCmd.AddCommand(logCmd)
}
