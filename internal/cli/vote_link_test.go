package cli

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestVoteLinkCommandsOwnTheirProject: vote link and vote unlink resolve
// their project through internal/verb, so the root hook must skip its eager
// resolution for them.
func TestVoteLinkCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{voteLinkCmd, voteUnlinkCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("%s lacks the verbOwnsProject annotation", cmd.CommandPath())
		}
	}
}

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

// TestVoteLinkCommandsRefuseAnUnanchoredCaller: a link is a write into the
// caller's project, so an identity that names none is refused and nothing is
// linked.
func TestVoteLinkCommandsRefuseAnUnanchoredCaller(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	pid := listedProposal(t, conn, "linked", model.ProposalStatusOpen, 2)
	issue := model.FormatID(createIssue(t, conn, "linked issue", model.StatusTodo, model.PriorityNone))

	_, err = runVoteLinkCmd(t, conn, voteLinkCmd, &config.Config{Identity: "/work/scratch"}, model.FormatProposalID(pid), issue)
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")
	linked, err := db.GetProposalIssues(conn, pid)
	testsupport.Must(t, err, "GetProposalIssues: %v", err)
	if len(linked) != 0 {
		t.Errorf("a refused link wrote %v", linked)
	}
}
