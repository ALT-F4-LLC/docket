package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueReopenThroughRootReportsTheReopen: a reopen run the way an
// operator runs it returns a closed issue to backlog without its abandonment,
// and a reopen of an open issue echoes it without writing.
func TestIssueReopenThroughRootReportsTheReopen(t *testing.T) {
	conn := newVoteCreateStore(t)
	id := createIssue(t, conn, "revisited", model.StatusDone, model.PriorityNone)
	seedAbandoned(t, conn, id)
	ref := model.FormatID(id)

	out, err := runThroughRoot(t, reopenCmd, "issue", "reopen", ref)
	testsupport.Must(t, err, "issue reopen: %v", err)
	if want := "Reopened " + ref + ": revisited"; !strings.Contains(out, want) {
		t.Errorf("output %q does not report %q", out, want)
	}
	got := issueState(t, conn, id)
	if got.Status != model.StatusBacklog || got.Resolution != "" {
		t.Errorf("status/resolution = %q/%q, want backlog with no resolution", got.Status, got.Resolution)
	}

	before, err := db.GetVersion(conn, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	out, err = runThroughRoot(t, reopenCmd, "issue", "reopen", ref, "--json")
	testsupport.Must(t, err, "second issue reopen: %v", err)
	if got := issueStatusEchoed(t, out); got != model.StatusBacklog {
		t.Errorf("echoed status = %q, want backlog", got)
	}
	if after, _ := db.GetVersion(conn, "issues", id); after != before {
		t.Errorf("version %d -> %d, want a reopen of an open issue to write nothing", before, after)
	}
}
