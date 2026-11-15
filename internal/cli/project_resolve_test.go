package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func registeredEventCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`,
		engine.EventProjectRegistered).Scan(&n)
	testsupport.Must(t, err, "counting project-registered events: %v", err)
	return n
}

// projectCount is the assertion that matters most to a verb run from a
// directory that is not a repository: the store must not GROW from it.
func projectCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n)
	testsupport.Must(t, err, "counting projects: %v", err)
	return n
}
