package cli

import (
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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

// scopeOf reads `--scope` into a verb input: nil when the flag was not given,
// so an edit leaves the declaration alone, and an empty list for `--scope=`,
// which clears it back to NULL.
func scopeOf(cmd *cobra.Command) *[]string {
	if !cmd.Flags().Changed(scopeFlag) {
		return nil
	}
	globs, _ := cmd.Flags().GetStringSlice(scopeFlag)
	return &globs
}

// scopeGlobsJSON reads `--scope` and returns it as the JSON array the column
// stores, or "" when the flag was not given; verb.ScopeGlobsJSON encodes it.
func scopeGlobsJSON(cmd *cobra.Command) (string, error) {
	scope := scopeOf(cmd)
	if scope == nil {
		return "", nil
	}
	return fromVerb(verb.ScopeGlobsJSON(*scope))
}

// applyScope writes an issue's scope when `--scope` was given, and leaves the
// column untouched otherwise — an `issue edit --title X` must not clear a scope
// the operator declared earlier.
func applyScope(cmd *cobra.Command, conn store.LegacyDB, issueID int) error {
	if !cmd.Flags().Changed(scopeFlag) {
		return nil
	}
	globsJSON, err := scopeGlobsJSON(cmd)
	if err != nil {
		return err
	}
	if err := store.StoreOn(conn).SetIssueScopeGlobs(issueID, globsJSON); err != nil {
		return cmdErr(err, output.ErrGeneral)
	}
	return nil
}
