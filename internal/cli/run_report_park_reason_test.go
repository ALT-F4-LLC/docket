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

	// THE WIRE KEY, read without engine.StepAttempt. Decoding into the struct
	// that encoded the report cannot catch a renamed tag — both sides move
	// together — so the key a consumer reads is pinned here by its name.
	var raw struct {
		Data struct {
			Attempts []map[string]any `json:"attempts"`
		} `json:"data"`
	}
	err = json.Unmarshal(rbuf.Bytes(), &raw)
	testsupport.Must(t, err, "decoding the report as raw JSON: %v\n%s", err, rbuf.String())
	var rawRow map[string]any
	for _, a := range raw.Data.Attempts {
		if a["instance"] == "flaky@0" {
			rawRow = a
		}
	}
	if rawRow == nil {
		t.Fatalf("the raw report carries no flaky@0 row:\n%s", rbuf.String())
	}
	if got, _ := rawRow["park_reason"].(string); got != parkReason {
		t.Errorf("run report attempts[].park_reason = %q, want %q under that "+
			"exact key", got, parkReason)
	}

	// `step show --json` carries the same key with the same text.
	sw, sbuf := bufWriter(true)
	err = runStepShow(cmdWithDB(conn), []string{model.FormatStepID(id)}, sw)
	testsupport.Must(t, err, "step show: %v\n%s", err, sbuf.String())
	var shown struct {
		Data map[string]any `json:"data"`
	}
	err = json.Unmarshal(sbuf.Bytes(), &shown)
	testsupport.Must(t, err, "decoding step show: %v\n%s", err, sbuf.String())
	if got, _ := shown.Data["park_reason"].(string); got != parkReason {
		t.Errorf("step show park_reason = %q, want %q under that exact key",
			got, parkReason)
	}
}
