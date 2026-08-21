package model

import (
	"fmt"
	"slices"
)

// validateOneOf returns an error unless value appears in allowed. label
// names the value's kind in the error message ("run status", "issue kind"),
// matching ValidateRunStatus's and ValidateIssueKind's pre-existing text
// exactly. T is constrained to ~string (rather than plain comparable) so
// the %q verb below type-checks under `go vet`: RunStatus and IssueKind are
// both string-based, and vet cannot otherwise see that a fully generic
// comparable would ever be printable as a quoted string.
func validateOneOf[T ~string](value T, allowed []T, label string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("invalid %s %q: must be one of %v", label, value, allowed)
}
