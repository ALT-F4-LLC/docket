package api

import (
	"context"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

func TestGuardHandlersBindVerbLayer(t *testing.T) {
	const caller = "/repos/guard-caller"
	st := openAPIStore(t)
	projectID, err := st.EnsureProject(caller, "guard-caller", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	server := serve(t, NewService(st, "test"))
	c := client.NewClient(server.Client(), server.URL, discardLogger)
	ctx := context.Background()

	stop, err := c.GuardStop(ctx, inProject(&docketv1.GuardStopRequest{}, caller))
	requireServed(t, "GuardStop", err)
	if !stop.Msg.GetDecision().GetAllow() {
		t.Errorf("GuardStop over a project with no runs = %v, want allow", stop.Msg.GetDecision())
	}

	record, err := c.GuardRecord(ctx, inProject(&docketv1.GuardRecordRequest{}, caller))
	requireServed(t, "GuardRecord", err)
	if !record.Msg.GetDecision().GetAllow() {
		t.Errorf("GuardRecord over a project with no runs = %v, want allow", record.Msg.GetDecision())
	}

	gate, err := c.GuardGate(ctx, inProject(&docketv1.GuardGateRequest{Step: "approve"}, caller))
	requireServed(t, "GuardGate", err)
	if d := gate.Msg.GetDecision(); d.GetAllow() || !strings.Contains(d.GetReason(), "approve") {
		t.Errorf("GuardGate approve with no run = %v, want a denial naming the step", d)
	}

	spawn, err := c.GuardSpawn(ctx, inProject(&docketv1.GuardSpawnRequest{Active: true}, caller))
	requireServed(t, "GuardSpawn", err)
	if !spawn.Msg.GetDecision().GetAllow() {
		t.Errorf("GuardSpawn --active with no reaps = %v, want allow", spawn.Msg.GetDecision())
	}

	_, err = c.GuardSpawn(ctx, inProject(&docketv1.GuardSpawnRequest{
		Active: true, Rows: []*docketv1.StepRow{{Step: "STEP-1"}},
	}, caller))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("GuardSpawn --active with rows code = %q, want %q: the verb sees the proposed rows (err %v)", got, output.ErrValidation, err)
	}

	run, _, err := st.InsertRunWithContextIdempotent(projectID, "", 0, model.NowMS(), db.RunContext{}, "")
	if err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	proposed, err := c.GuardSpawn(ctx, inProject(&docketv1.GuardSpawnRequest{
		Run: model.FormatRunID(run.ID), Rows: []*docketv1.StepRow{{Step: "STEP-1"}, {Step: "STEP-2"}},
	}, caller))
	requireServed(t, "GuardSpawn", err)
	if d := proposed.Msg.GetDecision(); d.GetAllow() || !strings.Contains(d.GetReason(), "2 row(s) were proposed") {
		t.Errorf("GuardSpawn two rows with no open dispatch = %v, want a denial counting both rows", d)
	}

	_, err = c.GuardStop(ctx, inProject(&docketv1.GuardStopRequest{Run: "RUN-1"}, caller))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("GuardStop with a run code = %q, want %q: the verb answers per project, never per run (err %v)", got, output.ErrValidation, err)
	}
}
