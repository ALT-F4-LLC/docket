package cli

import (
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func docLinkedIssueIDs(t *testing.T, st store.Store, docID int) []int {
	t.Helper()
	linked, err := st.HydrateLinkedIssues([]int{docID})
	testsupport.Must(t, err, "HydrateLinkedIssues: %v", err)
	var ids []int
	for _, ref := range linked[docID] {
		ids = append(ids, ref.ID)
	}
	return ids
}

func TestDocLinkAddThenRemove(t *testing.T) {
	conn := newTestDB(t)
	st := store.StoreOn(conn)
	docID := createDoc(t, conn, "linked doc", "spec", "draft")
	issueID := createIssue(t, conn, "linked issue", model.StatusBacklog, model.PriorityMedium)

	run := func(verb *cobra.Command) {
		t.Helper()
		cmd := cmdWithDB(conn)
		cmd.Flags().String("issue", "", "")
		testsupport.Must(t, cmd.Flags().Set("quiet", "true"), "set quiet")
		testsupport.Must(t, cmd.Flags().Set("issue", model.FormatID(issueID)), "set issue")
		err := verb.RunE(cmd, []string{model.FormatDocID(docID)})
		testsupport.Must(t, err, "doc link %s: %v", verb.Name(), err)
	}

	run(docLinkAddCmd)
	if ids := docLinkedIssueIDs(t, st, docID); !slices.Contains(ids, issueID) {
		t.Fatalf("after doc link add, linked issues = %v, want %d included", ids, issueID)
	}

	run(docLinkRemoveCmd)
	if ids := docLinkedIssueIDs(t, st, docID); slices.Contains(ids, issueID) {
		t.Fatalf("after doc link remove, linked issues = %v, want %d excluded", ids, issueID)
	}
}
