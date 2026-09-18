package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestRunReportKeepsRoutingAndResolutionApart is DKT-1898's report half.
//
// A resolved park reported ONE string where there are two facts: the engine's
// reason for parking and the operator's resolution. The resolution overwrote
// the reason, so the report could say a step was skipped but never why it had
// stopped — recovering that meant reading gate rows and vote tallies back.
func TestRunReportKeepsRoutingAndResolutionApart(t *testing.T) {
	conn := newTestDB(t)
	id := parkedStep(t, conn)

	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "reading the parked step: %v", err)
	parkReason := step.ParkReason
	if parkReason == "" {
		t.Fatal("premise: the parked step recorded no park reason")
	}

	const note = "the flake is known; skipping is the right call"
	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("note", note), "set --note: %v", nil)
	w, buf := bufWriter(true)
	testsupport.Must(t,
		runStepResolve(cmd, []string{model.FormatStepID(id)}, w),
		"step resolve: %v\n%s", err, buf.String())

	rw, rbuf := bufWriter(true)
	testsupport.Must(t,
		runRunReport(cmdWithDB(conn), model.FormatRunID(step.RunID), rw),
		"run report: %v\n%s", err, rbuf.String())

	var envelope struct {
		Data struct {
			Attempts []engine.StepAttempt `json:"attempts"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(rbuf.Bytes(), &envelope),
		"decoding the report: %v\n%s", err, rbuf.String())

	var row *engine.StepAttempt
	for i := range envelope.Data.Attempts {
		if envelope.Data.Attempts[i].Instance == "flaky@0" {
			row = &envelope.Data.Attempts[i]
		}
	}
	if row == nil {
		t.Fatalf("the report carries no flaky@0 row:\n%s", rbuf.String())
	}

	if row.ParkReason != parkReason {
		t.Errorf("park_reason = %q, want the engine's own text %q",
			row.ParkReason, parkReason)
	}
	if strings.Contains(row.ParkReason, note) {
		t.Errorf("park_reason = %q absorbed the resolution note", row.ParkReason)
	}
	if !strings.HasPrefix(row.Routing, engine.ResolveSkip) ||
		!strings.Contains(row.Routing, note) {
		t.Errorf("routing = %q, want the resolution's routing and note",
			row.Routing)
	}
}
