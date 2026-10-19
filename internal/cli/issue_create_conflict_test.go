package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueCreateReportsKeyReuseAsConflict: a key reused with a different
// title is CONFLICT, exit 4, with a message naming the key.
func TestIssueCreateReportsKeyReuseAsConflict(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), dbKey, conn))
	createCmd.SetContext(cmd.Context())
	resetCreateFlags()
	t.Cleanup(resetCreateFlags)

	const key = "reused-key"
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v2"), "set --json")
	testsupport.Must(t, createCmd.Flags().Set("idempotency-key", key), "set --idempotency-key")

	create := func(title string) error {
		t.Helper()
		testsupport.Must(t, createCmd.Flags().Set("title", title), "set --title")
		restore := captureStdout(t)
		defer restore()
		return createCmd.RunE(createCmd, nil)
	}

	testsupport.Must(t, create("first"), "first issue create")

	err := create("second")
	code := codeOf(t, err)
	if code != output.ErrConflict || output.ExitCodeForError(code) != 4 {
		t.Fatalf("reuse with a different title returned code %s (exit %d), want %s (exit 4)",
			code, output.ExitCodeForError(code), output.ErrConflict)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("conflict %q does not name the key %q", err, key)
	}
}
