package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

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
	setVoteBackfillFlags(t, flags...)
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

// setVoteBackfillFlags resets vote backfill-usage's flags to their defaults,
// then sets flags as name, value pairs.
func setVoteBackfillFlags(t *testing.T, flags ...string) {
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

// TestVoteBackfillUsageReportsTheBatchToAHuman: the human line counts the
// rows and seats under the source they carry.
func TestVoteBackfillUsageReportsTheBatchToAHuman(t *testing.T) {
	conn := newTestDB(t)
	ref := model.FormatProposalID(castProposal(t, conn, "seat-a", "seat-b"))
	setVoteBackfillFlags(t, "source", "relay",
		"voter", "seat-a", "unit", "tokens", "quantity", "1",
		"voter", "seat-b", "unit", "tokens", "quantity", "2")

	out, err := runVoteHuman(t, conn, voteBackfillUsageCmd, ref)
	testsupport.Must(t, err, "vote backfill-usage: %v", err)
	if want := `Back-filled 2 usage row(s) across 2 seat(s) of ` + ref + ` as "relay"`; !strings.Contains(out, want) {
		t.Errorf("vote backfill-usage printed %q, want it to contain %q", out, want)
	}
}

// TestVoteBackfillUsageResolvesTheProjectFirst: the project is settled
// before the proposal and the batch, as when the root hook resolved it. An
// unanchored caller is refused whatever else is wrong and nothing is
// written; an anchored caller's project registers even when its batch
// cannot be read.
func TestVoteBackfillUsageResolvesTheProjectFirst(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	pid := castProposal(t, conn, "seat-a")
	ref := model.FormatProposalID(pid)
	unanchored := &config.Config{Identity: "/work/scratch"}
	missing := filepath.Join(t.TempDir(), "absent.json")

	_, err = runVoteBackfill(t, conn, unanchored, ref, "voter", "seat-a", "unit", "tokens", "quantity", "1")
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")
	if got := usageSources(t, conn, pid); len(got["seat-a"]) != 0 {
		t.Errorf("a refused back-fill wrote usage %v", got)
	}
	_, err = runVoteBackfill(t, conn, unanchored, ref, "from-json", missing)
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")
	_, err = runVoteBackfill(t, conn, unanchored, "nope", "from-json", missing)
	requireCmdErrCode(t, err, output.ErrValidation, "cannot tell which project")

	_, err = runVoteBackfill(t, conn, &config.Config{Identity: "/repos/backfill", Anchored: true}, ref,
		"from-json", backfillBatch(t, "not a batch"))
	requireCmdErrCode(t, err, output.ErrValidation, "reading the back-fill batch")
	want := map[string]string{"/repos/backfill": "vote backfill-usage"}
	if got := registrationEvents(t, conn); !reflect.DeepEqual(got, want) {
		t.Errorf("registration events = %v, want %v", got, want)
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
