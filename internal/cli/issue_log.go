package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// logResult is the JSON wire format for the log command output.
type logResult struct {
	IssueID string           `json:"issue_id"`
	Entries []model.Activity `json:"entries"`
	Total   int              `json:"total"`
}

var logCmd = &cobra.Command{
	Use:   "log [id]",
	Short: "Show activity history for an issue",
	Args:  cobra.ExactArgs(1),
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
				return runIssueLog(cmd, args, w)
			})
		}
		return runIssueLog(cmd, args, getWriter(cmd))
	},
}

func runIssueLog(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := model.ParseID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
	}

	if _, err := db.GetIssue(conn, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
		}
		return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
	}

	limit, _ := cmd.Flags().GetInt("limit")
	limit = max(limit, 1)

	activity, err := db.GetActivity(conn, id, limit)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
	}

	entries := activity
	if entries == nil {
		entries = []model.Activity{}
	}

	result := logResult{
		IssueID: model.FormatID(id),
		Entries: entries,
		Total:   len(entries),
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
