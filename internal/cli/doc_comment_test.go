package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
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

// docCommentRow is the JSON shape both doc comment commands emit for one
// comment.
type docCommentRow struct {
	ID     int    `json:"id"`
	DocID  string `json:"doc_id"`
	Body   string `json:"body"`
	Author string `json:"author"`
}

// TestDocCommentAddReturnsTheStoredComment: `doc comment add` on an existing
// doc emits the stored comment's id and body, and that row is what the store
// now holds for the doc.
func TestDocCommentAddReturnsTheStoredComment(t *testing.T) {
	conn := newVoteCreateStore(t)
	docID, err := db.CreateDoc(conn, &model.Doc{
		Title: "d", Type: "tdd", Status: "draft", Body: "body", Author: "tester",
	})
	testsupport.Must(t, err, "CreateDoc: %v", err)
	ref := model.FormatDocID(docID)

	stdout := captureStdout(t)
	err = runDocCommentAdd(t, ref, "-m", "looks good")
	out := stdout()
	testsupport.Must(t, err, "doc comment add: %v", err)

	var env struct {
		Data docCommentRow `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	stored, err := db.ListDocComments(conn, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(stored) != 1 {
		t.Fatalf("doc has %d comments after add, want 1", len(stored))
	}
	if env.Data.ID != stored[0].ID || env.Data.ID == 0 {
		t.Errorf("add returned comment id %d, want the stored id %d", env.Data.ID, stored[0].ID)
	}
	if env.Data.Body != "looks good" || stored[0].Body != "looks good" {
		t.Errorf("add returned body %q, stored %q; want %q", env.Data.Body, stored[0].Body, "looks good")
	}
	if env.Data.DocID != ref {
		t.Errorf("add returned doc_id %q, want %q", env.Data.DocID, ref)
	}
	if author := config.DefaultAuthor(); env.Data.Author != author || stored[0].Author != author {
		t.Errorf("add returned author %q, stored %q; want the default author %q",
			env.Data.Author, stored[0].Author, author)
	}
}

// TestDocCommentAddOwnsItsProject: doc comment add resolves its project
// through internal/verb, so the root hook must skip its eager resolution.
func TestDocCommentAddOwnsItsProject(t *testing.T) {
	if _, ok := docCommentAddCmd.Annotations["verbOwnsProject"]; !ok {
		t.Errorf("%s has no verbOwnsProject annotation", docCommentAddCmd.CommandPath())
	}
}

// TestDocCommentAddUnknownDocIsNotFound: adding a comment to a doc that does
// not exist is NOT_FOUND naming the doc, and stores nothing.
func TestDocCommentAddUnknownDocIsNotFound(t *testing.T) {
	newVoteCreateStore(t)
	err := runDocCommentAdd(t, "DOC-999", "-m", "hello")
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Fatalf("doc comment add DOC-999 returned %v, want NOT_FOUND", err)
	}
	if !strings.Contains(err.Error(), "doc DOC-999 not found") {
		t.Errorf("error %q does not name the missing doc", err)
	}
}

// TestDocCommentAddRefusesBeforeAskingForTheBody: a comment that cannot be
// added is refused before the body is read, so nobody types a comment that is
// then thrown away. A JSON-mode run with no message shows the order: each
// refusal is reported, never the missing message.
func TestDocCommentAddRefusesBeforeAskingForTheBody(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	docID := createDoc(t, conn, "subject", "note", "draft")
	ref := model.FormatDocID(docID)

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		doc    string
		code   output.ErrorCode
		prefix string
	}{
		{"unanchored caller", &config.Config{Identity: "/work/scratch"}, ref,
			output.ErrValidation, "cannot tell which project this belongs to"},
		{"malformed doc ID", nil, "not-a-doc", output.ErrValidation, "invalid doc ID: "},
		{"missing doc", nil, "DOC-999", output.ErrNotFound, "doc DOC-999 not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStdin(t, "")
			_, err := runLeaseCommandOutput(t, conn, tc.cfg, docCommentAddCmd, tc.doc)
			var cmdError *CmdError
			if !errors.As(err, &cmdError) || cmdError.Code != tc.code ||
				!strings.HasPrefix(cmdError.Error(), tc.prefix) {
				t.Errorf("doc comment add %s with no message returned %v, want %s %q",
					tc.doc, err, tc.code, tc.prefix)
			}
		})
	}
	if comments, err := db.ListDocComments(conn, docID); err != nil || len(comments) != 0 {
		t.Errorf("comments = (%d, %v), want none", len(comments), err)
	}
}

// TestDocCommentAddRecordsTheProjectItRegisters keeps the project-registered
// event the root hook wrote before doc comment add resolved its own project,
// including when the comment is refused after the project is registered.
func TestDocCommentAddRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	ref := model.FormatDocID(createDoc(t, conn, "subject", "note", "draft"))
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, docCommentAddCmd.Flags().Set("message", "hello"), "set --message")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/comment"), docCommentAddCmd, ref)
	testsupport.Must(t, err, "doc comment add: %v", err)
	testsupport.Must(t, docCommentAddCmd.Flags().Set("message", "hello"), "set --message")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/missing"), docCommentAddCmd, "DOC-999")
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Fatalf("doc comment add DOC-999 returned %v, want NOT_FOUND", err)
	}

	want := map[string]string{
		"/repos/comment": "doc comment add",
		"/repos/missing": "doc comment add",
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

// TestDocCommentListReturnsTheDocsComments: `doc comment list` emits every
// comment on the doc and none from another doc.
func TestDocCommentListReturnsTheDocsComments(t *testing.T) {
	conn := newTestDB(t)
	docID := createDoc(t, conn, "d", "tdd", "draft")
	otherID := createDoc(t, conn, "other", "tdd", "draft")
	want := map[int]string{}
	for _, body := range []string{"first", "second"} {
		id, err := db.CreateDocComment(conn, &model.DocComment{DocID: docID, Body: body, Author: "tester"})
		testsupport.Must(t, err, "CreateDocComment(%q): %v", body, err)
		want[id] = body
	}
	_, err := db.CreateDocComment(conn, &model.DocComment{DocID: otherID, Body: "elsewhere", Author: "tester"})
	testsupport.Must(t, err, "CreateDocComment(other): %v", err)

	w, buf := bufWriter(true)
	err = runDocCommentList(cmdWithDB(conn), []string{model.FormatDocID(docID)}, w)
	testsupport.Must(t, err, "runDocCommentList: %v", err)

	var env struct {
		Data []docCommentRow `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(env.Data) != 2 {
		t.Fatalf("list returned %d comments, want 2:\n%s", len(env.Data), buf.String())
	}
	for _, got := range env.Data {
		body, ok := want[got.ID]
		if !ok || got.Body != body || got.DocID != model.FormatDocID(docID) {
			t.Errorf("listed comment {id %d, body %q, doc %q}; want one of %v on %s",
				got.ID, got.Body, got.DocID, want, model.FormatDocID(docID))
		}
		delete(want, got.ID)
	}
}

// TestDocCommentListUnknownDocIsNotFound: listing comments on a doc that does
// not exist is NOT_FOUND rather than an empty list.
func TestDocCommentListUnknownDocIsNotFound(t *testing.T) {
	conn := newTestDB(t)
	w, _ := bufWriter(true)
	err := runDocCommentList(cmdWithDB(conn), []string{"DOC-999"}, w)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Fatalf("doc comment list DOC-999 returned %v, want NOT_FOUND", err)
	}
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
