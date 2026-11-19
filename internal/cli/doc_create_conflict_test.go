package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// runDocCreate drives `docket doc create <args> --json` through the real root
// command, resetting every flag afterwards so no later invocation inherits
// this one's --title or --idempotency-key.
func runDocCreate(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetArgs(append([]string{"doc", "create", "--json"}, args...))
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		docCreateCmd.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	restore := captureStdout(t)
	defer restore()
	return rootCmd.Execute()
}

// TestDocCreateKeyMismatchIsConflict: reusing an idempotency key with a
// different doc body is a CONFLICT (exit 4) whose message names the key, and
// creates no second doc.
func TestDocCreateKeyMismatchIsConflict(t *testing.T) {
	conn := newVoteCreateStore(t)
	const key = "doc-key-1"

	err := runDocCreate(t, "-t", "design", "-d", "first body", "--idempotency-key", key)
	testsupport.Must(t, err, "first doc create: %v", err)

	err = runDocCreate(t, "-t", "design", "-d", "second body", "--idempotency-key", key)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrConflict {
		t.Fatalf("doc create reusing %q with a different body returned %v, want CONFLICT", key, err)
	}
	if got := output.ExitCodeForError(cmdError.Code); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("conflict %q does not name the key %q", err, key)
	}

	docs, _, err := db.ListDocs(conn, db.DocListOptions{})
	testsupport.Must(t, err, "ListDocs: %v", err)
	if len(docs) != 1 {
		t.Errorf("store holds %d docs after the rejected reuse, want 1", len(docs))
	}
}
