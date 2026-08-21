package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-480. Closing an issue on a run's outcome, an operator ran
// `issue move HRN-275 done --note "..."` and got `unknown flag: --note`. Every
// step verb already takes one (`step approve/reject/resolve --note`), so the
// spelling was a fair guess; the working fallback was two verbs — an
// `issue comment add` and then the move — which is two round trips and not
// atomic, so a refused move can leave a comment narrating something that never
// happened.

// issueMoveVerb drives the REAL `issue move` command object, so the flags exercised
// are the ones the binary registers rather than a stand-in a test declared.
// The command is package-global, so every flag it touches is restored —
// Changed included, since `--if-version` is read through Changed rather than
// through its value.
func issueMoveVerb(t *testing.T, conn *sql.DB, args []string, flags map[string]string) error {
	t.Helper()
	moveCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	for name, value := range flags {
		f := moveCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`issue move` has no --%s", name)
		}
		def := f.DefValue
		testsupport.Must(t, moveCmd.Flags().Set(name, value), "setting --%s to %q", name, value)
		t.Cleanup(func() {
			_ = f.Value.Set(def)
			f.Changed = false
		})
	}
	restore := captureStdout(t)
	err := moveCmd.RunE(moveCmd, args)
	restore()
	return err
}

// notesOn returns the comments recorded against an issue.
func notesOn(t *testing.T, conn *sql.DB, id int) []*model.Comment {
	t.Helper()
	comments, err := db.ListComments(conn, id)
	testsupport.Must(t, err, "listing comments: %v", err)
	return comments
}

// noteBodies renders comments for a failure message; the slice itself is
// pointers and prints as addresses.
func noteBodies(comments []*model.Comment) []string {
	bodies := make([]string, 0, len(comments))
	for _, c := range comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// TestIssueMoveNoteRecordsTheReasonWithTheMove is the acceptance case: one
// verb, and both halves land.
func TestIssueMoveNoteRecordsTheReasonWithTheMove(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "closed on a run's outcome", model.StatusTodo, model.PriorityNone)

	const note = "Closed on RUN-40's outcome: the fix landed in the consumer."
	testsupport.Must(t, issueMoveVerb(t, conn, []string{model.FormatID(id), "done"},
		map[string]string{"note": note}), "issue move --note failed")

	issue, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "reading the moved issue: %v", err)
	if issue.Status != model.StatusDone {
		t.Errorf("status = %q, want %q", issue.Status, model.StatusDone)
	}

	comments := notesOn(t, conn, id)
	if len(comments) != 1 {
		t.Fatalf("recorded %d comments, want the one --note carried", len(comments))
	}
	if comments[0].Body != note {
		t.Errorf("comment body = %q, want %q", comments[0].Body, note)
	}
	// The operator's own comment, not an engine-authored one: this replaces a
	// hand-typed `issue comment add`, and must be indistinguishable from it.
	if comments[0].Author != config.DefaultAuthor() {
		t.Errorf("comment author = %q, want the invoking author %q",
			comments[0].Author, config.DefaultAuthor())
	}
}

// TestIssueMoveWithoutNoteRecordsNothing is the unchanged-behavior half: the
// flag is opt-in, and a move that omits it writes exactly what it always did.
func TestIssueMoveWithoutNoteRecordsNothing(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "moved silently", model.StatusTodo, model.PriorityNone)

	testsupport.Must(t, issueMoveVerb(t, conn, []string{model.FormatID(id), "review"}, nil),
		"issue move failed")

	issue, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "reading the moved issue: %v", err)
	if issue.Status != model.StatusReview {
		t.Errorf("status = %q, want %q", issue.Status, model.StatusReview)
	}
	if comments := notesOn(t, conn, id); len(comments) != 0 {
		t.Errorf("a move with no --note recorded %d comments: %q",
			len(comments), noteBodies(comments))
	}
}

