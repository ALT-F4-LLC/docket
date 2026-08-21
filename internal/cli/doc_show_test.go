package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

// TestRunDocShowRevision_ErrorMapping pins runDocShowRevision's three-way
// error dispatch (doc_show.go): a validation failure (negative revision)
// reports VALIDATION_ERROR, a revision that does not exist reports
// NOT_FOUND with a message naming both the doc and the revision, and a
// missing doc reaches the same NOT_FOUND dispatch through db.GetDocRevision's
// own existence check. This equivalence was previously INFERRED only (no
// coverage at any level); this test makes it observed.
func TestRunDocShowRevision_ErrorMapping(t *testing.T) {
	conn := newTestDB(t)
	docID := createDoc(t, conn, "doc", "note", "draft")

	t.Run("negative revision is VALIDATION_ERROR", func(t *testing.T) {
		w, _ := bufWriter(false)
		err := runDocShowRevision(conn, w, "DOC-1", docID, -1)
		if err == nil {
			t.Fatal("runDocShowRevision(rev=-1) error = nil, want VALIDATION_ERROR")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrValidation {
			t.Fatalf("runDocShowRevision(rev=-1) = %#v, want VALIDATION_ERROR *CmdError", err)
		}
	})

	t.Run("missing revision is NOT_FOUND", func(t *testing.T) {
		w, _ := bufWriter(false)
		err := runDocShowRevision(conn, w, "DOC-1", docID, 999)
		if err == nil {
			t.Fatal("runDocShowRevision(rev=999) error = nil, want NOT_FOUND")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrNotFound {
			t.Fatalf("runDocShowRevision(rev=999) = %#v, want NOT_FOUND *CmdError", err)
		}
		if cmdErr.Error() != "doc DOC-1 revision 999 not found" {
			t.Errorf("Error() = %q, want %q", cmdErr.Error(), "doc DOC-1 revision 999 not found")
		}
	})

	t.Run("missing doc is NOT_FOUND via the same dispatch", func(t *testing.T) {
		w, _ := bufWriter(false)
		err := runDocShowRevision(conn, w, "DOC-999", 999999, 0)
		if err == nil {
			t.Fatal("runDocShowRevision(missing doc) error = nil, want NOT_FOUND")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok || cmdErr.Code != output.ErrNotFound {
			t.Fatalf("runDocShowRevision(missing doc) = %#v, want NOT_FOUND *CmdError", err)
		}
	})
}
