package cli

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	testsupport.Must(t, err, "Open(:memory:): %v", err)
	t.Cleanup(func() { conn.Close() })
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)
	return conn
}

func cmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Bool("quiet", false, "")
	cmd.Flags().Bool("watch", false, "")
	cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	return cmd
}

func bufWriter(jsonMode bool) (*output.Writer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	w := &output.Writer{JSONMode: jsonMode, Stdout: buf, Stderr: &bytes.Buffer{}}
	return w, buf
}

func createIssue(t *testing.T, conn *sql.DB, title string, status model.Status, priority model.Priority) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:    title,
		Status:   status,
		Priority: priority,
		Kind:     model.IssueKindFeature,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", title, err)
	}
	return id
}

func createDoc(t *testing.T, conn *sql.DB, title, typ, status string) int {
	t.Helper()
	id, err := db.CreateDoc(conn, &model.Doc{
		Title:  title,
		Type:   typ,
		Status: status,
		Body:   "body",
		Author: "tester",
	})
	if err != nil {
		t.Fatalf("CreateDoc(%q): %v", title, err)
	}
	return id
}

func linkDocIssue(t *testing.T, conn *sql.DB, docID, issueID int) {
	t.Helper()
	if err := db.LinkDocIssue(conn, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue(%d,%d): %v", docID, issueID, err)
	}
}

// holdConductor puts a run's conductor capability where the seven operator
// verbs read it (DKT-2465): DOCKET_TOKEN, for the rest of the test. A fixture
// that activates a run and then rules on it passes the token activation
// returned, the way a real conductor session holds it.
func holdConductor(t *testing.T, token string) {
	t.Helper()
	if token == "" {
		t.Fatal("holdConductor: activation returned no conductor token")
	}
	t.Setenv(TokenEnvVar, token)
}

// activateHolding activates a run and holds the capability it minted.
func activateHolding(t *testing.T, conn *sql.DB, runID int, nowMS int64) *engine.ActivateResult {
	t.Helper()
	result, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)
	holdConductor(t, result.ConductorToken)
	return result
}

// seatConductor binds a run to a token the test chooses and holds it — for a
// run activated through a path that did not hand the token back to the test.
func seatConductor(t *testing.T, conn *sql.DB, runID int) string {
	t.Helper()
	const token = "the-test-holds-this-run"
	_, err := conn.Exec(`UPDATE runs SET conductor_token_hash = ? WHERE id = ?`,
		model.HashToken(token), runID)
	testsupport.Must(t, err, "seating the test conductor: %v", err)
	t.Setenv(TokenEnvVar, token)
	return token
}
