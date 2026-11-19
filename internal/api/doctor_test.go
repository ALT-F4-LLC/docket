package api

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/output"
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

// TestDoctorRefusedOverHTTP: over HTTP, Doctor is refused before any check
// runs, so an existing source, a missing source and no source all draw the
// same error, and that error names none of the client's paths.
func TestDoctorRefusedOverHTTP(t *testing.T) {
	overHTTP := serveStampedHTTP(t, openProjectStore(t))
	cwd := t.TempDir()
	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "absent")
	// The client's interceptor sends os.Getwd() as Docket-Cwd.
	t.Chdir(cwd)

	paths := []string{cwd, existing, missing}
	for _, p := range []string{cwd, existing} {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			paths = append(paths, resolved)
		}
	}

	var first *connect.Error
	for _, source := range []string{existing, missing, ""} {
		_, err := overHTTP.Doctor(context.Background(), connect.NewRequest(&docketv1.DoctorRequest{Source: source}))
		label := "HTTP Doctor with source " + strconv.Quote(source)
		requireRefusal(t, label, err, connect.CodeInvalidArgument, output.ErrValidation)
		var got *connect.Error
		if !errors.As(err, &got) {
			t.Fatalf("%s: error %v is not a Connect error", label, err)
		}
		for _, p := range paths {
			if strings.Contains(got.Message(), p) {
				t.Errorf("%s: message %q names the path %q", label, got.Message(), p)
			}
		}
		if first == nil {
			first = got
			continue
		}
		if got.Code() != first.Code() || got.Message() != first.Message() {
			t.Errorf("%s: got %v %q, want the same %v %q as the first call",
				label, got.Code(), got.Message(), first.Code(), first.Message())
		}
	}
}

// TestDoctorServedOverSocket: a request without the HTTP stamp, the socket
// transport, still runs Doctor's checks against the source it names.
func TestDoctorServedOverSocket(t *testing.T) {
	client := exportClient(t)
	source := t.TempDir()
	req := connect.NewRequest(&docketv1.DoctorRequest{Source: source})
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, t.TempDir())

	res, err := client.Doctor(context.Background(), req)

	if err != nil {
		t.Fatalf("socket Doctor: %v", err)
	}
	for _, c := range res.Msg.GetChecks() {
		if c.GetName() == "install-drift" {
			if !strings.Contains(c.GetDetail(), source) {
				t.Errorf("install-drift detail = %q, want it to name the source %q", c.GetDetail(), source)
			}
			return
		}
	}
	t.Fatalf("socket Doctor returned no install-drift check: %v", res.Msg.GetChecks())
}
