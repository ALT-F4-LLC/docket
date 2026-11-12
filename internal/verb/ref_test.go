package verb_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// requireVerbError asserts err is a *verb.Error carrying code and a message
// that starts with wantPrefix.
func requireVerbError(t *testing.T, err error, code output.ErrorCode, wantPrefix string) {
	t.Helper()
	var verr *verb.Error
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v (%T), want *verb.Error", err, err)
	}
	if verr.Code != code {
		t.Errorf("code = %s, want %s", verr.Code, code)
	}
	if !strings.HasPrefix(verr.Error(), wantPrefix) {
		t.Errorf("message = %q, want prefix %q", verr.Error(), wantPrefix)
	}
}

func TestParseIssueRefAcceptsPrefixedAndBareIDs(t *testing.T) {
	for _, ref := range []string{"DKT-42", "dkt-42", "42", " 42 "} {
		id, err := verb.ParseIssueRef(ref)
		if err != nil {
			t.Errorf("ParseIssueRef(%q) error = %v", ref, err)
			continue
		}
		if id != 42 {
			t.Errorf("ParseIssueRef(%q) = %d, want 42", ref, id)
		}
	}
}

func TestParseIssueRefRejectsMalformedRefAsValidation(t *testing.T) {
	for _, ref := range []string{"", "abc", "DKT-", "DKT-0", "-3"} {
		_, err := verb.ParseIssueRef(ref)
		requireVerbError(t, err, output.ErrValidation, "invalid issue ID: ")
	}
}

func TestParseRunRefAcceptsPrefixedAndBareIDs(t *testing.T) {
	for _, ref := range []string{"RUN-7", "run-7", "7", " RUN-7 "} {
		id, err := verb.ParseRunRef(ref)
		if err != nil {
			t.Errorf("ParseRunRef(%q) error = %v", ref, err)
			continue
		}
		if id != 7 {
			t.Errorf("ParseRunRef(%q) = %d, want 7", ref, id)
		}
	}
}

func TestParseRunRefRejectsMalformedRefAsValidation(t *testing.T) {
	for _, ref := range []string{"", "RUN-", "RUN-0", "STEP-7", "DKT-7", "seven"} {
		_, err := verb.ParseRunRef(ref)
		requireVerbError(t, err, output.ErrValidation, "invalid run ID: ")
	}
}

func TestLeaseErrorMapsEachRefusal(t *testing.T) {
	cases := []struct {
		cause error
		code  output.ErrorCode
		want  string
	}{
		{store.ErrLeaseHeld, output.ErrConflict, "issue DKT-5 is already claimed by another holder"},
		{store.ErrNotHolder, output.ErrAuth, "issue DKT-5: the supplied token does not hold this lease"},
		{store.ErrLeaseExpired, output.ErrStaleLease, "issue DKT-5: the lease has expired; claim it again to continue"},
		{store.ErrNotFound, output.ErrNotFound, "issue DKT-5 not found"},
	}
	for _, c := range cases {
		err := verb.LeaseError(fmt.Errorf("claim: %w", c.cause), "issue DKT-5")
		requireVerbError(t, err, c.code, c.want)
		if err.Error() != c.want {
			t.Errorf("LeaseError(%v) = %q, want %q", c.cause, err.Error(), c.want)
		}
	}
}

func TestLeaseErrorReturnsNilForOtherErrors(t *testing.T) {
	for _, cause := range []error{errors.New("disk full"), store.ErrVersionConflict} {
		if err := verb.LeaseError(cause, "issue DKT-5"); err != nil {
			t.Errorf("LeaseError(%v) = %v, want nil", cause, err)
		}
	}
}

func TestCASErrorMapsVersionConflictAndNotFound(t *testing.T) {
	cases := []struct {
		cause error
		code  output.ErrorCode
		want  string
	}{
		{store.ErrVersionConflict, output.ErrConflict, "RUN-3 was modified by another writer (version mismatch)"},
		{store.ErrNotFound, output.ErrNotFound, "RUN-3 not found"},
	}
	for _, c := range cases {
		err := verb.CASError(fmt.Errorf("update: %w", c.cause), "RUN-3")
		requireVerbError(t, err, c.code, c.want)
		if err.Error() != c.want {
			t.Errorf("CASError(%v) = %q, want %q", c.cause, err.Error(), c.want)
		}
	}
}

func TestCASErrorReturnsNilForOtherErrors(t *testing.T) {
	for _, cause := range []error{errors.New("disk full"), store.ErrLeaseHeld} {
		if err := verb.CASError(cause, "RUN-3"); err != nil {
			t.Errorf("CASError(%v) = %v, want nil", cause, err)
		}
	}
}
