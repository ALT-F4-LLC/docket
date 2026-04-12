package cli

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
	"os"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
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
func unsetNoColor(t *testing.T) {
	t.Helper()
	old, ok := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatalf("Unsetenv(NO_COLOR): %v", err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv("NO_COLOR", old)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})
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
