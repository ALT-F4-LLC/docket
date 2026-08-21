package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// validateLimit rejects a negative --limit under the v2 envelope.
//
// A negative limit is meaningless input that v1 silently reinterprets, three
// different ways across three verbs: `issue list` and `next` treat it as
// unlimited (their `limit > 0` guards fail), while `issue log` clamps it to 1
// via max(limit, 1) — so `issue log --limit -1` silently returns exactly one
// row for a request that asked for a negative number. engine-spec.md §5 calls
// for a hard VALIDATION_ERROR on these silent-drop cases.
//
// The check is v2-only by necessity: raising it under v1 would change the
// behavior of a workflow-free repo on an existing verb and fail the §9 item 8
// compatibility criterion. Under v1 all three legacy behaviors are preserved
// exactly.
func validateLimit(cmd *cobra.Command, limit int) error {
	if limit >= 0 {
		return nil
	}
	if _, version := jsonModeOf(cmd); version != output.JSONV2 {
		return nil
	}
	return cmdErr(
		fmt.Errorf("--limit must be >= 0, got %d (0 means no limit)", limit),
		output.ErrValidation,
	)
}
