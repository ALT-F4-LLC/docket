package cli

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// newVoteCreateStore initializes a scratch store under DOCKET_PATH, so the real
// root command opens the same database the returned connection reads.
func newVoteCreateStore(t *testing.T) *sql.DB {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	err := os.MkdirAll(docketDir, 0o755)
	testsupport.Must(t, err, "creating the store directory: %v", err)
	t.Setenv("DOCKET_PATH", docketDir)

	conn, err := db.Open(filepath.Join(docketDir, "issues.db"))
	testsupport.Must(t, err, "opening: %v", err)
	t.Cleanup(func() { conn.Close() })
	testsupport.Must(t, db.Initialize(conn), "Initialize: %v", err)
	testsupport.Must(t, db.Migrate(conn), "Migrate: %v", err)
	return conn
}

// runVoteCreate drives `docket vote create <args> --json` through the real
// root command. Cobra keeps flag values across Execute calls, so every flag
// goes back to its default afterwards and no later invocation inherits this
// one's --idempotency-key or --json.
func runVoteCreate(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetArgs(append([]string{"vote", "create", "--json"}, args...))
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		voteCreateCmd.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	return rootCmd.Execute()
}

// countProposals is the store's proposal row count.
func countProposals(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&n)
	testsupport.Must(t, err, "counting proposals: %v", err)
	return n
}

// `vote create --sealed` is the conversational counterpart of
// `vote.rule.<name>.sealed` (DKT-2447): a proposal a person or a harness opens
// by hand can be sealed the same way a workflow vote step's is. Driven through
// the real root command so the flag, the store hook and the create path are
// all exercised.
func TestVoteCreateSealedFlagStoresTheFlag(t *testing.T) {
	conn := newVoteCreateStore(t)

	err := runVoteCreate(t, "--description", "Sealed by hand", "--voters", "2", "--sealed")
	testsupport.Must(t, err, "vote create --sealed: %v", err)

	created, err := db.GetProposal(conn, 1)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if !created.Sealed || created.Description != "Sealed by hand" {
		t.Errorf("created proposal = sealed:%v %q, want sealed \"Sealed by hand\"",
			created.Sealed, created.Description)
	}
}

// TestVoteCreateRefusesVoteStepIdempotencyKey: the `vote-step:` key family is
// the engine's link from a workflow vote step to its proposal. A caller key in
// that family is refused as VALIDATION_ERROR before anything is written, so it
// can neither insert a proposal nor claim the key.
func TestVoteCreateRefusesVoteStepIdempotencyKey(t *testing.T) {
	conn := newVoteCreateStore(t)
	const key = "vote-step:1:1:review@0"

	err := runVoteCreate(t, "--description", "planted", "--voters", "1",
		"--idempotency-key", key)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrValidation {
		t.Fatalf("vote create with a vote-step key returned %v, want VALIDATION_ERROR", err)
	}
	if !strings.Contains(err.Error(), "vote-step:") {
		t.Errorf("refusal %q does not name the reserved prefix", err)
	}
	if n := countProposals(t, conn); n != 0 {
		t.Errorf("proposals = %d after the refusal, want 0", n)
	}
	_, found, err := db.LookupIdempotencyKey(conn, db.ScopeVoteCreate, key)
	testsupport.Must(t, err, "LookupIdempotencyKey: %v", err)
	if found {
		t.Errorf("the refused key %q was recorded anyway", key)
	}
}

// TestVoteCreateReplaysCallerKeys: keys outside the engine's vote-step family
// keep their replay contract. That includes `reap-ack:`, which a conductor
// opens through `vote create` so the engine's acknowledgment can find it.
func TestVoteCreateReplaysCallerKeys(t *testing.T) {
	for _, key := range []string{"my-key", "reap-ack:1:7"} {
		t.Run(key, func(t *testing.T) {
			conn := newVoteCreateStore(t)
			args := []string{"--description", "by hand", "--voters", "1", "--idempotency-key", key}

			err := runVoteCreate(t, args...)
			testsupport.Must(t, err, "first vote create: %v", err)
			first, found, err := db.LookupIdempotencyKey(conn, db.ScopeVoteCreate, key)
			testsupport.Must(t, err, "LookupIdempotencyKey: %v", err)
			if !found {
				t.Fatalf("key %q was not recorded by the first create", key)
			}

			err = runVoteCreate(t, args...)
			testsupport.Must(t, err, "repeat vote create: %v", err)
			again, _, err := db.LookupIdempotencyKey(conn, db.ScopeVoteCreate, key)
			testsupport.Must(t, err, "LookupIdempotencyKey: %v", err)
			if again != first || countProposals(t, conn) != 1 {
				t.Errorf("repeat with key %q: proposal %d of %d rows, want the original %d and 1 row",
					key, again, countProposals(t, conn), first)
			}
		})
	}
}

// TestVoteCreateReportsKeyReuseAsConflict: a key reused with a different
// description is CONFLICT, exit 4, with a message naming the key.
func TestVoteCreateReportsKeyReuseAsConflict(t *testing.T) {
	conn := newVoteCreateStore(t)
	const key = "reused-key"

	err := runVoteCreate(t, "--description", "first", "--voters", "1", "--idempotency-key", key)
	testsupport.Must(t, err, "first vote create: %v", err)

	err = runVoteCreate(t, "--description", "second", "--voters", "1", "--idempotency-key", key)
	code := codeOf(t, err)
	if code != output.ErrConflict || output.ExitCodeForError(code) != 4 {
		t.Fatalf("reuse with a different description returned code %s (exit %d), want %s (exit 4)",
			code, output.ExitCodeForError(code), output.ErrConflict)
	}
	if !strings.Contains(err.Error(), key) {
		t.Errorf("conflict %q does not name the key %q", err, key)
	}
	if n := countProposals(t, conn); n != 1 {
		t.Errorf("proposals = %d after the conflict, want 1", n)
	}
}

// TestVoteStepAdoptsNoPlantedProposal: a caller who runs `vote create` with a
// step's vote-step key before phase 2 does not get the step to adopt their
// ballot. The engine opens its own proposal, created by "docket" at the step
// rule's threshold.
func TestVoteStepAdoptsNoPlantedProposal(t *testing.T) {
	conn := newVoteCreateStore(t)
	// seedPinnedVoteProposal seeds run 1, issue 1 and instance review@0 in a
	// fresh store; the assertion on the step's key below confirms the plant
	// aimed at the key the engine actually uses.
	const planted = "vote-step:1:1:review@0"

	err := runVoteCreate(t, "--description", "planted", "--voters", "1",
		"--threshold", "0.1", "--created-by", "mallory", "--idempotency-key", planted)
	if err == nil {
		t.Errorf("the plant with key %q was accepted", planted)
	}

	ref := seedPinnedVoteProposal(t, conn, pinnedVoteSrc(``))
	id, err := model.ParseProposalID(ref)
	testsupport.Must(t, err, "ParseProposalID: %v", err)

	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, id)
	testsupport.Must(t, err, "IdempotencyKeyOf: %v", err)
	if !found || key != planted {
		t.Fatalf("the step's proposal %s is keyed %q (found %v), want %q",
			ref, key, found, planted)
	}
	adopted, err := db.GetProposal(conn, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if adopted.CreatedBy != "docket" || adopted.Threshold != 0.6 {
		t.Errorf("the step adopted a proposal created by %q at threshold %v, want \"docket\" at 0.6",
			adopted.CreatedBy, adopted.Threshold)
	}
}
