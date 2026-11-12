package cli

import (
	"strconv"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// TestRunDocShowRevision_ErrorMapping pins `doc show --rev`'s three-way error
// dispatch: a validation failure (negative revision) reports
// VALIDATION_ERROR, a revision that does not exist reports NOT_FOUND with a
// message naming both the doc and the revision, and a missing doc reaches the
// same NOT_FOUND dispatch through db.GetDocRevision's own existence check.
func TestRunDocShowRevision_ErrorMapping(t *testing.T) {
	conn := newTestDB(t)
	docID := createDoc(t, conn, "doc", "note", "draft")
	ref := model.FormatDocID(docID)

	showRev := func(t *testing.T, docRef string, rev int) error {
		t.Helper()
		cmd := cmdWithDB(conn)
		cmd.Flags().Int("rev", 0, "")
		if err := cmd.Flags().Set("rev", strconv.Itoa(rev)); err != nil {
			t.Fatalf("set --rev: %v", err)
		}
		w, _ := bufWriter(false)
		return runDocShow(cmd, []string{docRef}, w)
	}

	t.Run("negative revision is VALIDATION_ERROR", func(t *testing.T) {
		err := showRev(t, ref, -1)
		if err == nil {
			t.Fatal("doc show --rev -1 error = nil, want VALIDATION_ERROR")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrValidation {
			t.Fatalf("doc show --rev -1 = %#v, want VALIDATION_ERROR *CmdError", err)
		}
	})

	t.Run("missing revision is NOT_FOUND", func(t *testing.T) {
		err := showRev(t, ref, 999)
		if err == nil {
			t.Fatal("doc show --rev 999 error = nil, want NOT_FOUND")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrNotFound {
			t.Fatalf("doc show --rev 999 = %#v, want NOT_FOUND *CmdError", err)
		}
		want := "doc " + ref + " revision 999 not found"
		if cmdErr.Error() != want {
			t.Errorf("Error() = %q, want %q", cmdErr.Error(), want)
		}
	})

	t.Run("missing doc is NOT_FOUND via the same dispatch", func(t *testing.T) {
		err := showRev(t, "DOC-999999", 0)
		if err == nil {
			t.Fatal("doc show DOC-999999 --rev 0 error = nil, want NOT_FOUND")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrNotFound {
			t.Fatalf("doc show DOC-999999 --rev 0 = %#v, want NOT_FOUND *CmdError", err)
		}
	})
}
