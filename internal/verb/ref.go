package verb

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ParseIssueRef parses an issue reference ("DKT-5", a project's own prefix,
// or a bare number) and reports a malformed one as VALIDATION_ERROR.
func ParseIssueRef(ref string) (int, error) {
	id, err := model.ParseID(ref)
	if err != nil {
		return 0, newError(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// ParseRunRef parses a run reference ("RUN-3" or a bare number) and reports
// a malformed one as VALIDATION_ERROR. An empty reference is malformed here;
// a verb whose run is optional checks for empty before calling it.
func ParseRunRef(ref string) (int, error) {
	runID, err := model.ParseRunID(ref)
	if err != nil {
		return 0, newError(fmt.Errorf("invalid run ID: %w", err), output.ErrValidation)
	}
	return runID, nil
}

// LeaseError maps a lease refusal, or a missing row, to its error code per
// the lease refusal matrix, naming the leased entity by label. It returns nil
// for any other error, which the caller wraps itself.
//
// The texts never echo the presented token: a token that leaks into a log
// line or a CI transcript is a live capability.
func LeaseError(err error, label string) error {
	switch {
	case errors.Is(err, store.ErrLeaseHeld):
		return newError(fmt.Errorf("%s is already claimed by another holder", label), output.ErrConflict)
	case errors.Is(err, store.ErrNotHolder):
		return newError(fmt.Errorf("%s: the supplied token does not hold this lease", label), output.ErrAuth)
	case errors.Is(err, store.ErrLeaseExpired):
		return newError(fmt.Errorf("%s: the lease has expired; claim it again to continue", label), output.ErrStaleLease)
	}
	return notFound(err, label)
}

// CASError maps a compare-and-swap version mismatch to CONFLICT and a missing
// row to NOT_FOUND, naming the entity by label. It returns nil for any other
// error, which the caller wraps itself.
func CASError(err error, label string) error {
	if errors.Is(err, store.ErrVersionConflict) {
		return newError(fmt.Errorf("%s was modified by another writer (version mismatch)", label), output.ErrConflict)
	}
	return notFound(err, label)
}

func notFound(err error, label string) error {
	if errors.Is(err, store.ErrNotFound) {
		return newError(fmt.Errorf("%s not found", label), output.ErrNotFound)
	}
	return nil
}
