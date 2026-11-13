package verb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func doctorCheck(t *testing.T, got verb.DoctorResult, name string) engine.DoctorCheck {
	t.Helper()
	for _, c := range got.Checks {
		if c.Check == name {
			return c
		}
	}
	t.Fatalf("no %s row in %+v", name, got.Checks)
	return engine.DoctorCheck{}
}

// Doctor is a read: an anchored identity no project holds is reported as
// unbound in the project row and is not registered.
func TestDoctorReportsAnUnregisteredIdentityWithoutRegistering(t *testing.T) {
	t.Setenv(config.PostgresDSNEnv, "")
	st := openStore(t)
	env := projectEnv(st, "/repo/never-registered", true)
	env.Cwd = t.TempDir()

	got, err := verb.Doctor(context.Background(), env, verb.DoctorInput{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}

	want := []string{"seat", "store", "project", "install-drift", "pins", "link-farm", "stragglers", "postgres"}
	if len(got.Checks) != len(want) {
		t.Fatalf("checks = %+v, want %d rows", got.Checks, len(want))
	}
	for i, name := range want {
		if got.Checks[i].Check != name {
			t.Errorf("checks[%d] = %q, want %q", i, got.Checks[i].Check, name)
		}
	}
	project := doctorCheck(t, got, "project")
	if project.Verdict != engine.DoctorFail || !strings.Contains(project.Detail, "/repo/never-registered") {
		t.Errorf("project row = %+v, want FAIL naming the unbound identity", project)
	}
	if postgres := doctorCheck(t, got, "postgres"); postgres.Detail != "not configured" {
		t.Errorf("postgres detail = %q, want %q", postgres.Detail, "not configured")
	}
	if got.Clean || !got.Skipped {
		t.Errorf("clean, skipped = %t, %t; want false, true with neither run nor source", got.Clean, got.Skipped)
	}
	requireUnregistered(t, st, "/repo/never-registered")
}

func TestDoctorRefusesAMalformedRunRef(t *testing.T) {
	st := openStore(t)
	_, err := verb.Doctor(context.Background(), projectEnv(st, "", false), verb.DoctorInput{Run: "not-a-run"})
	requireVerbCode(t, err, output.ErrValidation, "invalid run ID")
}
