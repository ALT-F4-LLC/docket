package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// runDocDeleteCmd drives a FRESH `doc delete` through cobra's own arg
// parsing, per runImportCmd's pattern: newDocDeleteCmd is the same factory
// the registered command uses, so the flags under test are the flags a real
// invocation parses.
func runDocDeleteCmd(t *testing.T, conn *sql.DB, args ...string) error {
	t.Helper()
	cmd := newDocDeleteCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	// --json and --quiet are persistent flags rootCmd normally supplies; this
	// instance is parentless, so register the ones RunE reads.
	cmd.Flags().String("json", "", "")
	cmd.Flags().Bool("quiet", false, "")
	cmd.SetArgs(args)
	ctx := context.WithValue(context.Background(), dbKey, conn)
	return cmd.ExecuteContext(ctx)
}

// TestDocDeleteJSONRequiresForce pins DKT-27: `doc delete --json` without
// --force must refuse with VALIDATION_ERROR and delete nothing. An
// output-format flag must never double as consent for a destructive verb —
// the same rule DKT-15 established for `import --replace`.
func TestDocDeleteJSONRequiresForce(t *testing.T) {
	conn := newTestDB(t)
	docID, err := db.CreateDoc(conn, &model.Doc{Title: "keep me"})
	testsupport.Must(t, err, "CreateDoc: %v", err)
	id := model.FormatDocID(docID)

	err = runDocDeleteCmd(t, conn, id, "--json=v1")
	if err == nil {
		t.Fatal("doc delete --json without --force deleted unconfirmed")
	}
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrValidation {
		t.Fatalf("error = %v, want a VALIDATION_ERROR CmdError", err)
	}

	if _, err := db.GetDoc(conn, docID); err != nil {
		t.Fatalf("the refused delete still removed %s: %v", id, err)
	}

	// --force is the consent, in JSON mode too: the delete now proceeds.
	err = runDocDeleteCmd(t, conn, id, "--json=v1", "--force")
	testsupport.Must(t, err, "doc delete --json --force: %v", err)
	if _, err := db.GetDoc(conn, docID); err == nil {
		t.Fatalf("doc delete --json --force left %s in place", id)
	}
}
