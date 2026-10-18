package testsupport

import (
	"os"
	"testing"
)

const postgresDSNEnv = "DOCKET_TEST_POSTGRES_DSN"

// PostgresDSN returns the PostgreSQL DSN for tests that need a live server.
// Without one, it skips the test on a local run but fails it when CI is set,
// so a CI job missing its database cannot pass by skipping the suite.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresDSNEnv)
	if dsn != "" {
		return dsn
	}
	if os.Getenv("CI") != "" {
		t.Fatal(postgresDSNEnv + " is empty under CI; set it to run the PostgreSQL tests")
	}
	t.Skip(postgresDSNEnv + " is empty; skipping PostgreSQL test")
	return ""
}
