package testsupport

import (
	"os"
	"testing"
)

const (
	postgresDSNEnv          = "DOCKET_TEST_POSTGRES_DSN"
	postgresPasswordFileEnv = "DOCKET_TEST_POSTGRES_PASSWORD_FILE"
	postgresRequiredEnv     = "DOCKET_TEST_POSTGRES_REQUIRED"
)

// PostgresDSN returns the PostgreSQL DSN for tests that need a live server.
// DOCKET_TEST_POSTGRES_DSN carries no password: the store's DSN policy rejects
// one, so the password comes from the file named by
// DOCKET_TEST_POSTGRES_PASSWORD_FILE (mode 0600, owned by the test user), read
// through PostgresPasswordFile. A loopback example:
//
//	DOCKET_TEST_POSTGRES_DSN=postgres://docket@127.0.0.1:5432/docket_test?sslmode=disable
//	DOCKET_TEST_POSTGRES_PASSWORD_FILE=/path/to/0600-file-holding-the-password
//
// Without a DSN, it skips the test unless DOCKET_TEST_POSTGRES_REQUIRED is set,
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

// PostgresPasswordFile returns the password file path for the server
// PostgresDSN names, or "" when the server needs no password.
func PostgresPasswordFile() string {
	return os.Getenv(postgresPasswordFileEnv)
}
