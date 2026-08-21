package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

type importResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
	// Remapped counts rows whose source id was already taken in this store
	// and which were assigned a fresh id, with every reference rewritten.
	// Before v12 those rows were silently skipped — the consolidation trap.
	Remapped int `json:"remapped"`
}

// newImportCmd builds a fresh import command with its own flag set, real
// registration included. Production wires exactly one instance (importCmd,
// below); tests use this constructor directly so the command under test
// carries the same --merge/--replace/--yes flags a real invocation parses,
// rather than a hand-rolled stand-in that can drift from them.
func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import issues from a JSON export file",
		Args:  cobra.ExactArgs(1),
		RunE:  runImport,
	}
	cmd.Flags().Bool("merge", false,
		"Import into a non-empty project; colliding ids are remapped, nothing is dropped")
	cmd.Flags().Bool("replace", false,
		"Replace THIS PROJECT's data with the import file (destructive)")
	cmd.Flags().Bool("yes", false,
		"Confirm --replace's destructive data replacement (required in every output mode)")
	return cmd
}

var importCmd = newImportCmd()

func runImport(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)
	conn := getDB(cmd)

	merge, _ := cmd.Flags().GetBool("merge")
	replace, _ := cmd.Flags().GetBool("replace")
	yes, _ := cmd.Flags().GetBool("yes")

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
		// --replace is destructive in every output mode, so --yes is
		// required unconditionally: an output-format flag, or whether a
		// terminal happens to be attached, must never double as consent
		// (DKT-15). See events_prune.go's P6 comment for the same rule
		// applied to another destructive verb.
		if !yes {
			return cmdErr(fmt.Errorf("--replace deletes THIS PROJECT's existing data; pass --yes to confirm"), output.ErrValidation)
		}
	} else if !merge {
		// Default mode: require an empty PROJECT — under the shared store,
		// another project's issues are not "existing data" for this one.
		count, err := db.CountIssues(conn, getProjectID(cmd))
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

	// Perform the import within a single transaction, into THIS project.
	result, err := doImport(conn, &export, replace, getProjectID(cmd))
	if err != nil {
		return cmdErr(fmt.Errorf("importing data: %w", err), output.ErrGeneral)
	}

	var message string
	if !w.JSONMode {
		message = fmt.Sprintf("Imported %d entities", result.Imported)
		if result.Remapped > 0 {
			message += fmt.Sprintf(", %d assigned fresh ids (source ids were taken in this store)", result.Remapped)
		}
		if result.Skipped > 0 {
			message += fmt.Sprintf(", skipped %d duplicates", result.Skipped)
		}
	}
	w.Success(result, message)
	return nil
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

	for _, p := range export.Proposals {
		if err := model.ValidateCriticality(p.Criticality); err != nil {
			errs = append(errs, fmt.Sprintf("proposal %s: %s", model.FormatProposalID(p.ID), err))
		}
		if err := model.ValidateProposalStatus(p.Status); err != nil {
			errs = append(errs, fmt.Sprintf("proposal %s: %s", model.FormatProposalID(p.ID), err))
		}
	}

	for _, v := range export.Votes {
		if err := model.ValidateVerdict(v.Verdict); err != nil {
			errs = append(errs, fmt.Sprintf("vote %d: %s", v.ID, err))
		}
	}

	return errs
}

// doImport inserts all export data into the database, into one project.
// Source ids already taken in the store are remapped rather than skipped —
// see remapExportForImport. Returns counts of imported, skipped, and
// remapped entities.
func doImport(conn *sql.DB, export *model.ExportData, replace bool, projectID int) (*importResult, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	if replace {
		// Replace clears THIS PROJECT's data, never the store's: under the
		// shared store, "replace everything" scoped any wider would destroy
		// projects the operator was not looking at.
		if err := db.ClearProjectDataTx(tx, projectID); err != nil {
			return nil, fmt.Errorf("clearing project data: %w", err)
		}
	}

	remapped, err := remapExportForImport(tx, export, projectID)
	if err != nil {
		return nil, err
	}

	// exportCollections is listed in an order that satisfies every foreign key,
	// which is why restoring it front to back needs no ordering logic here.
	var imported, skipped int
	for _, c := range exportCollections {
		collectionImported, collectionSkipped, err := c.restore(tx, export)
		if err != nil {
			return nil, err
		}
		imported += collectionImported
		skipped += collectionSkipped
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &importResult{Imported: imported, Skipped: skipped, Remapped: remapped}, nil
}

func init() {
	rootCmd.AddCommand(importCmd)
}
