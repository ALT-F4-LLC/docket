package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// runDocCommentAdd drives `docket doc comment add <args> --json` through the
// real root command, resetting every flag afterwards so no later invocation
// inherits this one's --message or --idempotency-key.
func runDocCommentAdd(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetArgs(append([]string{"doc", "comment", "add", "--json"}, args...))
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		docCommentAddCmd.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	return rootCmd.Execute()
}

// TestDocCommentAddKeyMismatchIsConflict: reusing an idempotency key with a
// different comment body is a CONFLICT (exit 4) whose message names the key,
// and adds no comment.
func TestDocCommentAddKeyMismatchIsConflict(t *testing.T) {
	conn := newVoteCreateStore(t)
	docID, err := db.CreateDoc(conn, &model.Doc{
		Title: "d", Type: "tdd", Status: "draft", Body: "body", Author: "tester",
	})
	testsupport.Must(t, err, "CreateDoc: %v", err)
	ref := model.FormatDocID(docID)
	const key = "comment-key-1"

	err = runDocCommentAdd(t, ref, "-m", "looks good", "--idempotency-key", key)
	testsupport.Must(t, err, "first doc comment add: %v", err)

	err = runDocCommentAdd(t, ref, "-m", "needs work", "--idempotency-key", key)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrConflict {
		t.Fatalf("doc comment add reusing %q with a different body returned %v, want CONFLICT", key, err)
	}
	if got := output.ExitCodeForError(cmdError.Code); got != output.ExitConflict {
		t.Errorf("exit code = %d, want %d", got, output.ExitConflict)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("conflict %q does not name the key %q", err, key)
	}

	comments, err := db.ListDocComments(conn, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(comments) != 1 {
		t.Errorf("doc has %d comments after the rejected reuse, want 1", len(comments))
	}
}
