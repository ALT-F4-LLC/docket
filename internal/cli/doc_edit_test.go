package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func latestDocRevision(t *testing.T, st store.Store, docID int) int {
	t.Helper()
	revs, err := st.ListDocRevisions(docID)
	testsupport.Must(t, err, "ListDocRevisions: %v", err)
	latest := 0
	for _, r := range revs {
		if r.RevisionNumber > latest {
			latest = r.RevisionNumber
		}
	}
	return latest
}

func TestDocEditUpdatesStoredBodyTitleAndRevision(t *testing.T) {
	conn := newTestDB(t)
	st := store.StoreOn(conn)
	docID := createDoc(t, conn, "old title", "spec", "draft")
	revBefore := latestDocRevision(t, st, docID)

	cmd := cmdWithDB(conn)
	cmd.Flags().String("title", "", "")
	cmd.Flags().String("description", "", "")
	cmd.Flags().String("type", "", "")
	cmd.Flags().String("status", "", "")
	testsupport.Must(t, cmd.Flags().Set("quiet", "true"), "set quiet")
	testsupport.Must(t, cmd.Flags().Set("title", "new title"), "set title")
	testsupport.Must(t, cmd.Flags().Set("description", "new body"), "set description")

	err := docEditCmd.RunE(cmd, []string{model.FormatDocID(docID)})
	testsupport.Must(t, err, "doc edit: %v", err)

	doc, err := st.GetDoc(docID)
	testsupport.Must(t, err, "GetDoc: %v", err)
	if doc.Title != "new title" {
		t.Errorf("title = %q, want %q", doc.Title, "new title")
	}
	if doc.Body != "new body" {
		t.Errorf("body = %q, want %q", doc.Body, "new body")
	}
	if revAfter := latestDocRevision(t, st, docID); revAfter <= revBefore {
		t.Errorf("latest revision = %d after edit, want greater than %d", revAfter, revBefore)
	}
}
