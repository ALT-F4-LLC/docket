package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// ifVersionFlag is the name of the optimistic-concurrency precondition flag.
const ifVersionFlag = "if-version"

// addIfVersionFlag registers --if-version on a mutating command.
func addIfVersionFlag(cmd *cobra.Command) {
	cmd.Flags().Int(ifVersionFlag, 0,
		"Only apply if the entity is at this version (optimistic concurrency); CONFLICT otherwise")
}

// ifVersionOf returns the --if-version precondition, or nil when the flag was
// not supplied. Cobra's Changed is what distinguishes "not given" from an
// explicit value, so a zero default is not mistaken for a precondition.
func ifVersionOf(cmd *cobra.Command) (*int, error) {
	if !cmd.Flags().Changed(ifVersionFlag) {
		return nil, nil
	}
	v, err := cmd.Flags().GetInt(ifVersionFlag)
	if err != nil {
		return nil, cmdErr(err, output.ErrValidation)
	}
	if v < 1 {
		return nil, cmdErr(
			fmt.Errorf("--%s must be >= 1, got %d (versions start at 1)", ifVersionFlag, v),
			output.ErrValidation,
		)
	}
	return &v, nil
}

// idempotencyKeyFlag is the name of the create-verb replay-protection flag.
const idempotencyKeyFlag = "idempotency-key"

// addIdempotencyKeyFlag registers --idempotency-key on a create command.
func addIdempotencyKeyFlag(cmd *cobra.Command) {
	cmd.Flags().String(idempotencyKeyFlag, "",
		"Replay-protection key: repeating a create with the same key returns the original entity")
}

// idempotencyKeyOf returns the --idempotency-key value ("" when absent).
func idempotencyKeyOf(cmd *cobra.Command) (string, error) {
	key, err := cmd.Flags().GetString(idempotencyKeyFlag)
	if err != nil {
		return "", cmdErr(err, output.ErrValidation)
	}
	if cmd.Flags().Changed(idempotencyKeyFlag) && key == "" {
		return "", cmdErr(
			fmt.Errorf("--%s must not be empty", idempotencyKeyFlag),
			output.ErrValidation,
		)
	}
	return key, nil
}

// casError maps a CAS failure to its CLI error code. A version mismatch is
// CONFLICT (exit 4); a missing row stays NOT_FOUND (exit 2), delegated to
// notFound rather than re-implementing its db.ErrNotFound check. Anything
// else passes through unchanged.
func casError(err error, label string) error {
	if errors.Is(err, db.ErrVersionConflict) {
		return cmdErr(
			fmt.Errorf("%s was modified by another writer (version mismatch)", label),
			output.ErrConflict,
		)
	}
	return notFound(err, label)
}
