package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// tripwireReader fails the test the moment anything reads from it. It stands
// in for an agent's inherited stdin pipe that never reaches EOF: a read there
// is not merely wasted, it blocks the process forever.
type tripwireReader struct{ t *testing.T }

func (r tripwireReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read; this path must not touch stdin")
	return 0, nil
}

// TestTokenForCloseSkipsStdinWithoutLiveLease pins the DKT-124 fix: closing
// an issue that no one holds must not read stdin at all. The token fallback
// drains its reader to EOF, so an unconditional read turns `issue close` into
// an indefinite hang under any parent that keeps the stdin pipe open — the
// default in agent harnesses — for a verb whose happy path needs no token.
func TestTokenForCloseSkipsStdinWithoutLiveLease(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	conn := newTestDB(t)

	t.Run("unclaimed", func(t *testing.T) {
		id := createIssue(t, conn, "unclaimed", model.StatusTodo, model.PriorityNone)
		if tok := tokenForClose(conn, id, tripwireReader{t}); tok != "" {
			t.Errorf("token = %q, want empty for an unclaimed issue", tok)
		}
	})

	t.Run("lapsed lease", func(t *testing.T) {
		id := createIssue(t, conn, "lapsed", model.StatusTodo, model.PriorityNone)
		_, _, err := db.ClaimIssue(conn, id, "owner", -1, model.NowMS())
		testsupport.Must(t, err, "ClaimIssue: %v", err)
		if tok := tokenForClose(conn, id, tripwireReader{t}); tok != "" {
			t.Errorf("token = %q, want empty for a lapsed lease", tok)
		}
	})
}

// TestTokenForCloseReadsStdinUnderLiveLease is the control: with a live lease
// the token IS required, so the stdin channel must still work.
func TestTokenForCloseReadsStdinUnderLiveLease(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	conn := newTestDB(t)

	id := createIssue(t, conn, "claimed", model.StatusTodo, model.PriorityNone)
	_, _, err := db.ClaimIssue(conn, id, "owner", 60_000, model.NowMS())
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	if tok := tokenForClose(conn, id, strings.NewReader("piped-token\n")); tok != "piped-token" {
		t.Errorf("token = %q, want piped-token", tok)
	}
}

// seedAbandoned records the resolution `abandon-issue` leaves on an issue,
// through the same write the engine uses.
func seedAbandoned(t *testing.T, conn *sql.DB, id int) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	testsupport.Must(t, db.SetIssueResolutionTx(tx, id, db.IssueResolutionAbandoned), "seeding resolution")
	testsupport.Must(t, tx.Commit(), "commit")
}

// runIssueVerb drives a real, package-global issue command with flags set as
// the binary would parse them, restoring each flag (Changed included) after.
func runIssueVerb(t *testing.T, conn *sql.DB, cmd *cobra.Command, args []string, flags map[string]string) {
	t.Helper()
	cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	for name, value := range flags {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`issue %s` has no --%s", cmd.Name(), name)
		}
		def := f.DefValue
		testsupport.Must(t, cmd.Flags().Set(name, value), "setting --%s to %q", name, value)
		t.Cleanup(func() {
			_ = f.Value.Set(def)
			f.Changed = false
		})
	}
	restore := captureStdout(t)
	err := cmd.RunE(cmd, args)
	restore()
	testsupport.Must(t, err, "issue %s %v: %v", cmd.Name(), args, err)
}

func issueState(t *testing.T, conn *sql.DB, id int) *model.Issue {
	t.Helper()
	issue, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	return issue
}

type issueVerbRow struct {
	name  string
	cmd   *cobra.Command
	args  func(ref string) []string
	flags map[string]string
}

func refOnly(ref string) []string { return []string{ref} }

// TestMovingToDoneClearsStaleResolution pins that every operator path to
// `done` drops an `abandoned` left by an earlier run, so delivered work does
// not read as cancelled. Each verb is its own row: a clear wired into one
// verb alone fails the others.
func TestMovingToDoneClearsStaleResolution(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	conn := newTestDB(t)

	rows := []issueVerbRow{
		{"close", closeCmd, refOnly, nil},
		{"move done", moveCmd, func(ref string) []string { return []string{ref, "done"} }, nil},
		{"edit -s done", editCmd, refOnly, map[string]string{"status": "done"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			id := createIssue(t, conn, r.name, model.StatusTodo, model.PriorityNone)
			seedAbandoned(t, conn, id)

			runIssueVerb(t, conn, r.cmd, r.args(model.FormatID(id)), r.flags)

			got := issueState(t, conn, id)
			if got.Status != model.StatusDone {
				t.Errorf("status = %q, want done", got.Status)
			}
			if got.Resolution != "" {
				t.Errorf("resolution = %q, want empty after an operator moved the issue to done", got.Resolution)
			}
		})
	}
}

// TestNonDoneStatusChangeKeepsResolution is the counterpart: abandoned on an
// open issue is intended, so moving it anywhere but done leaves it alone.
func TestNonDoneStatusChangeKeepsResolution(t *testing.T) {
	conn := newTestDB(t)

	rows := []issueVerbRow{
		{"move in-progress", moveCmd, func(ref string) []string { return []string{ref, "in-progress"} }, nil},
		{"edit -s backlog", editCmd, refOnly, map[string]string{"status": "backlog"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			id := createIssue(t, conn, r.name, model.StatusTodo, model.PriorityNone)
			seedAbandoned(t, conn, id)

			runIssueVerb(t, conn, r.cmd, r.args(model.FormatID(id)), r.flags)

			if got := issueState(t, conn, id).Resolution; got != db.IssueResolutionAbandoned {
				t.Errorf("resolution = %q, want %q kept on a non-done status change", got, db.IssueResolutionAbandoned)
			}
		})
	}
}

// TestAlreadyDoneKeepsResolution pins the machine case: an issue already at
// done that a routing marked abandoned keeps that record when an operator
// re-asserts done, since no transition into done happened.
func TestAlreadyDoneKeepsResolution(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	conn := newTestDB(t)

	rows := []issueVerbRow{
		{"edit -s done", editCmd, refOnly, map[string]string{"status": "done"}},
		{"move done --note", moveCmd, func(ref string) []string { return []string{ref, "done"} }, map[string]string{"note": "x"}},
		{"close", closeCmd, refOnly, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			id := createIssue(t, conn, r.name, model.StatusDone, model.PriorityNone)
			seedAbandoned(t, conn, id)
			before, err := db.GetVersion(conn, "issues", id)
			testsupport.Must(t, err, "GetVersion: %v", err)

			runIssueVerb(t, conn, r.cmd, r.args(model.FormatID(id)), r.flags)

			if got := issueState(t, conn, id).Resolution; got != db.IssueResolutionAbandoned {
				t.Errorf("resolution = %q, want %q kept on an issue already done", got, db.IssueResolutionAbandoned)
			}
			if r.cmd != closeCmd {
				return
			}
			after, err := db.GetVersion(conn, "issues", id)
			testsupport.Must(t, err, "GetVersion: %v", err)
			if after != before {
				t.Errorf("version %d -> %d, want close on a done issue to write nothing", before, after)
			}
		})
	}
}
