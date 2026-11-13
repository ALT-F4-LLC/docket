package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// eventsPruneTestCmd is `events prune`'s flag set on a pristine command with
// values set.
func eventsPruneTestCmd(t *testing.T, conn *sql.DB, values map[string]string) *cobra.Command {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().Int64("before", 0, "")
	cmd.Flags().String("before-run", "", "")
	cmd.Flags().String("run", "", "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Bool("yes", false, "")
	for name, value := range values {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("Set(%s): %v", name, err)
		}
	}
	return cmd
}

// Without --yes the prune is refused with VALIDATION_ERROR and prints nothing.
func TestEventsPruneRefusesWithoutYes(t *testing.T) {
	conn := newTestDB(t)
	followTrustEvent(t, conn, "kept")

	w, buf := bufWriter(false)
	err := runEventsPrune(eventsPruneTestCmd(t, conn, map[string]string{"before": "5"}), w)

	assertCmdCode(t, err, output.ErrValidation, "events prune without --yes")
	if buf.Len() != 0 {
		t.Errorf("a refused prune printed %q", buf.String())
	}
}

// --dry-run prints the human line naming the count and the retained minimum,
// and deletes nothing.
func TestEventsPruneDryRunHumanLine(t *testing.T) {
	conn := newTestDB(t)
	followTrustEvent(t, conn, "one")
	followTrustEvent(t, conn, "two")
	followTrustEvent(t, conn, "three")
	var oldest int64
	if err := conn.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&oldest); err != nil {
		t.Fatalf("reading the oldest seq: %v", err)
	}

	w, buf := bufWriter(false)
	cmd := eventsPruneTestCmd(t, conn, map[string]string{
		"before": fmt.Sprint(oldest + 1), "dry-run": "true",
	})
	if err := runEventsPrune(cmd, w); err != nil {
		t.Fatalf("events prune --dry-run: %v", err)
	}

	// A dry run deletes nothing, so the retained minimum is still the oldest.
	want := fmt.Sprintf("Would prune 1 event(s). Retained minimum is now seq %d.\n", oldest)
	if got := buf.String(); !strings.HasSuffix(got, want) {
		t.Errorf("printed %q, want the line %q", got, want)
	}
	var left int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE seq >= ?`, oldest).Scan(&left); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if left != 3 {
		t.Errorf("a dry run left %d events, want all 3", left)
	}
}
