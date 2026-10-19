package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// issueCommentAddVerb drives the real `issue comment add` command object. The
// command is package-global, so every flag it sets is restored afterwards.
func issueCommentAddVerb(t *testing.T, conn *sql.DB, args []string, flags map[string]string) error {
	t.Helper()
	commentAddCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	for name, value := range flags {
		f := commentAddCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`issue comment add` has no --%s", name)
		}
		def := f.DefValue
		testsupport.Must(t, commentAddCmd.Flags().Set(name, value), "setting --%s to %q", name, value)
		t.Cleanup(func() {
			_ = f.Value.Set(def)
			f.Changed = false
		})
	}
	restore := captureStdout(t)
	err := commentAddCmd.RunE(commentAddCmd, args)
	restore()
	return err
}

// TestIssueCommentAddReportsKeyMismatchAsConflict: reusing an idempotency key
// with a different body is CONFLICT, exit 4, naming the key, and adds nothing.
func TestIssueCommentAddReportsKeyMismatchAsConflict(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "commented on twice", model.StatusTodo, model.PriorityNone)
	ref := model.FormatID(id)

	testsupport.Must(t, issueCommentAddVerb(t, conn, []string{ref},
		map[string]string{"message": "first body", "idempotency-key": "retry-key"}),
		"first comment add failed")

	err := issueCommentAddVerb(t, conn, []string{ref},
		map[string]string{"message": "second body", "idempotency-key": "retry-key"})
	if err == nil {
		t.Fatal("reusing the key with a different body succeeded")
	}
	code := codeOf(t, err)
	if code != output.ErrConflict {
		t.Errorf("code = %s, want CONFLICT", code)
	}
	if exit := output.ExitCodeForError(code); exit != 4 {
		t.Errorf("exit = %d, want 4", exit)
	}
	if !strings.Contains(err.Error(), "retry-key") {
		t.Errorf("message %q does not name the key retry-key", err)
	}
	if comments := notesOn(t, conn, id); len(comments) != 1 {
		t.Errorf("issue has %d comments, want only the first: %q", len(comments), noteBodies(comments))
	}
}
