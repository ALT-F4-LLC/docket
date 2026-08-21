package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// TokenEnvVar is the environment variable carrying a capability token.
const TokenEnvVar = "DOCKET_TOKEN"

// readToken returns the capability token for a lease-mediated verb.
//
// Accepted channels, in precedence order (engine-spec.md §4: "Tokens pass via
// env/stdin, never argv"):
//
//  1. the DOCKET_TOKEN environment variable
//  2. stdin, when DOCKET_TOKEN is unset and stdin is not a terminal
//
// There is deliberately NO --token flag, on any verb — not a deprecated one,
// not a hidden one. argv is world-readable through `ps` on a shared host, so a
// flag would defeat the capability model at the transport layer. A guard test
// (TestNoTokenFlag) asserts none exists in the tree.
func readToken(stdin io.Reader) (string, error) {
	if tok := strings.TrimSpace(os.Getenv(TokenEnvVar)); tok != "" {
		return tok, nil
	}

	if f, ok := stdin.(*os.File); ok {
		info, err := f.Stat()
		// A terminal means nobody piped a token: reading would block forever
		// waiting for input the caller does not know to give.
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", errMissingToken()
		}
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", cmdErr(fmt.Errorf("reading token from stdin: %w", err), output.ErrValidation)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", errMissingToken()
	}
	return tok, nil
}

// optionalToken reads a capability token for a verb that requires one only
// when a live lease exists (`issue close`). A missing token is not an error
// here: it yields the empty string, which authorizes nothing and is refused by
// the lease check only if a live lease is actually present.
//
// This is what keeps a lease-ending verb byte-compatible on an unclaimed
// issue — the overwhelmingly common case, and the one engine-spec.md §9 item 8
// is about.
func optionalToken(stdin io.Reader) string {
	tok, err := readToken(stdin)
	if err != nil {
		return ""
	}
	return tok
}

// errMissingToken names both accepted channels, since a caller that supplied
// neither most likely does not know which are accepted.
func errMissingToken() error {
	return cmdErr(
		fmt.Errorf(
			"no capability token supplied: set %s or pipe the token on stdin (tokens are never passed in argv)",
			TokenEnvVar,
		),
		output.ErrValidation,
	)
}

// leaseError maps a lease refusal to its CLI error code, per the refusal matrix
// in docs/tdd/claims-leases.md §4 (engine-spec.md §9 item 3). Anything else
// passes through unchanged, as casError does.
//
// The error texts never echo the presented token: a token that leaks into a
// log line or a CI transcript is a live capability.
func leaseError(err error, label string) error {
	switch {
	case errors.Is(err, db.ErrLeaseHeld):
		return cmdErr(
			fmt.Errorf("%s is already claimed by another holder", label),
			output.ErrConflict,
		)
	case errors.Is(err, db.ErrNotHolder):
		return cmdErr(
			fmt.Errorf("%s: the supplied token does not hold this lease", label),
			output.ErrAuth,
		)
	case errors.Is(err, db.ErrLeaseExpired):
		return cmdErr(
			fmt.Errorf("%s: the lease has expired; claim it again to continue", label),
			output.ErrStaleLease,
		)
	}
	return notFound(err, label)
}
