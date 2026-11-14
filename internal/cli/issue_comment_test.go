package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
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

// TestIssueCommentAddOwnsItsProject: issue comment add resolves its project
// through internal/verb, so the root hook must skip its eager resolution.
func TestIssueCommentAddOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(commentAddCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", commentAddCmd.CommandPath())
	}
}

// TestIssueCommentAddRefusesBeforeAskingForTheBody: a comment that cannot be
// added is refused before the body is read. A JSON-mode run with no message
// shows the order: each refusal is reported, never the missing message.
func TestIssueCommentAddRefusesBeforeAskingForTheBody(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	id := createIssue(t, conn, "subject", model.StatusTodo, model.PriorityNone)

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		issue  string
		code   output.ErrorCode
		prefix string
	}{
		{"unanchored caller", &config.Config{Identity: "/work/scratch"}, model.FormatID(id),
			output.ErrValidation, "cannot tell which project this belongs to"},
		{"malformed issue ID", nil, "not-an-issue", output.ErrValidation, "invalid issue ID: "},
		{"missing issue", nil, "DKT-9999", output.ErrNotFound, "issue DKT-9999 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStdin(t, "")
			_, err := runLeaseCommandOutput(t, conn, tc.cfg, commentAddCmd, tc.issue)
			var cmdError *CmdError
			if !errors.As(err, &cmdError) || cmdError.Code != tc.code ||
				!strings.HasPrefix(cmdError.Error(), tc.prefix) {
				t.Errorf("issue comment add %s with no message returned %v, want %s %q",
					tc.issue, err, tc.code, tc.prefix)
			}
		})
	}
	if comments := notesOn(t, conn, id); len(comments) != 0 {
		t.Errorf("issue has %d comments after refusals, want none", len(comments))
	}
}

// TestIssueCommentAddRecordsTheProjectItRegisters keeps the project-registered
// event the root hook wrote before issue comment add resolved its own project,
// including when the comment is refused after the project is registered.
func TestIssueCommentAddRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	ref := model.FormatID(createIssue(t, conn, "subject", model.StatusTodo, model.PriorityNone))
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, commentAddCmd.Flags().Set("message", "hello"), "set --message")
	out, err := runLeaseCommandOutput(t, conn, anchored("/repos/comment"), commentAddCmd, ref)
	testsupport.Must(t, err, "issue comment add: %v", err)
	var added struct {
		Data struct {
			Body   string `json:"body"`
			Author string `json:"author"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &added), "decoding %q", out)
	if added.Data.Body != "hello" || added.Data.Author != config.DefaultAuthor() {
		t.Errorf("added comment = %+v, want body hello by the default author", added.Data)
	}

	testsupport.Must(t, commentAddCmd.Flags().Set("message", "hello"), "set --message")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/missing"), commentAddCmd, "DKT-9999")
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Fatalf("issue comment add DKT-9999 returned %v, want NOT_FOUND", err)
	}

	want := map[string]string{
		"/repos/comment": "issue comment add",
		"/repos/missing": "issue comment add",
	}
	got := registrationEvents(t, conn)
	if len(got) != len(want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
	for identity, verb := range want {
		if got[identity] != verb {
			t.Errorf("registration of %s attributed to %q, want %q", identity, got[identity], verb)
		}
	}
}
