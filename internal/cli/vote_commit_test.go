package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// runVoteCommit drives the registered `vote commit` RunE on a fresh command
// carrying the verb's real flags.
func runVoteCommit(conn *sql.DB, proposalID int) error {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("outcome", "Committed", "")
	cmd.Flags().String("escalation-reason", "", "")
	return voteCommitCmd.RunE(cmd, []string{model.FormatProposalID(proposalID)})
}

// approvedVoteStepProposal opens a vote step's proposal on a fresh run and
// approves it. With bind set, the run holds a conductor capability and the
// returned token is that capability; otherwise the run is unbound and the
// token is empty.
func approvedVoteStepProposal(t *testing.T, conn *sql.DB, bind bool) (int, string) {
	t.Helper()
	proposalID, err := model.ParseProposalID(seedPinnedVoteProposal(t, conn, pinnedVoteSrc("")))
	testsupport.Must(t, err, "ParseProposalID: %v", err)
	_, err = conn.Exec(`UPDATE proposals SET status = ? WHERE id = ?`,
		string(model.ProposalStatusApproved), proposalID)
	testsupport.Must(t, err, "approving: %v", err)
	if !bind {
		return proposalID, ""
	}
	token, hash, err := model.MintToken()
	testsupport.Must(t, err, "MintToken: %v", err)
	_, err = conn.Exec(`UPDATE runs SET conductor_token_hash = ?`, hash)
	testsupport.Must(t, err, "binding the run: %v", err)
	return proposalID, token
}

func assertProposalStatus(t *testing.T, conn *sql.DB, proposalID int, want model.ProposalStatus) {
	t.Helper()
	p, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.Status != want {
		t.Errorf("status = %s, want %s", p.Status, want)
	}
}

// TestVoteCommitRefusesAVoteStepProposalWithoutTheCapability: on a bound
// run, a commit with nothing on DOCKET_TOKEN or stdin is VALIDATION_ERROR and
// the proposal stays approved.
func TestVoteCommitRefusesAVoteStepProposalWithoutTheCapability(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	withStdin(t, "")
	conn := newTestDB(t)
	proposalID, _ := approvedVoteStepProposal(t, conn, true)

	assertCmdCode(t, runVoteCommit(conn, proposalID), output.ErrValidation, "vote commit with no token")
	assertProposalStatus(t, conn, proposalID, model.ProposalStatusApproved)
}

// TestVoteCommitRefusesATokenThatIsNotTheRunsCapability: a well-formed token
// that is not the run's capability is AUTH_ERROR, the refusal does not echo
// it, and the proposal stays approved.
func TestVoteCommitRefusesATokenThatIsNotTheRunsCapability(t *testing.T) {
	withStdin(t, "")
	conn := newTestDB(t)
	proposalID, _ := approvedVoteStepProposal(t, conn, true)
	wrong, _, err := model.MintToken()
	testsupport.Must(t, err, "MintToken: %v", err)
	t.Setenv(TokenEnvVar, wrong)

	err = runVoteCommit(conn, proposalID)
	assertCmdCode(t, err, output.ErrAuth, "vote commit with a wrong token")
	if strings.Contains(err.Error(), wrong) {
		t.Errorf("the refusal echoes the presented token: %v", err)
	}
	assertProposalStatus(t, conn, proposalID, model.ProposalStatusApproved)
}

// TestVoteCommitCommitsWithTheRunsCapability: the run's own capability on
// DOCKET_TOKEN commits the proposal.
func TestVoteCommitCommitsWithTheRunsCapability(t *testing.T) {
	withStdin(t, "")
	conn := newTestDB(t)
	proposalID, token := approvedVoteStepProposal(t, conn, true)
	t.Setenv(TokenEnvVar, token)

	testsupport.Must(t, runVoteCommit(conn, proposalID), "vote commit with the capability: %v", nil)
	assertProposalStatus(t, conn, proposalID, model.ProposalStatusCommitted)
}

// TestVoteCommitNeedsNoTokenOffTheConductorPath: a conversational proposal,
// and a vote step's proposal on a run that never minted a capability, commit
// with no token.
func TestVoteCommitNeedsNoTokenOffTheConductorPath(t *testing.T) {
	t.Run("conversational proposal", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		withStdin(t, "")
		conn := newTestDB(t)
		proposalID, err := db.CreateProposal(conn, &model.Proposal{
			Description: "an operator's own ballot", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusApproved, RequiredVoters: 1, Threshold: 0.5,
		})
		testsupport.Must(t, err, "CreateProposal: %v", err)

		testsupport.Must(t, runVoteCommit(conn, proposalID), "vote commit: %v", nil)
		assertProposalStatus(t, conn, proposalID, model.ProposalStatusCommitted)
	})

	t.Run("vote step on an unbound run", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		withStdin(t, "")
		conn := newTestDB(t)
		proposalID, _ := approvedVoteStepProposal(t, conn, false)

		testsupport.Must(t, runVoteCommit(conn, proposalID), "vote commit: %v", nil)
		assertProposalStatus(t, conn, proposalID, model.ProposalStatusCommitted)
	})
}
