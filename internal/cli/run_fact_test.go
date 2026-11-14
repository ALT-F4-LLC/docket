package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func runFactCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	for _, name := range []string{"kind", "reason", "proposal", "voter", "step", "cause"} {
		cmd.Flags().String(name, "", "")
	}
	return cmd
}

// The run fact and note commands resolve their project through the verb, so
// the root hook must not resolve it for them as well.
func TestRunFactAndNoteCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{runFactAddCmd, runNoteAddCmd, runNoteListCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("%s lacks the verbOwnsProject annotation", cmd.CommandPath())
		}
	}
}

func stepDeferredEvents(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ?`, runID, engine.RunFactStepDeferred,
	).Scan(&n)
	testsupport.Must(t, err, "counting step-deferred events: %v", err)
	return n
}

// A bound run records the fact only under its capability, and the human
// line names the fact, the run, its seq and the reason.
func TestRunFactAddCLIRecordsAStepDeferral(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	token := activateHolding(t, conn, runID, model.NowMS()).ConductorToken
	steps, err := store.StoreOn(conn).ListRunSteps(runID)
	testsupport.Must(t, err, "listing the run's steps: %v", err)
	if len(steps) == 0 {
		t.Fatal("the activated run has no steps")
	}
	runRef := model.FormatRunID(runID)
	addFact := func(step string) (string, error) {
		cmd := runFactCmdWithDB(conn)
		for name, value := range map[string]string{
			"kind": engine.RunFactStepDeferred, "step": step,
			"cause": engine.DeferralBudget, "reason": "over budget",
		} {
			err := cmd.Flags().Set(name, value)
			testsupport.Must(t, err, "setting --%s: %v", name, err)
		}
		w, buf := bufWriter(false)
		err := runRunFactAdd(cmd, runRef, w)
		return buf.String(), err
	}

	_, err = addFact("STEP-x")
	assertCmdCode(t, err, output.ErrValidation, "run fact add --step STEP-x")
	if !strings.Contains(err.Error(), `invalid --step "STEP-x"`) {
		t.Errorf("the refusal %q does not name the malformed --step", err)
	}

	withStdin(t, "")
	t.Setenv(TokenEnvVar, "")
	_, err = addFact(model.FormatStepID(steps[0].ID))
	assertCmdCode(t, err, output.ErrValidation, "run fact add with no token")
	if n := stepDeferredEvents(t, conn, runID); n != 0 {
		t.Errorf("the refusals recorded %d step-deferred event(s), want none", n)
	}

	t.Setenv(TokenEnvVar, token)
	out, err := addFact(model.FormatStepID(steps[0].ID))
	testsupport.Must(t, err, "run fact add with the capability: %v", err)
	if want := fmt.Sprintf("Recorded %s on %s (seq ", engine.RunFactStepDeferred, runRef); !strings.Contains(out, want) ||
		!strings.Contains(out, "): over budget") {
		t.Errorf("output = %q, want %q...): over budget", out, want)
	}
	if n := stepDeferredEvents(t, conn, runID); n != 1 {
		t.Errorf("step-deferred events = %d, want 1", n)
	}
}

// With an unreadable working directory, a malformed request still reports its
// own fault, as the commands did before the move: the run reference, --step
// and --proposal for a fact, the run reference and --authority for a note.
// Only a well-formed request reports the directory.
func TestRunFactAndNoteAddReportTheRequestFaultBeforeTheDirectory(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)
	stubGetwdFailure(t)

	addFact := func(ref string, flags map[string]string) error {
		cmd := runFactCmdWithDB(conn)
		flags["kind"], flags["reason"] = engine.RunFactStepDeferred, "over budget"
		for name, value := range flags {
			err := cmd.Flags().Set(name, value)
			testsupport.Must(t, err, "setting --%s: %v", name, err)
		}
		w, _ := bufWriter(false)
		return runRunFactAdd(cmd, ref, w)
	}
	addNote := func(ref, authority string) error {
		cmd := runNoteCmdNoAuthority(conn)
		for name, value := range map[string]string{"authority": authority, "text": "a ruling"} {
			err := cmd.Flags().Set(name, value)
			testsupport.Must(t, err, "setting --%s: %v", name, err)
		}
		w, _ := bufWriter(false)
		return runRunNoteAdd(cmd, ref, w)
	}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"fact add RUN-x", addFact("RUN-x", map[string]string{}), "RUN-x"},
		{"fact add --step STEP-x", addFact(runRef, map[string]string{"step": "STEP-x"}), `invalid --step "STEP-x"`},
		{"fact add --proposal PROPOSAL-x",
			addFact(runRef, map[string]string{"proposal": "PROPOSAL-x"}), `invalid --proposal "PROPOSAL-x"`},
		{"note add RUN-x", addNote("RUN-x", engine.AuthorityOperator), "RUN-x"},
		{"note add --authority standing-grant", addNote(runRef, "standing-grant"), "--authority"},
	} {
		assertCmdCode(t, tc.err, output.ErrValidation, tc.name)
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: refusal %q does not name %q", tc.name, tc.err, tc.want)
		}
	}

	for name, err := range map[string]error{
		"fact add": addFact(runRef, map[string]string{"step": "STEP-1", "cause": engine.DeferralBudget}),
		"note add": addNote(runRef, engine.AuthorityOperator),
	} {
		assertCmdCode(t, err, output.ErrGeneral, name)
		if !strings.Contains(err.Error(), "working directory could not be resolved") {
			t.Errorf("%s: refusal %q does not name the working directory", name, err)
		}
	}
}
