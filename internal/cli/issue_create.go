package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new issue",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		title, _ := cmd.Flags().GetString("title")
		description, _ := cmd.Flags().GetString("description")
		status, _ := cmd.Flags().GetString("status")
		priority, _ := cmd.Flags().GetString("priority")
		kind, _ := cmd.Flags().GetString("type")
		labelFlag, _ := cmd.Flags().GetStringSlice("label")
		fileFlag, _ := cmd.Flags().GetStringSlice("file")
		assignee, _ := cmd.Flags().GetString("assignee")
		parent, _ := cmd.Flags().GetString("parent")
		jsonMode, _ := jsonModeOf(cmd)

		// If JSON mode and no title, return validation error.
		if jsonMode && title == "" {
			return cmdErr(fmt.Errorf("--title is required in JSON mode"), output.ErrValidation)
		}

		// If no title and not JSON mode, launch interactive form.
		// The status, priority, and kind variables already hold their flag
		// defaults ("backlog", "none", "task"). Passing them via .Value(...)
		// ensures the select widgets pre-select the matching default.
		if !jsonMode && title == "" {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmdErr(fmt.Errorf("non-interactive environment detected; provide all required flags: --title"), output.ErrValidation)
			}
			var labelStr string
			var fileStr string
			form := huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title("Title").
						Value(&title).
						Validate(func(s string) error {
							if strings.TrimSpace(s) == "" {
								return fmt.Errorf("title is required")
							}
							return nil
						}),
					huh.NewText().
						Title("Description").
						Value(&description),
					huh.NewSelect[string]().
						Title("Status").
						Options(
							huh.NewOption("backlog", "backlog"),
							huh.NewOption("todo", "todo"),
							huh.NewOption("in-progress", "in-progress"),
							huh.NewOption("review", "review"),
							huh.NewOption("done", "done"),
						).
						Value(&status), // pre-selects flag default ("backlog")
					huh.NewSelect[string]().
						Title("Priority").
						Options(
							huh.NewOption("none", "none"),
							huh.NewOption("low", "low"),
							huh.NewOption("medium", "medium"),
							huh.NewOption("high", "high"),
							huh.NewOption("critical", "critical"),
						).
						Value(&priority), // pre-selects flag default ("none")
					huh.NewSelect[string]().
						Title("Type").
						Options(
							huh.NewOption("task", "task"),
							huh.NewOption("bug", "bug"),
							huh.NewOption("feature", "feature"),
							huh.NewOption("epic", "epic"),
							huh.NewOption("chore", "chore"),
						).
						Value(&kind), // pre-selects flag default ("task")
					huh.NewInput().
						Title("Assignee").
						Value(&assignee),
					huh.NewInput().
						Title("Labels (comma-separated)").
						Value(&labelStr),
					huh.NewInput().
						Title("Files (comma-separated)").
						Value(&fileStr),
				),
			)

			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					w.Info("Cancelled.")
					return nil
				}
				return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
			}

			if labelStr != "" {
				for _, l := range strings.Split(labelStr, ",") {
					l = strings.TrimSpace(l)
					if l != "" {
						labelFlag = append(labelFlag, l)
					}
				}
			}

			if fileStr != "" {
				for _, f := range strings.Split(fileStr, ",") {
					f = strings.TrimSpace(f)
					if f != "" {
						fileFlag = append(fileFlag, f)
					}
				}
			}
		}

		// Read description from stdin if "-".
		if description == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading description from stdin: %w", err), output.ErrGeneral)
			}
			description = strings.TrimRight(string(data), "\n")
		}

		// Validate enum values.
		if err := model.ValidateStatus(model.Status(status)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		if err := model.ValidatePriority(model.Priority(priority)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		if err := model.ValidateIssueKind(model.IssueKind(kind)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		// Handle parent ID. The parent must exist AND live in the project this
		// issue is being created in (DKT-22) — resolveParentIssue enforces both.
		var parentID *int
		if parent != "" {
			parentIssue, err := resolveParentIssue(conn, parent, getProjectID(cmd))
			if err != nil {
				return err
			}
			parentID = &parentIssue.ID
		}

		issue := model.Issue{
			ProjectID:   getProjectID(cmd),
			ParentID:    parentID,
			Title:       title,
			Description: description,
			Status:      model.Status(status),
			Priority:    model.Priority(priority),
			Kind:        model.IssueKind(kind),
			Assignee:    assignee,
		}

		idempotencyKey, err := idempotencyKeyOf(cmd)
		if err != nil {
			return err
		}

		id, err := db.CreateIssueIdempotent(conn, &issue, labelFlag, fileFlag, idempotencyKey)
		if err != nil {
			return cmdErr(fmt.Errorf("creating issue: %w", err), output.ErrGeneral)
		}

		// Scope is written after the insert rather than through CreateIssue,
		// so the create path an unmodified `issue create` takes is unchanged:
		// a repo that never declares a scope executes exactly the v6 code and
		// leaves scope_globs NULL (§3 phase-2 dormancy).
		if err := applyScope(cmd, conn, id); err != nil {
			return err
		}

		// Refetch to get full object with timestamps.
		created, err := db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching created issue: %w", err), output.ErrGeneral)
		}

		// Echo back what was actually linked, not a bare issues row (DKT-240).
		if err := hydrateIssueAssociations(conn, created); err != nil {
			return err
		}

		w.Success(withIssueVersion(created), fmt.Sprintf("Created %s: %s", model.FormatID(id), created.Title))

		return nil
	},
}

func init() {
	createCmd.Flags().StringP("title", "t", "", "Issue title")
	createCmd.Flags().StringP("description", "d", "", "Issue description (use \"-\" for stdin)")
	createCmd.Flags().StringP("status", "s", "backlog", "Issue status")
	createCmd.Flags().StringP("priority", "p", "none", "Issue priority")
	createCmd.Flags().StringP("type", "T", "task", "Issue type")
	createCmd.Flags().StringSliceP("label", "l", nil, "Issue labels (repeatable)")
	createCmd.Flags().StringSliceP("file", "f", nil, "File paths (repeatable)")
	createCmd.Flags().StringP("assignee", "a", "", "Issue assignee")
	createCmd.Flags().String("parent", "", "Parent issue ID")
	addScopeFlag(createCmd)
	addIdempotencyKeyFlag(createCmd)
	issueCmd.AddCommand(createCmd)
}