// TestIssueMoveNoteIsAtomicWithTheMove is why this is one verb rather than two:
// a refused move records no comment. `--if-version` against a stale version is
// the reachable refusal — the two-verb form would already have written the
// comment by the time the move was rejected.
func TestIssueMoveNoteIsAtomicWithTheMove(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "moved underneath the caller", model.StatusTodo, model.PriorityNone)

	// Bump the version out from under the precondition the caller will pass.
	testsupport.Must(t, db.UpdateIssue(conn, id, map[string]interface{}{"title": "retitled"}, "someone else"),
		"bumping the issue version out from under the caller")

	err := issueMoveVerb(t, conn, []string{model.FormatID(id), "done"},
		map[string]string{"note": "closed on a stale read", "if-version": "1"})
	if err == nil {
		t.Fatal("a stale --if-version moved the issue anyway")
	}
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %s, want CONFLICT", got)
	}

	issue, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "reading the issue: %v", err)
	if issue.Status != model.StatusTodo {
		t.Errorf("status = %q, want the untouched %q", issue.Status, model.StatusTodo)
	}
	if comments := notesOn(t, conn, id); len(comments) != 0 {
		t.Errorf("a refused move left %d orphan comment(s) behind: %q — the note "+
			"and the move must commit together or not at all",
			len(comments), noteBodies(comments))
	}
}

// TestIssueMoveNoteRecordsOnANoOpMove pins the no-op decision: moving an issue
// to the status it already holds still records the reason it was given, because
// the two-verb form this replaces would have.
func TestIssueMoveNoteRecordsOnANoOpMove(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "already done", model.StatusDone, model.PriorityNone)

	const note = "Confirming: closed by RUN-40, no further work."
	testsupport.Must(t, issueMoveVerb(t, conn, []string{model.FormatID(id), "done"},
		map[string]string{"note": note}), "no-op issue move --note failed")

	comments := notesOn(t, conn, id)
	if len(comments) != 1 {
		t.Fatalf("a no-op move with --note recorded %d comments, want 1", len(comments))
	}
	if comments[0].Body != note {
		t.Errorf("comment body = %q, want %q", comments[0].Body, note)
	}
}

// TestIssueMoveNoteRefusedOnProjectMigration: `--project` writes no comment, and
// a note silently discarded is worse than one that never landed — the same
// reasoning `--if-version` on a migration already carries.
func TestIssueMoveNoteRefusedOnProjectMigration(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "filed in the wrong place", model.StatusBacklog, model.PriorityNone)

	err := issueMoveVerb(t, conn, []string{model.FormatID(id)},
		map[string]string{"note": "re-homed", "project": "elsewhere"})
	if err == nil {
		t.Fatal("--note on a project migration was accepted and silently dropped")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want VALIDATION_ERROR", got)
	}
	if comments := notesOn(t, conn, id); len(comments) != 0 {
		t.Errorf("the refused migration still recorded %d comment(s)", len(comments))
	}
}

// TestIssueMoveDocumentsTheNoteFlag is the help half of DKT-480: the flag an
// operator guessed has to be findable without guessing twice.
func TestIssueMoveDocumentsTheNoteFlag(t *testing.T) {
	f := moveCmd.Flags().Lookup("note")
	if f == nil {
		t.Fatal("`issue move` has no --note; the operator who guessed it got `unknown flag`")
	}
	if !strings.Contains(f.Usage, "comment") {
		t.Errorf("--note does not say where the text lands: %q", f.Usage)
	}
	// FlagUsages is the block `--help` prints for this command's own flags;
	// asserting on it rather than on Usage alone pins that the flag is
	// REGISTERED, not merely declared somewhere.
	//
	// (Flags().FlagUsages, not cobra's UsageString: the latter merges the
	// root's persistent flags into this package-global command's flagset as a
	// side effect, and every later test in the package reads that flagset.)
	if usages := moveCmd.Flags().FlagUsages(); !strings.Contains(usages, "--note") {
		t.Errorf("`issue move --help` lists no --note:\n%s", usages)
	}
	if !strings.Contains(moveCmd.Long, "--note") {
		t.Error("`issue move --help` never mentions --note in its long help")
	}
}
