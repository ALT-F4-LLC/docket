package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestStepHoldParksWithTheConductorToken is DKT-3305: with DOCKET_TOKEN set to
// the run's conductor capability, `step hold STEP-N --reason R` parks the
// ready step; with --reason omitted it is refused VALIDATION_ERROR and the
// step stays ready.
func TestStepHoldParksWithTheConductorToken(t *testing.T) {
	conn := newTestDB(t)
	activatedRunForNext(t, conn) // holds the run's capability in DOCKET_TOKEN
	first := stepIDNamed(t, conn, "first@0")
	status := func() string {
		t.Helper()
		var s string
		testsupport.Must(t, conn.QueryRow(`SELECT status FROM steps WHERE id = ?`, first).Scan(&s),
			"reading status: %v", nil)
		return s
	}

	w, _ := bufWriter(true)
	err := runStepHold(reapCmdWithDB(conn), []string{model.FormatStepID(first)}, w)
	assertCmdCode(t, err, output.ErrValidation, "step hold without --reason")
	if s := status(); s != db.StepPending {
		t.Fatalf("a refused hold left the step %s, want pending", s)
	}

	cmd := reapCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("reason", "keeps failing"), "set --reason: %v", nil)
	w, buf := bufWriter(true)
	testsupport.Must(t, runStepHold(cmd, []string{model.FormatStepID(first)}, w),
		"step hold: %v\n%s", nil, buf.String())
	if s := status(); s != db.StepWaitingHuman {
		t.Errorf("held step is %s, want %s", s, db.StepWaitingHuman)
	}
	data := rulingEvent(t, conn, first, engine.EventStepHeld)
	if data["reason"] != "keeps failing" {
		t.Errorf("step-held reason = %#v, want the --reason", data["reason"])
	}
}
