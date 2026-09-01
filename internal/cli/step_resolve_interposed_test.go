package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-861 at the CLI boundary. internal/engine/dkt861_test.go proves the
// refusal, the acknowledgment, and the no-interposed regression guard against
// resolveStep directly; what this file asserts is the VERB — that
// `step resolve --as override-pass` without --drop-interposed surfaces the
// engine's refusal as a validation error carrying the DKT-470 sentence, that
// `--drop-interposed` reaches the engine and prints that same sentence as a
// stderr warning BEFORE the resolution commits, and that a step with no
// interposed dependents still resolves with no flag and no warning.

// interposedResolveSrc is dkt470_test.go's shape: a verify step whose
// threshold interposes a tribunal vote, parked `waiting-human` on its own
// single-attempt failure.
const interposedResolveSrc = `
[pipeline]
name = "cli-interpose"
version = 1

[match]
kind = ["task"]

[[step]]
name = "verify"
executor = "verify"
emits = "report"
max_attempts = 1
on_fail = "waiting-human"
threshold = { "tribunal" = "any(status == blocked)" }

[[step]]
name = "tribunal"
after = ["verify"]
type = "vote"
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"
`

// plainResolveSrc is the same park with NO threshold at all — the AC3 shape.
const plainResolveSrc = `
[pipeline]
name = "cli-plain"
version = 1

[match]
kind = ["task"]

[[step]]
name = "verify"
executor = "verify"
emits = "report"
max_attempts = 1
on_fail = "waiting-human"
`

// parkedResolveRun registers src, activates a one-task run over it, and fails
// verify@0 into `waiting-human` — the state `resolve --as override-pass` is
// reached for.
func parkedResolveRun(t *testing.T, conn *sql.DB, src string) (stepID int) {
	t.Helper()

	registerForRun(t, conn, src)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "park me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID),
		"adding issue to run: %v", err)
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)

	err = conn.QueryRow(
		`SELECT id FROM steps WHERE instance = 'verify@0'`).Scan(&stepID)
	testsupport.Must(t, err, "finding verify@0: %v", err)

	claim, err := engine.ClaimStep(conn, stepID,
		engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, engine.NewEngine().FailStep(
		conn, stepID, claim.Token, "gate failed", "", model.NowMS()), "fail: %v", err)

	var status string
	err = conn.QueryRow(
		`SELECT status FROM steps WHERE id = ?`, stepID).Scan(&status)
	testsupport.Must(t, err, "reading status: %v", err)
	if status != string(db.StepWaitingHuman) {
		t.Fatalf("premise: verify@0 = %q, want %q", status, db.StepWaitingHuman)
	}
	return stepID
}

// resolveCmdWithDB builds a `step resolve` command with its real flag names.
func resolveCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("as", "", "")
	cmd.Flags().String("note", "", "")
	cmd.Flags().Bool("batch", false, "")
	cmd.Flags().Bool("drop-interposed", false, "")
	return cmd
}

func stepStatusByInstance(t *testing.T, conn *sql.DB, instance string) string {
	t.Helper()
	var status string
	err := conn.QueryRow(
		`SELECT status FROM steps WHERE instance = ?`, instance).Scan(&status)
	testsupport.Must(t, err, "reading status of %s: %v", instance, err)
	return status
}

// TestResolveOverridePassRefusesInterposedWithoutFlag: without the
// acknowledgment the verb refuses as a validation error, the refusal carries
// the DKT-470 sentence and names the flag, and nothing mutates.
func TestResolveOverridePassRefusesInterposedWithoutFlag(t *testing.T) {
	conn := newTestDB(t)
	stepID := parkedResolveRun(t, conn, interposedResolveSrc)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{Stdout: outBuf, Stderr: errBuf}

	err := runStepResolve(cmd, []string{fmt.Sprintf("STEP-%d", stepID)}, w)
	if err == nil {
		t.Fatal("override-pass with interposed dependents and no " +
			"--drop-interposed was accepted")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want %s", got, output.ErrValidation)
	}
	for _, want := range []string{
		"tribunal", "will NOT be routed", "--drop-interposed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}
	if got := stepStatusByInstance(t, conn, "verify@0"); got != string(db.StepWaitingHuman) {
		t.Errorf("verify@0 = %q after the refusal, want it still %q",
			got, db.StepWaitingHuman)
	}
	if got := stepStatusByInstance(t, conn, "tribunal@0"); got != string(db.StepPending) {
		t.Errorf("tribunal@0 = %q after the refusal, want it still %q",
			got, db.StepPending)
	}
}

// TestResolveOverridePassDropInterposedWarnsAndProceeds: the acknowledged
// resolution prints the same DKT-470 sentence as a warning — emitted before
// the resolution commits — and then behaves exactly as override-pass always
// has: verify passes, the interposed tribunal is skipped.
func TestResolveOverridePassDropInterposedWarnsAndProceeds(t *testing.T) {
	conn := newTestDB(t)
	stepID := parkedResolveRun(t, conn, interposedResolveSrc)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("drop-interposed", "true"),
		"set --drop-interposed: %v", nil)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{Stdout: outBuf, Stderr: errBuf}

	err := runStepResolve(cmd, []string{fmt.Sprintf("STEP-%d", stepID)}, w)
	testsupport.Must(t, err, "acknowledged override-pass: %v", err)

	for _, want := range []string{"tribunal", "will NOT be routed"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr = %q, want the pre-mutation warning to contain %q",
				errBuf.String(), want)
		}
	}
	if got := stepStatusByInstance(t, conn, "verify@0"); got != string(db.StepDone) {
		t.Errorf("verify@0 = %q, want %q", got, db.StepDone)
	}
	if got := stepStatusByInstance(t, conn, "tribunal@0"); got != string(db.StepSkipped) {
		t.Errorf("tribunal@0 = %q, want %q — the acknowledged pass still "+
			"skips the interposed step, exactly as today", got, db.StepSkipped)
	}
}

// TestResolveOverridePassNoInterposedUnchanged is AC3 at the verb: a step
// with no interposed dependents resolves with no flag, no warning, and no
// refusal — exactly the bytes it always produced.
func TestResolveOverridePassNoInterposedUnchanged(t *testing.T) {
	conn := newTestDB(t)
	stepID := parkedResolveRun(t, conn, plainResolveSrc)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{Stdout: outBuf, Stderr: errBuf}

	err := runStepResolve(cmd, []string{fmt.Sprintf("STEP-%d", stepID)}, w)
	testsupport.Must(t, err, "override-pass with no interposed dependents: %v", err)

	if errBuf.Len() != 0 {
		t.Errorf("stderr = %q, want no warning for a step with no interposed "+
			"dependents", errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "Resolved") {
		t.Errorf("stdout = %q, want the ordinary resolution message", outBuf.String())
	}
	if got := stepStatusByInstance(t, conn, "verify@0"); got != string(db.StepDone) {
		t.Errorf("verify@0 = %q, want %q", got, db.StepDone)
	}
}
