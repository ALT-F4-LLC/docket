package testsupport

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const postgresChildEnv = "TESTSUPPORT_POSTGRES_CHILD"

// TestPostgresHelperFailsWhenRequired pins that a run which declares the
// server required fails without a DSN instead of silently skipping the
// PostgreSQL suite. t.Fatal and t.Skip both halt the calling test, so only a
// subprocess exit status tells them apart.
func TestPostgresHelperFailsWhenRequired(t *testing.T) {
	out, err := runPostgresChild(t, postgresRequiredEnv+"=1")
	if err == nil {
		t.Fatalf("subprocess succeeded with %s set and no %s, want failure; output:\n%s", postgresRequiredEnv, postgresDSNEnv, out)
	}
	if !strings.Contains(string(out), postgresDSNEnv+" is empty but "+postgresRequiredEnv+" is set") {
		t.Errorf("subprocess failed without the helper's required-server message; output:\n%s", out)
	}
}

// TestPostgresHelperSkipsUnderCIWhenNotRequired pins that CI alone does not
// demand a server: the engine sets CI=1 for every gate child, and those
// children carry no DSN.
func TestPostgresHelperSkipsUnderCIWhenNotRequired(t *testing.T) {
	out, err := runPostgresChild(t, "CI=1")
	if err != nil {
		t.Fatalf("subprocess failed with CI=1 and no %s, want skip; err: %v; output:\n%s", postgresRequiredEnv, err, out)
	}
	if !strings.Contains(string(out), postgresDSNEnv+" is empty; skipping") {
		t.Errorf("subprocess did not report the helper's skip; output:\n%s", out)
	}
}

func TestPostgresDSNReturnsConfiguredValue(t *testing.T) {
	const dsn = "postgres://docket@localhost/docket_test"
	t.Setenv(postgresDSNEnv, dsn)
	if got := PostgresDSN(t); got != dsn {
		t.Fatalf("PostgresDSN = %q, want %q", got, dsn)
	}
}

// runPostgresChild re-executes this test binary on TestPostgresHelperChild
// with CI and the helper's variables removed from the inherited environment,
// then adds extraEnv.
func runPostgresChild(t *testing.T, extraEnv ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPostgresHelperChild$", "-test.v")
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CI=") ||
			strings.HasPrefix(kv, postgresDSNEnv+"=") ||
			strings.HasPrefix(kv, postgresRequiredEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(append(env, postgresChildEnv+"=1"), extraEnv...)
	return cmd.CombinedOutput()
}

// TestPostgresHelperChild is the subprocess body for the tests above and a
// no-op in an ordinary run.
func TestPostgresHelperChild(t *testing.T) {
	if os.Getenv(postgresChildEnv) == "" {
		t.Skip("not a child invocation")
	}
	PostgresDSN(t)
	t.Log("helper returned without halting")
}
