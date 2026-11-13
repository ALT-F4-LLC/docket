package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestVoteWriteCommandsOwnTheirProject: vote create, vote cast and vote close
// resolve their project through internal/verb, so the root hook must skip its
// eager resolution for them.
func TestVoteWriteCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{voteCreateCmd, voteCastCmd, voteCloseCmd} {
		if _, ok := cmd.Annotations["verbOwnsProject"]; !ok {
			t.Errorf("%s has no verbOwnsProject annotation", cmd.CommandPath())
		}
	}
}

// TestVoteCloseCommandReportsTheClosure drives `vote close` through the real
// root command: the envelope names the proposal, its closed status and the
// reason, and the row is closed with the reason as its outcome.
func TestVoteCloseCommandReportsTheClosure(t *testing.T) {
	conn := newVoteCreateStore(t)
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description: "superseded", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 2, Threshold: 0.5,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	ref := model.FormatProposalID(id)

	out, err := runRootVerb(t, voteCloseCmd, "vote", "close", ref, "--reason", "decided out of band", "--json")
	testsupport.Must(t, err, "vote close: %v", err)

	var envelope struct {
		Data struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			FinalOutcome string `json:"final_outcome"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	d := envelope.Data
	if d.ID != ref || d.Status != "closed" || d.FinalOutcome != "decided out of band" || d.UpdatedAt == "" {
		t.Errorf("envelope data = %+v, want %s closed with its reason and a timestamp", d, ref)
	}

	p, err := db.GetProposal(conn, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.Status != model.ProposalStatusClosed || p.FinalOutcome != "decided out of band" {
		t.Errorf("proposal after close = %s %q, want closed with the reason", p.Status, p.FinalOutcome)
	}
}

// TestVoteCloseCommandRefusesWithoutAReason: the refusal is VALIDATION_ERROR
// naming the flag, and the proposal stays open.
func TestVoteCloseCommandRefusesWithoutAReason(t *testing.T) {
	conn := newVoteCreateStore(t)
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description: "still open", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 2, Threshold: 0.5,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	_, err = runRootVerb(t, voteCloseCmd, "vote", "close", model.FormatProposalID(id), "--json")
	if code := codeOf(t, err); code != output.ErrValidation {
		t.Fatalf("vote close without --reason returned %s (%v), want %s", code, err, output.ErrValidation)
	}
	if !strings.Contains(err.Error(), "--reason is required") {
		t.Errorf("refusal %q does not name --reason", err)
	}
	p, err := db.GetProposal(conn, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.Status != model.ProposalStatusOpen {
		t.Errorf("a refused close left the proposal %s", p.Status)
	}
}
