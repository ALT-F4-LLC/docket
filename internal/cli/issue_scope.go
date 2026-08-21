package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `--scope GLOB` (repeatable) on `issue create|edit` is the ONLY writer of
// `issues.scope_globs`.
//
// Scope is a list of path GLOBS declaring what an issue is EXPECTED to touch —
// a judgment, made ahead of the work. It is not `--file`, which records the
// concrete paths an issue actually concerns and which `docket plan` already
// uses to split colliding work. The two differ in cardinality, in semantics
// (intended vs. actual), and in matching rule (glob intersection vs. equality),
// and overloading `--file` would make `plan` output depend on scope
// declarations for repos that never run a workflow at all.
//
// An issue with no `--scope` stores NULL, not `[]`. "No scope declared" and
// "declared to touch nothing" are different facts, and only the first is the
// dormant default every pre-existing issue carries.

const scopeFlag = "scope"

// addScopeFlag registers `--scope` on a command.
func addScopeFlag(cmd *cobra.Command) {
	cmd.Flags().StringSlice(scopeFlag, nil,
		"Path glob this issue is expected to touch (repeatable)")
}

// scopeGlobsJSON reads `--scope` and returns it as the JSON array the column
// stores, or "" when the flag was not given.
//
// Globs are stored in DECLARED ORDER, not sorted: the order is the author's,
// it is echoed back in the issue snapshot, and re-ordering it would make an
// activation-time snapshot differ from what the operator wrote.
func scopeGlobsJSON(cmd *cobra.Command) (string, error) {
	if !cmd.Flags().Changed(scopeFlag) {
		return "", nil
	}

	globs, _ := cmd.Flags().GetStringSlice(scopeFlag)

	cleaned := make([]string, 0, len(globs))
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			return "", cmdErr(
				fmt.Errorf("--scope glob must not be empty"), output.ErrValidation)
		}
		cleaned = append(cleaned, g)
	}

	// `--scope=` with nothing after it clears the declaration back to NULL,
	// which is how an operator retracts a scope they no longer stand behind.
	if len(cleaned) == 0 {
		return "", nil
	}

	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return "", cmdErr(fmt.Errorf("encoding --scope: %w", err), output.ErrGeneral)
	}
	return string(encoded), nil
}

// applyScope writes an issue's scope when `--scope` was given, and leaves the
// column untouched otherwise — an `issue edit --title X` must not clear a scope
// the operator declared earlier.
func applyScope(cmd *cobra.Command, conn *sql.DB, issueID int) error {
	if !cmd.Flags().Changed(scopeFlag) {
		return nil
	}
	globsJSON, err := scopeGlobsJSON(cmd)
	if err != nil {
		return err
	}
	if err := db.SetIssueScopeGlobs(conn, issueID, globsJSON); err != nil {
		return cmdErr(err, output.ErrGeneral)
	}
	return nil
}
