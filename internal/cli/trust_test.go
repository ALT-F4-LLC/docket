package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestTrustRmMissingEntryIsNotFound: removing a name an existing store does
// not hold is NOT_FOUND. trust.Remove reports that absence as a false
// `removed`, not as an error, so the verb's own branch is what yields the code.
func TestTrustRmMissingEntryIsNotFound(t *testing.T) {
	_, cfg := trustRepo(t)
	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "trust add: %v", err)

	err = runTrustVerb(t, cfg, newTrustRmCmd(), "never-added")
	if err == nil {
		t.Fatal("trust rm of an unknown name must fail")
	}
	if code := errorCodeOf(t, err); code != output.ErrNotFound {
		t.Errorf("trust rm of an unknown name is %s, want NOT_FOUND: %v", code, err)
	}
}
