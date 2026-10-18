package testsupport

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const postgresChildEnv = "TESTSUPPORT_POSTGRES_CHILD"

// TestPostgresHelperFailsInCI pins that a CI run without a DSN fails instead
// of silently skipping the PostgreSQL suite. t.Fatal and t.Skip both halt the
// calling test, so only a subprocess exit status tells them apart.
func TestPostgresHelperFailsInCI(t *testing.T) {
	out, err := runPostgresChild(t, "CI=true")
	if err == nil {
		t.Fatalf("subprocess succeeded with CI=true and no %s, want failure; output:\n%s", postgresDSNEnv, out)
	}
	if !strings.Contains(string(out), postgresDSNEnv+" is empty under CI") {
		t.Errorf("subprocess failed without the helper's CI message; output:\n%s", out)
	}
}

// TestPostgresHelperSkipsOutsideCI pins the local-run path: without CI the
// helper skips, so the subprocess passes and reports the skip.
func TestPostgresHelperSkipsOutsideCI(t *testing.T) {
	out, err := runPostgresChild(t)
	if err != nil {
		t.Fatalf("subprocess failed without CI, want skip; err: %v; output:\n%s", err, out)
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
// with CI and the DSN variable removed from the inherited environment, then
// adds extraEnv.
func runPostgresChild(t *testing.T, extraEnv ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPostgresHelperChild$", "-test.v")
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CI=") || strings.HasPrefix(kv, postgresDSNEnv+"=") {
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
