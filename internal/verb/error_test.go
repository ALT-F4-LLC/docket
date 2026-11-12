package verb_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func TestErrorReportsCauseMessageAndUnwrapsToIt(t *testing.T) {
	cause := errors.New("boom")
	var err error = &verb.Error{Code: output.ErrGeneral, Err: fmt.Errorf("doing it: %w", cause)}

	if err.Error() != "doing it: boom" {
		t.Errorf("Error() = %q, want %q", err.Error(), "doing it: boom")
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false, want the cause reachable through Unwrap")
	}

	var verr *verb.Error
	if !errors.As(fmt.Errorf("outer: %w", err), &verr) || verr.Code != output.ErrGeneral {
		t.Errorf("errors.As through a wrap = %v, want the *verb.Error with code %s", verr, output.ErrGeneral)
	}
}
