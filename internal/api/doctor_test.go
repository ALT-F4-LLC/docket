package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
)

func TestDoctorHandlersBindVerbLayer(t *testing.T) {
	client := exportClient(t)
	req := connect.NewRequest(&docketv1.DoctorRequest{})
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, t.TempDir())

	res, err := client.Doctor(context.Background(), req)

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("Doctor answered unimplemented: %v", err)
	}
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	// verb.Doctor appends the postgres row after the engine's checks, so its
	// presence shows the verb, not some other path, answered.
	checks := res.Msg.GetChecks()
	if len(checks) == 0 {
		t.Fatal("Doctor returned no checks")
	}
	last := checks[len(checks)-1]
	if last.GetName() != "postgres" || last.GetStatus() != "OK" {
		t.Errorf("last check = %q %q, want postgres OK", last.GetName(), last.GetStatus())
	}
}
