package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// TestVoteBackfillUsageOwnsItsProject: vote backfill-usage resolves its
// project through internal/verb, so the root hook must skip its eager
// resolution for it.
func TestVoteBackfillUsageOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(voteBackfillUsageCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", voteBackfillUsageCmd.CommandPath())
	}
}

// castProposal opens a proposal and casts one ballot per voter on it.
func castProposal(t *testing.T, conn *sql.DB, voters ...string) int {
	t.Helper()
	id := listedProposal(t, conn, "spent", model.ProposalStatusOpen, len(voters)+1)
	for _, voter := range voters {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: id, VoterName: voter, Verdict: model.VerdictApprove,
			Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", voter, err)
	}
	return id
}

// runVoteBackfill runs vote backfill-usage on proposal under --json=v2 with
// flags set, returning the decoded outcome.
func runVoteBackfill(t *testing.T, conn *sql.DB, cfg *config.Config, proposal string, flags ...string) (voteBackfillOutcome, error) {
	t.Helper()
	reset := func() {
		voteBackfillUsageCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		})
	}
	// Each call starts from the defaults, and the cleanup, registered before
	// the harness's own, runs after it: that one resets a slice flag by
	// Set(DefValue), which appends.
	reset()
	t.Cleanup(reset)
	for i := 0; i < len(flags); i += 2 {
		testsupport.Must(t, voteBackfillUsageCmd.Flags().Set(flags[i], flags[i+1]), "set --%s", flags[i])
	}
	out, err := runLeaseCommandOutput(t, conn, cfg, voteBackfillUsageCmd, proposal)
	if err != nil {
		return voteBackfillOutcome{}, err
	}
	var envelope struct {
		Data voteBackfillOutcome `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	return envelope.Data, nil
}

// backfillBatch writes a --from-json batch file and returns its path.
func backfillBatch(t *testing.T, batch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "batch.json")
	testsupport.Must(t, os.WriteFile(path, []byte(batch), 0o600), "writing the batch")
	return path
}

// usageSources reads each cast's back-filled units and their sources.
func usageSources(t *testing.T, conn *sql.DB, proposalID int) map[string]map[string]string {
	t.Helper()
	byVote, err := db.GetProposalVoteUsageSources(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVoteUsageSources: %v", err)
	votes, err := db.GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	out := map[string]map[string]string{}
	for _, v := range votes {
		out[v.VoterName] = byVote[v.ID]
	}
	return out
}

// TestVoteBackfillUsageReportsTheBatch: a JSON batch lands on each seat's
// cast under the default source, and the outcome counts its rows and seats;
// the flag form carries a named source.
func TestVoteBackfillUsageReportsTheBatch(t *testing.T) {
	conn := newTestDB(t)
	pid := castProposal(t, conn, "seat-a", "seat-b")
	ref := model.FormatProposalID(pid)

	got, err := runVoteBackfill(t, conn, nil, ref, "from-json", backfillBatch(t, `[
		{"voter": "seat-a", "unit": "input_tokens", "quantity": 10},
		{"voter": "seat-a", "unit": "output_tokens", "quantity": 20},
		{"voter": "seat-b", "unit": "input_tokens", "quantity": 30}]`))
	testsupport.Must(t, err, "vote backfill-usage --from-json: %v", err)
	if want := (voteBackfillOutcome{Proposal: ref, Rows: 3, Seats: 2, Source: "backfilled"}); got != want {
		t.Errorf("outcome = %+v, want %+v", got, want)
	}

	got, err = runVoteBackfill(t, conn, nil, ref,
		"voter", "seat-b", "unit", "output_tokens", "quantity", "40", "source", "relay")
	testsupport.Must(t, err, "vote backfill-usage --voter: %v", err)
	if want := (voteBackfillOutcome{Proposal: ref, Rows: 1, Seats: 1, Source: "relay"}); got != want {
		t.Errorf("outcome = %+v, want %+v", got, want)
	}

	want := map[string]map[string]string{
		"seat-a": {"input_tokens": "backfilled", "output_tokens": "backfilled"},
		"seat-b": {"input_tokens": "backfilled", "output_tokens": "relay"},
	}
	if got := usageSources(t, conn, pid); !reflect.DeepEqual(got, want) {
		t.Errorf("usage sources = %v, want %v", got, want)
	}
}

func TestVoteBackfillUsageRefusals(t *testing.T) {
	conn := newTestDB(t)
	ref := model.FormatProposalID(castProposal(t, conn, "seat-a"))
	missing := filepath.Join(t.TempDir(), "absent.json")

	// The proposal is refused before the batch is read.
	_, err := runVoteBackfill(t, conn, nil, "nope", "from-json", missing)
	requireCmdErrCode(t, err, output.ErrValidation, "invalid proposal ID")

	_, err = runVoteBackfill(t, conn, nil, ref, "from-json", missing)
	requireCmdErrCode(t, err, output.ErrNotFound, "reading the back-fill batch")

	_, err = runVoteBackfill(t, conn, nil, "DKT-V999",
		"voter", "seat-a", "unit", "tokens", "quantity", "1")
	requireCmdErrCode(t, err, output.ErrNotFound, "proposal DKT-V999 not found")

	_, err = runVoteBackfill(t, conn, nil, ref,
		"voter", "seat-never", "unit", "tokens", "quantity", "1")
	if code := codeOf(t, err); code != output.ErrValidation {
		t.Errorf("a seat that never cast returned %s (%v), want %s", code, err, output.ErrValidation)
	}
}

// TestVoteBackfillUsageRecordsTheProjectItRegisters keeps the
// project-registered event the root hook wrote before vote backfill-usage
// resolved its own project.
func TestVoteBackfillUsageRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	ref := model.FormatProposalID(castProposal(t, conn, "seat-a"))

	_, err = runVoteBackfill(t, conn, &config.Config{Identity: "/repos/backfill", Anchored: true}, ref,
		"voter", "seat-a", "unit", "tokens", "quantity", "1")
	testsupport.Must(t, err, "vote backfill-usage: %v", err)

	want := map[string]string{"/repos/backfill": "vote backfill-usage"}
	if got := registrationEvents(t, conn); !reflect.DeepEqual(got, want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
}
