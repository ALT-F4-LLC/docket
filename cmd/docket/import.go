package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

type importResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

var importCmd = &cobra.Command{
	Use:   "import <file>",
	Short: "Import issues from a JSON export file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		merge, _ := cmd.Flags().GetBool("merge")
		replace, _ := cmd.Flags().GetBool("replace")

		if merge && replace {
			return cmdErr(fmt.Errorf("--merge and --replace are mutually exclusive"), output.ErrValidation)
		}

		// Read and parse the export file.
		data, err := os.ReadFile(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("reading file: %w", err), output.ErrGeneral)
		}

		var export model.ExportData
		if err := json.Unmarshal(data, &export); err != nil {
			return cmdErr(fmt.Errorf("parsing JSON: %w", err), output.ErrValidation)
		}

		// Validate export data before any mutations.
		if errs := validateExportData(&export); len(errs) > 0 {
			msg := fmt.Sprintf("validation failed with %d error(s):", len(errs))
			for _, e := range errs {
				msg += "\n  - " + e
			}
			return cmdErr(fmt.Errorf("%s", msg), output.ErrValidation)
		}

		// Determine import mode.
		if replace {
			// In human mode, prompt for confirmation.
			if !w.JSONMode {
				var confirmed bool
				form := huh.NewForm(
					huh.NewGroup(
						huh.NewConfirm().
							Title("This will delete ALL existing data and replace it with the import file. Continue?").
							Affirmative("Yes, replace all data").
							Negative("Cancel").
							Value(&confirmed),
					),
				)

				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						w.Info("Cancelled.")
						return nil
					}
					return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
				}

				if !confirmed {
					w.Info("Cancelled.")
					return nil
				}
			}

			if err := db.ClearAllData(conn); err != nil {
				return cmdErr(fmt.Errorf("clearing database: %w", err), output.ErrGeneral)
			}
		} else if !merge {
			// Default mode: require empty database.
			count, err := db.CountIssues(conn)
			if err != nil {
				return cmdErr(fmt.Errorf("checking database: %w", err), output.ErrGeneral)
			}
			if count > 0 {
				return cmdErr(
					fmt.Errorf("database is not empty: use --merge to merge with existing data or --replace to replace it"),
					output.ErrConflict,
				)
			}
		}

		// Perform the import within a single transaction.
		result, err := doImport(conn, &export)
		if err != nil {
			return cmdErr(fmt.Errorf("importing data: %w", err), output.ErrGeneral)
		}

		var message string
		if !w.JSONMode {
			if merge {
				message = fmt.Sprintf("Imported %d entities, skipped %d duplicates", result.Imported, result.Skipped)
			} else {
				message = fmt.Sprintf("Imported %d entities", result.Imported)
			}
		}
		w.Success(result, message)
		return nil
	},
}

// validateExportData checks the export data for structural validity.
func validateExportData(export *model.ExportData) []string {
	var errs []string

	if export.Version != 1 {
		errs = append(errs, fmt.Sprintf("unsupported version %d: expected 1", export.Version))
	}

	// Issues are validated by UnmarshalJSON (status, priority, kind), but we
	// re-validate here to collect all errors instead of failing on the first.
	for _, issue := range export.Issues {
		if err := model.ValidateStatus(issue.Status); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
		if err := model.ValidatePriority(issue.Priority); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
		if err := model.ValidateIssueKind(issue.Kind); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
	}

	for _, rel := range export.Relations {
		if err := model.ValidateRelationType(rel.RelationType); err != nil {
			errs = append(errs, fmt.Sprintf("relation %d: %s", rel.ID, err))
		}
	}

	return errs
}

// doImport inserts all export data into the database. In merge mode, existing
// IDs are skipped. Returns counts of imported and skipped entities.
func doImport(conn *sql.DB, export *model.ExportData) (*importResult, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var imported, skipped int

	// 1. Labels (no FK dependencies).
	for _, label := range export.Labels {
		inserted, err := db.InsertLabelWithID(tx, label)
		if err != nil {
			return nil, fmt.Errorf("inserting label %q: %w", label.Name, err)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}

	// 2. Issues: insert all with parent_id = NULL first, then UPDATE parent_id.
	parentIDs := make(map[int]*int) // issue ID -> original parent_id
	for _, issue := range export.Issues {
		// Stash parent_id and insert without it for safe insertion order.
		// We avoid mutating the caller's data by restoring after insert.
		origParentID := issue.ParentID
		if issue.ParentID != nil {
			pid := *issue.ParentID
			parentIDs[issue.ID] = &pid
			issue.ParentID = nil
		}
		inserted, err := db.InsertIssueWithID(tx, issue)
		if err != nil {
			issue.ParentID = origParentID
			return nil, fmt.Errorf("inserting issue %s: %w", model.FormatID(issue.ID), err)
		}
		issue.ParentID = origParentID
		if inserted {
			imported++
		} else {
			skipped++
			// Remove from parentIDs so we don't UPDATE a skipped issue.
			delete(parentIDs, issue.ID)
		}
	}

	// Now restore parent_id references for newly inserted issues.
	for issueID, parentID := range parentIDs {
		_, err := tx.Exec(`UPDATE issues SET parent_id = ? WHERE id = ?`, *parentID, issueID)
		if err != nil {
			return nil, fmt.Errorf("setting parent_id for issue %s: %w", model.FormatID(issueID), err)
		}
	}

	// 3. Issue-label mappings.
	for _, m := range export.IssueLabelMappings {
		inserted, err := db.InsertIssueLabelMapping(tx, m.IssueID, m.LabelID)
		if err != nil {
			return nil, fmt.Errorf("inserting issue-label mapping (issue=%d, label=%d): %w", m.IssueID, m.LabelID, err)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}

	// 4. Issue-file mappings.
	for _, m := range export.IssueFileMappings {
		inserted, err := db.InsertIssueFileMapping(tx, m.IssueID, m.FilePath)
		if err != nil {
			return nil, fmt.Errorf("inserting issue-file mapping (issue=%d, file=%q): %w", m.IssueID, m.FilePath, err)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}

	// 5. Comments.
	for _, comment := range export.Comments {
		inserted, err := db.InsertCommentWithID(tx, comment)
		if err != nil {
			return nil, fmt.Errorf("inserting comment %d: %w", comment.ID, err)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}

	// 6. Relations.
	for _, rel := range export.Relations {
		inserted, err := db.InsertRelationWithID(tx, &rel)
		if err != nil {
			return nil, fmt.Errorf("inserting relation %d: %w", rel.ID, err)
		}
		if inserted {
			imported++
		} else {
			skipped++
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &importResult{Imported: imported, Skipped: skipped}, nil
}

func init() {
	importCmd.Flags().Bool("merge", false, "Merge with existing database, skip duplicates by ID")
	importCmd.Flags().Bool("replace", false, "Replace entire database (destructive)")
	rootCmd.AddCommand(importCmd)
}
