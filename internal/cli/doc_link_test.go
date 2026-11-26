package cli

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestDocLinkAddEmitsTheLinkedPair: in JSON mode, doc link add emits the doc
// and issue ids it linked.
func TestDocLinkAddEmitsTheLinkedPair(t *testing.T) {
	conn := newTestDB(t)
	docID := createDoc(t, conn, "linked doc", "spec", "draft")
	issueID := createIssue(t, conn, "linked issue", model.StatusBacklog, model.PriorityMedium)

	cmd := cmdWithDB(conn)
	cmd.Flags().String("issue", "", "")
	testsupport.Must(t, cmd.Flags().Set("json", "true"), "set json")
	testsupport.Must(t, cmd.Flags().Set("issue", model.FormatID(issueID)), "set issue")

	stdout := captureStdout(t)
	err := docLinkAddCmd.RunE(cmd, []string{model.FormatDocID(docID)})
	out := stdout()
	testsupport.Must(t, err, "doc link add: %v", err)

	var env struct {
		Data docLinkResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	want := docLinkResult{DocID: model.FormatDocID(docID), IssueID: model.FormatID(issueID)}
	if env.Data != want {
		t.Errorf("doc link add emitted %+v, want %+v", env.Data, want)
	}
}

// TestDocLinkRemoveEmitsTheUnlinkedPair: in JSON mode, doc link remove emits
// the doc and issue ids it unlinked.
func TestDocLinkRemoveEmitsTheUnlinkedPair(t *testing.T) {
	conn := newTestDB(t)
	docID := createDoc(t, conn, "linked doc", "spec", "draft")
	issueID := createIssue(t, conn, "linked issue", model.StatusBacklog, model.PriorityMedium)
	testsupport.Must(t, store.StoreOn(conn).LinkDocIssue(docID, issueID), "LinkDocIssue")

	testsupport.Must(t, docLinkRemoveCmd.Flags().Set("issue", model.FormatID(issueID)), "set --issue")
	out, err := runLeaseCommandOutput(t, conn, nil, docLinkRemoveCmd, model.FormatDocID(docID))
	testsupport.Must(t, err, "doc link remove: %v", err)

	var env struct {
		Data docLinkResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	want := docLinkResult{DocID: model.FormatDocID(docID), IssueID: model.FormatID(issueID)}
	if env.Data != want {
		t.Errorf("doc link remove emitted %+v, want %+v", env.Data, want)
	}
}

// TestDocLinkCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before doc link add and remove
// resolved their own project, including for a remove refused after it
// registered the project.
func TestDocLinkCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	docRef := model.FormatDocID(createDoc(t, conn, "linked doc", "spec", "draft"))
	issueRef := model.FormatID(createIssue(t, conn, "linked issue", model.StatusBacklog, model.PriorityMedium))
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, docLinkAddCmd.Flags().Set("issue", issueRef), "set --issue")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/link"), docLinkAddCmd, docRef)
	testsupport.Must(t, err, "doc link add: %v", err)
	testsupport.Must(t, docLinkRemoveCmd.Flags().Set("issue", issueRef), "set --issue")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/unlink"), docLinkRemoveCmd, docRef)
	testsupport.Must(t, err, "doc link remove: %v", err)
	testsupport.Must(t, docLinkRemoveCmd.Flags().Set("issue", issueRef), "set --issue")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/refused"), docLinkRemoveCmd, docRef)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Fatalf("removing a missing link returned %v, want NOT_FOUND", err)
	}

	want := map[string]string{
		"/repos/link":    "doc link add",
		"/repos/unlink":  "doc link remove",
		"/repos/refused": "doc link remove",
	}
	got := registrationEvents(t, conn)
	if len(got) != len(want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
	for identity, verb := range want {
		if got[identity] != verb {
			t.Errorf("registration of %s attributed to %q, want %q", identity, got[identity], verb)
		}
	}
}
