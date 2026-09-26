package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1564 — `step claim` must not exit non-zero while the lease it just took
// stands. A wave executor reads a non-zero exit as "stop, claim nothing", so a
// refusal on a committed claim strands the lease AND the token in one step.

// TestClaimWithAPostCommitFailureExitsZeroCarryingTheToken is AC1 at the CLI
// seam: the claim JSON — token included — is what a caller receives, and the
// failure is reported beside it rather than in place of it.
func TestClaimWithAPostCommitFailureExitsZeroCarryingTheToken(t *testing.T) {
	w, buf := bufWriter(true)

	incomplete := &engine.IncompleteClaimError{
		Result: &engine.ClaimResult{
			Step: "STEP-1", Token: "tok-live", LeaseExpiresMS: 1_800_000_600_000,
			Attempt: 1, RowVersion: 3,
		},
		Err: errors.New("database is locked (5) (SQLITE_BUSY)"),
	}

	if err := emitIncompleteClaim(w, incomplete, "step STEP-1"); err != nil {
		t.Fatalf("a committed claim was reported as a command failure: %v", err)
	}

	var envelope struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Data    struct {
			Step       string `json:"step"`
			Token      string `json:"token"`
			ClaimError string `json:"claim_error"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
		"the response is not a JSON envelope: %q", buf.String())

	if !envelope.OK {
		t.Error("ok = false; a caller branching on .ok would discard the token it holds")
	}
	// wave.js reads exactly this path. It is the whole point of the change.
	if envelope.Data.Token != "tok-live" {
		t.Errorf("data.token = %q, want the committed claim's token", envelope.Data.Token)
	}
	if !strings.Contains(envelope.Data.ClaimError, "SQLITE_BUSY") {
		t.Errorf("data.claim_error = %q, want the post-commit failure reported",
			envelope.Data.ClaimError)
	}
	if !strings.Contains(envelope.Message, "SQLITE_BUSY") {
		t.Errorf("message = %q, want the failure named for a human reader too",
			envelope.Message)
	}
}

// TestClaimFailureBeforeTheLeaseIsStillARefusal keeps the ordinary path: an
// error carrying no committed claim is the refusal it always was.
func TestClaimFailureBeforeTheLeaseIsStillARefusal(t *testing.T) {
	w, buf := bufWriter(true)

	err := emitIncompleteClaim(w, errors.New("step verify@0 is not ready to claim"),
		"step STEP-1")
	if err == nil {
		t.Fatal("a plain claim refusal was reported as a success")
	}
	if buf.Len() != 0 {
		t.Errorf("a refusal wrote a claim envelope: %q", buf.String())
	}
}

// TestStepClaimCommandReMintsForItsOwnOwner is AC2 through the REAL RunE: the
// executor whose token never arrived re-runs the same command and gets a
// working one, instead of the CONFLICT that sent RUN-90 to a reap panel.
func TestStepClaimCommandReMintsForItsOwnOwner(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	_, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)

	var stepID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND step_name = 'first'`, runID,
	).Scan(&stepID)
	testsupport.Must(t, err, "reading the claimable step: %v", err)

	cmd := cmdWithDB(conn)
	cmd.Flags().AddFlagSet(stepClaimCmd.Flags())
	t.Cleanup(func() { _ = stepClaimCmd.Flags().Set("owner", "") })
	testsupport.Must(t, cmd.Flags().Set("owner", "wave:STEP-1"), "setting --owner: %v", err)

	arg := model.FormatStepID(stepID)
	testsupport.Must(t, stepClaimCmd.RunE(cmd, []string{arg}), "the first claim: %v", err)

	before, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	// The retry a stranded executor makes: same verb, same owner, same step.
	testsupport.Must(t, stepClaimCmd.RunE(cmd, []string{arg}),
		"the stranded executor's own re-claim was refused: %v", err)

	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.TokenHash == before.TokenHash {
		t.Error("the re-claim did not re-mint; the executor still has no usable token")
	}
	if after.Attempt != before.Attempt {
		t.Errorf("attempt = %d after the re-mint, want %d", after.Attempt, before.Attempt)
	}
	if after.Status != db.StepClaimed || after.Owner != "wave:STEP-1" {
		t.Errorf("the re-mint disturbed the claim: status=%q owner=%q",
			after.Status, after.Owner)
	}
}

// TestReMintedClaimSaysSoInItsResponse keeps the two outcomes distinguishable:
// a caller must be able to tell a fresh claim from a re-key of the lease it
// already held, without diffing the attempt against a value it may not have.
func TestReMintedClaimSaysSoInItsResponse(t *testing.T) {
	w, buf := bufWriter(true)

	err := emitClaim(w, &engine.ClaimResult{
		Step: "STEP-1", Token: "tok", LeaseExpiresMS: 1_800_000_600_000,
		Attempt: 1, RowVersion: 4, ReMinted: true,
	}, nil)
	testsupport.Must(t, err, "emitClaim: %v", err)

	var envelope struct {
		Data struct {
			ReMinted bool `json:"re_minted"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
		"the response is not a JSON envelope: %q", buf.String())
	if !envelope.Data.ReMinted {
		t.Errorf("data.re_minted is absent from a re-mint: %q", buf.String())
	}

	// An ordinary claim's envelope is UNCHANGED: the field is omitted, not false.
	w, buf = bufWriter(true)
	err = emitClaim(w, &engine.ClaimResult{
		Step: "STEP-1", Token: "tok", LeaseExpiresMS: 1_800_000_600_000, Attempt: 1,
	}, nil)
	testsupport.Must(t, err, "emitClaim: %v", err)
	if strings.Contains(buf.String(), "re_minted") {
		t.Errorf("a clean claim's envelope grew a field: %q", buf.String())
	}
}
