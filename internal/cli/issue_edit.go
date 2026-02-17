package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var editCmd = &cobra.Command{
	Use:   "edit [id]",
	Short: "Edit an existing issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		// Verify issue exists.
		if _, err := db.GetIssue(conn, id); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		updates := make(map[string]interface{})
		filesChanged := false

		if cmd.Flags().Changed("title") {
			title, _ := cmd.Flags().GetString("title")
			updates["title"] = title
		}

		if cmd.Flags().Changed("description") {
			description, _ := cmd.Flags().GetString("description")
			if description == "-" {
				const maxStdinSize = 1 << 20 // 1 MiB
				data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
				if err != nil {
					return cmdErr(fmt.Errorf("reading description from stdin: %w", err), output.ErrGeneral)
				}
				description = strings.TrimRight(string(data), "\n")
			}
			updates["description"] = description
		}

		if cmd.Flags().Changed("status") {
			status, _ := cmd.Flags().GetString("status")
			if err := model.ValidateStatus(model.Status(status)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
			updates["status"] = status
		}

		if cmd.Flags().Changed("priority") {
			priority, _ := cmd.Flags().GetString("priority")
			if err := model.ValidatePriority(model.Priority(priority)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
			updates["priority"] = priority
		}

		if cmd.Flags().Changed("type") {
			kind, _ := cmd.Flags().GetString("type")
			if err := model.ValidateIssueKind(model.IssueKind(kind)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
			updates["kind"] = kind
		}

		if cmd.Flags().Changed("assignee") {
			assignee, _ := cmd.Flags().GetString("assignee")
			updates["assignee"] = assignee
		}

		if cmd.Flags().Changed("file") {
			fileFlag, _ := cmd.Flags().GetStringSlice("file")
			if err := db.SetIssueFiles(conn, id, fileFlag, config.DefaultAuthor()); err != nil {
				return cmdErr(fmt.Errorf("setting files: %w", err), output.ErrGeneral)
			}
			filesChanged = true
		}

		if cmd.Flags().Changed("parent") {
			parent, _ := cmd.Flags().GetString("parent")
			if strings.EqualFold(parent, "0") || strings.EqualFold(parent, "none") {
				updates["parent_id"] = nil
			} else {
				newParentID, err := model.ParseID(parent)
				if err != nil {
					return cmdErr(fmt.Errorf("invalid parent ID: %w", err), output.ErrValidation)
				}
				if newParentID == id {
					return cmdErr(fmt.Errorf("cannot set parent to self"), output.ErrValidation)
				}
				if _, err := db.GetIssue(conn, newParentID); err != nil {
					if errors.Is(err, db.ErrNotFound) {
						return cmdErr(fmt.Errorf("parent issue %s not found", parent), output.ErrNotFound)
					}
					return cmdErr(fmt.Errorf("checking parent issue: %w", err), output.ErrGeneral)
				}
				isCycle, err := db.IsDescendant(conn, id, newParentID)
				if err != nil {
					return cmdErr(fmt.Errorf("checking for cycles: %w", err), output.ErrGeneral)
				}
				if isCycle {
					return cmdErr(fmt.Errorf("cannot reparent: would create a cycle"), output.ErrConflict)
				}
				updates["parent_id"] = newParentID
			}
		}

		if len(updates) == 0 && !filesChanged {
			if w.JSONMode {
				issue, err := db.GetIssue(conn, id)
				if err != nil {
					return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
				}
				w.Success(issue, "")
			} else {
				w.Info("No changes specified")
			}
			return nil
		}

		if len(updates) > 0 {
			if err := db.UpdateIssue(conn, id, updates, config.DefaultAuthor()); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
				}
				return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
			}
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(issue, fmt.Sprintf("Updated %s: %s", model.FormatID(id), issue.Title))

		return nil
	},
}

func init() {
	editCmd.Flags().StringP("title", "t", "", "Issue title")
	editCmd.Flags().StringP("description", "d", "", "Issue description (use \"-\" for stdin)")
	editCmd.Flags().StringP("status", "s", "", "Issue status")
	editCmd.Flags().StringP("priority", "p", "", "Issue priority")
	editCmd.Flags().StringP("type", "T", "", "Issue type")
	editCmd.Flags().StringP("assignee", "a", "", "Issue assignee")
	editCmd.Flags().StringSliceP("file", "f", nil, "File paths (repeatable, replaces existing)")
	editCmd.Flags().String("parent", "", "Parent issue ID (use \"0\" or \"none\" to make root)")
	issueCmd.AddCommand(editCmd)
}
