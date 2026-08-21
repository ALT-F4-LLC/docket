package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestParseVoteMetadata pins what `vote cast --metadata` accepts and what it
// refuses, and that every refusal names the flag — the caster reads the
// message on a command line and needs to know which flag to fix.
func TestParseVoteMetadata(t *testing.T) {
	oversized := rawBagOfEncodedSize(t, db.VoteMetadataMaxBytes+1)

	cases := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{"an unset flag asserts nothing", "", nil},
		{"an object decodes whole", `{"a":"b","n":2,"nested":{"k":true}}`, map[string]any{
			"a":      "b",
			"n":      float64(2),
			"nested": map[string]any{"k": true},
		}},
		// An explicitly empty object is a caller saying "no keys", and it
		// stores as NULL like any other empty bag.
		{"an empty object decodes empty", `{}`, map[string]any{}},
		// The at-cap accept. Without it nothing tells `>` from `>=`, and a cap
		// one byte tighter than the constant says would pass every refusal
		// case below.
		{"exactly at the cap is accepted", rawBagOfEncodedSize(t, db.VoteMetadataMaxBytes), map[string]any{
			"blob": strings.Repeat("x", db.VoteMetadataMaxBytes-len(`{"blob":""}`)),
		}},
		// The cap measures the ENCODED bag, the quantity the column measures —
		// not the caller's raw text. Whitespace is insignificant to JSON, so a
		// padded bag far larger than the cap encodes to nine bytes and is
		// accepted, exactly as the column would accept it.
		{"whitespace padding is not part of the size", "{" + strings.Repeat(" ", db.VoteMetadataMaxBytes) + `"a":1}`, map[string]any{
			"a": float64(1),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVoteMetadata(tc.raw)
			if err != nil {
				t.Fatalf("parseVoteMetadata(%q) = error %v, want a bag", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseVoteMetadata(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}

	refusals := []struct {
		name string
		raw  string
	}{
		{"a JSON array is not a bag", `["a","b"]`},
		{"a bare scalar is not a bag", `"a"`},
		{"null is not a bag", `null`},
		{"malformed JSON", `{"a":`},
		{"one byte over the cap", oversized},
		// Escaping grows the encoded bag: encoding/json writes `<` as the six
		// bytes <, so a bag well under the cap as raw text is over it as
		// stored. The flag must refuse what the column would refuse.
		{"escaping pushes an under-cap bag over", `{"a":"` + strings.Repeat("<", db.VoteMetadataMaxBytes/2) + `"}`},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVoteMetadata(tc.raw)
			if err == nil {
				t.Fatalf("parseVoteMetadata accepted %.40q, returning %#v", tc.raw, got)
			}
			if !strings.Contains(err.Error(), "--metadata") {
				t.Errorf("refusal %q does not name the flag", err)
			}
		})
	}
}

// rawBagOfEncodedSize returns the JSON TEXT of a one-key bag that encodes to
// exactly n bytes — the quantity both the flag and the column measure. Sizing
// the filler by hand is how the previous fixture came to be eleven bytes over
// a cap it described as one over.
func rawBagOfEncodedSize(t *testing.T, n int) string {
	t.Helper()
	const shape = `{"blob":""}`
	if n < len(shape) {
		t.Fatalf("cannot build a %d-byte bag: the empty shape is already %d", n, len(shape))
	}
	raw := `{"blob":"` + strings.Repeat("x", n-len(shape)) + `"}`
	if len(raw) != n {
		t.Fatalf("fixture is %d bytes, want %d", len(raw), n)
	}
	return raw
}

// TestVoteCastCommandStoresTheMetadataFlag drives the REAL command — cobra
// parsing, RunE, db.CastVote — and reads the vote back out of the store. It
// exists because everything either side of the join was already pinned and the
// join itself was not: parseVoteMetadata is tested as a pure function and
// CastVote at the db seam, so a RunE that looked up a flag name nobody
// registers would silently drop every caster's claim with the whole suite
// green.
//
// The no-claim seat is the control: it proves the assertion reads this vote's
// own cell rather than anything the command happens to leave lying around.
func TestVoteCastCommandStoresTheMetadataFlag(t *testing.T) {
	conn := newTestDB(t)

	proposalID, err := db.CreateProposal(conn, &model.Proposal{
		Description:    "flag to storage",
		RequiredVoters: 2,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	id := model.FormatProposalID(proposalID)

	claim := `{"resolved":"yes","effort":"high"}`
	err = runVoteCastCmd(t, conn, id, "--voter", "seat-claim", "--verdict", "approve",
		"--confidence", "0.9", "--domain-relevance", "0.8", "--metadata", claim)
	testsupport.Must(t, err, "vote cast with --metadata: %v", err)

	err = runVoteCastCmd(t, conn, id, "--voter", "seat-silent", "--verdict", "approve",
		"--confidence", "0.9", "--domain-relevance", "0.8")
	testsupport.Must(t, err, "vote cast without --metadata: %v", err)

	votes, err := db.GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	byVoter := map[string]*model.Vote{}
	for _, v := range votes {
		byVoter[v.VoterName] = v
	}

	var want map[string]any
	err = json.Unmarshal([]byte(claim), &want)
	testsupport.Must(t, err, "decoding the fixture: %v", err)

	got, ok := byVoter["seat-claim"]
	if !ok {
		t.Fatalf("no vote stored for seat-claim (stored %d votes)", len(votes))
	}
	if !reflect.DeepEqual(got.Metadata, want) {
		t.Errorf("seat-claim metadata = %#v, want %#v — the flag never reached the store",
			got.Metadata, want)
	}

	silent, ok := byVoter["seat-silent"]
	if !ok {
		t.Fatalf("no vote stored for seat-silent (stored %d votes)", len(votes))
	}
	if silent.Metadata != nil {
		t.Errorf("seat-silent metadata = %#v, want nil (the flag was not passed)", silent.Metadata)
	}
}

// TestVoteCastCommandRefusesAnOverCapBag pins the refusal at the command, not
// only at the helper: an over-cap bag fails the invocation and writes no vote.
func TestVoteCastCommandRefusesAnOverCapBag(t *testing.T) {
	conn := newTestDB(t)

	proposalID, err := db.CreateProposal(conn, &model.Proposal{
		Description:    "flag refusal",
		RequiredVoters: 2,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	err = runVoteCastCmd(t, conn, model.FormatProposalID(proposalID),
		"--voter", "seat-a", "--verdict", "approve",
		"--confidence", "0.9", "--domain-relevance", "0.8",
		"--metadata", rawBagOfEncodedSize(t, db.VoteMetadataMaxBytes+1))
	if err == nil {
		t.Fatal("vote cast accepted an over-cap --metadata bag")
	}
	if !strings.Contains(err.Error(), "--metadata") {
		t.Errorf("refusal %q does not name the flag", err)
	}

	votes, err := db.GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	if len(votes) != 0 {
		t.Errorf("a refused invocation stored %d vote(s)", len(votes))
	}
}

// runVoteCastCmd drives a FRESH `vote cast` through cobra's own arg parsing,
// the way a shell invocation would, per runActivateViaCLI's pattern
// (run_test.go). Building through newVoteCastCmd — the same factory the
// registered command uses — means a flag that stopped being registered fails
// here with "unknown flag" instead of a hand-built stand-in flag set silently
// testing nothing.
func runVoteCastCmd(t *testing.T, conn *sql.DB, args ...string) error {
	t.Helper()
	cmd := newVoteCastCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	// --json and --quiet are persistent flags rootCmd normally supplies; this
	// instance is parentless, so register the ones RunE reads. Human mode is
	// deliberate: it is the path that reaches the interactive form, so a
	// required flag that stopped being parsed hangs or refuses here.
	cmd.Flags().String("json", "", "")
	cmd.Flags().Bool("quiet", false, "")
	cmd.SetArgs(args)
	ctx := context.WithValue(context.Background(), dbKey, conn)
	return cmd.ExecuteContext(ctx)
}
