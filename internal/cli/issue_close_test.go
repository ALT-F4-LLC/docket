package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
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
