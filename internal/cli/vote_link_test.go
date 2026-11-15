package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// runVoteLinkCmd runs cmd for proposal with --issue issue under --json=v2,
// returning the envelope's proposal and issue ids.
func runVoteLinkCmd(t *testing.T, conn *sql.DB, cmd *cobra.Command, cfg *config.Config, proposal, issue string) (voteLinkResult, error) {
	t.Helper()
	testsupport.Must(t, cmd.Flags().Set("issue", issue), "set --issue")
	out, err := runLeaseCommandOutput(t, conn, cfg, cmd, proposal)
	if err != nil {
		return voteLinkResult{}, err
	}
	var envelope struct {
		Data voteLinkResult `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	return envelope.Data, nil
}

// TestVoteLinkAndUnlinkReportThePair: link writes the link and reports the
// pair, a second link is a conflict, unlink removes it and reports the pair,
// and a second unlink finds no link.
func TestVoteLinkAndUnlinkReportThePair(t *testing.T) {
	conn := newTestDB(t)
	pid := listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2)
	issue := createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone)
	proposal, issueRef := model.FormatProposalID(pid), model.FormatID(issue)
	want := voteLinkResult{ProposalID: proposal, IssueID: issueRef}

	got, err := runVoteLinkCmd(t, conn, voteLinkCmd, nil, proposal, issueRef)
	testsupport.Must(t, err, "vote link: %v", err)
	if got != want {
		t.Errorf("vote link = %+v, want %+v", got, want)
	}
	linked, err := db.GetProposalIssues(conn, pid)
	testsupport.Must(t, err, "GetProposalIssues: %v", err)
	if !reflect.DeepEqual(linked, []int{issue}) {
		t.Errorf("linked issues = %v, want [%d]", linked, issue)
	}

	_, err = runVoteLinkCmd(t, conn, voteLinkCmd, nil, proposal, issueRef)
	requireCmdErrCode(t, err, output.ErrConflict, "link already exists")

	got, err = runVoteLinkCmd(t, conn, voteUnlinkCmd, nil, proposal, issueRef)
	testsupport.Must(t, err, "vote unlink: %v", err)
	if got != want {
		t.Errorf("vote unlink = %+v, want %+v", got, want)
	}
	linked, err = db.GetProposalIssues(conn, pid)
	testsupport.Must(t, err, "GetProposalIssues: %v", err)
	if len(linked) != 0 {
		t.Errorf("linked issues after unlink = %v, want none", linked)
	}

	_, err = runVoteLinkCmd(t, conn, voteUnlinkCmd, nil, proposal, issueRef)
	requireCmdErrCode(t, err, output.ErrNotFound, "link not found")
}

func TestVoteLinkCommandsRefuseBadReferences(t *testing.T) {
	conn := newTestDB(t)
	proposal := model.FormatProposalID(listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2))
	issue := model.FormatID(createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone))

	for _, cmd := range []*cobra.Command{voteLinkCmd, voteUnlinkCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			_, err := runVoteLinkCmd(t, conn, cmd, nil, "nope", issue)
			requireCmdErrCode(t, err, output.ErrValidation, "invalid proposal ID")
			_, err = runVoteLinkCmd(t, conn, cmd, nil, proposal, "nope")
			requireCmdErrCode(t, err, output.ErrValidation, "invalid issue ID")
		})
	}
	_, err := runVoteLinkCmd(t, conn, voteLinkCmd, nil, "DKT-V999", issue)
	requireCmdErrCode(t, err, output.ErrNotFound, "proposal or issue not found")
}

// TestVoteLinkCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before vote link and vote
// unlink resolved their own project.
func TestVoteLinkCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	proposal := model.FormatProposalID(listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2))
	issue := model.FormatID(createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone))

	_, err = runVoteLinkCmd(t, conn, voteLinkCmd, &config.Config{Identity: "/repos/link", Anchored: true}, proposal, issue)
	testsupport.Must(t, err, "vote link: %v", err)
	_, err = runVoteLinkCmd(t, conn, voteUnlinkCmd, &config.Config{Identity: "/repos/unlink", Anchored: true}, proposal, issue)
	testsupport.Must(t, err, "vote unlink: %v", err)

	want := map[string]string{"/repos/link": "vote link", "/repos/unlink": "vote unlink"}
	if got := registrationEvents(t, conn); !reflect.DeepEqual(got, want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
}

// TestVoteLinkCommandsRefuseAnUnanchoredCaller: link and unlink are writes
// into the caller's project, so an identity that names none is refused and
// the links stay as they were.
func TestVoteLinkCommandsRefuseAnUnanchoredCaller(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	unanchored := &config.Config{Identity: "/work/scratch"}
	requireLinks := func(pid int, want []int) {
		t.Helper()
		linked, err := db.GetProposalIssues(conn, pid)
		testsupport.Must(t, err, "GetProposalIssues: %v", err)
		if !slices.Equal(linked, want) {
			t.Errorf("after a refused write the links are %v, want %v", linked, want)
		}
	}

	pid := listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2)
	issueID := createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone)
	proposal, issue := model.FormatProposalID(pid), model.FormatID(issueID)

	_, err = runVoteLinkCmd(t, conn, voteLinkCmd, unanchored, proposal, issue)
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")
	requireLinks(pid, nil)

	_, err = runVoteLinkCmd(t, conn, voteLinkCmd, nil, proposal, issue)
	testsupport.Must(t, err, "vote link: %v", err)
	_, err = runVoteLinkCmd(t, conn, voteUnlinkCmd, unanchored, proposal, issue)
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")
	requireLinks(pid, []int{issueID})
}

// runVoteHuman runs cmd with args in human mode and returns its stdout.
func runVoteHuman(t *testing.T, conn *sql.DB, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		cmd.Flags().Visit(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})
	cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	stdout := captureStdout(t)
	err := cmd.RunE(cmd, args)
	return stdout(), err
}

// TestVoteLinkCommandsReportThePairToAHuman: link and unlink each name the
// issue and the proposal on their human line.
func TestVoteLinkCommandsReportThePairToAHuman(t *testing.T) {
	conn := newTestDB(t)
	proposal := model.FormatProposalID(listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2))
	issue := model.FormatID(createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone))

	for _, tc := range []struct {
		cmd  *cobra.Command
		want string
	}{
		{voteLinkCmd, "Linked " + issue + " to proposal " + proposal},
		{voteUnlinkCmd, "Unlinked " + issue + " from proposal " + proposal},
	} {
		testsupport.Must(t, tc.cmd.Flags().Set("issue", issue), "set --issue")
		out, err := runVoteHuman(t, conn, tc.cmd, proposal)
		testsupport.Must(t, err, "vote %s: %v", tc.cmd.Name(), err)
		if !strings.Contains(out, tc.want) {
			t.Errorf("vote %s printed %q, want it to contain %q", tc.cmd.Name(), out, tc.want)
		}
	}
}
