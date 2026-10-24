package testsupport

import (
	"os"
	"testing"
)

const (
	postgresDSNEnv      = "DOCKET_TEST_POSTGRES_DSN"
	postgresRequiredEnv = "DOCKET_TEST_POSTGRES_REQUIRED"
)

// PostgresDSN returns the PostgreSQL DSN for tests that need a live server.
// Without one, it skips the test unless DOCKET_TEST_POSTGRES_REQUIRED is set,
// in which case it fails, so a job that provides a server cannot pass by
// skipping the suite. CI is not that signal: the engine sets CI=1 for every
// gate child, and those children carry no DSN.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(postgresDSNEnv)
	if dsn != "" {
		return dsn
	}
	if os.Getenv(postgresRequiredEnv) != "" {
		t.Fatal(postgresDSNEnv + " is empty but " + postgresRequiredEnv + " is set; set the DSN to run the PostgreSQL tests")
	}
	t.Skip(postgresDSNEnv + " is empty; skipping PostgreSQL test")
	return ""
}
