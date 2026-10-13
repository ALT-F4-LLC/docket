package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A back-fill row may carry an opaque metadata bag. It lands on the step's
// record in the same transaction as its usage, which is what lets a transcript
// read record a step's observed routing for a step that already finished.

// TestBackfillMetadataLandsWithItsUsage pins the write: on a finished step the
// bag merges over the claim-time bag, the claim-time keys survive, and the
// usage row is written in the same call.
func TestBackfillMetadataLandsWithItsUsage(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	claim, err := ClaimStep(conn, implID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
		Metadata: `{"variant":"x","model_resolved":"unknown"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, implID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "tokens", Quantity: 1200,
		Metadata: `{"model_resolved":"claude-opus-5-5","effort_resolved":"unknown"}`,
	}}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	if rows := usageRowsFor(t, conn, implID); len(rows) != 1 {
		t.Fatalf("wrote %d ledger rows, want 1", len(rows))
	}
	if out.MetadataMerged != 1 {
		t.Errorf("MetadataMerged = %d, want 1 — one step's record took a bag",
			out.MetadataMerged)
	}
	bag := stepMetadata(t, conn, implID)
	if got := bag["model_resolved"]; got != "claude-opus-5-5" {
		t.Errorf("model_resolved = %v, want claude-opus-5-5 — the back-fill's "+
			"observation did not reach the step's record", got)
	}
	if got := bag["variant"]; got != "x" {
		t.Errorf("variant = %v, want x — the back-fill's merge dropped the "+
			"claim-time key", got)
	}
}

// TestBackfillMetadataAfterSkippedDuplicate is the historical shape: a step
// whose usage is already in the ledger, back-filled again with its observation.
// The duplicate usage row is skipped under --on-duplicate=skip, and the bag
// still merges, because the metadata pass does not depend on the usage write.
func TestBackfillMetadataAfterSkippedDuplicate(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	claim, err := ClaimStep(conn, implID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
		Metadata: `{"variant":"x","model_resolved":"unknown"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, implID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 1200},
	}, "", "", nowMS)
	testsupport.Must(t, err, "first backfill-usage: %v", err)

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "tokens", Quantity: 1200,
		Metadata: `{"model_resolved":"claude-opus-5-5"}`,
	}}, "", OnDuplicateSkip, nowMS)
	testsupport.Must(t, err, "second backfill-usage with skip: %v", err)

	if len(out.Skipped) != 1 {
		t.Fatalf("skipped %d rows, want 1 — the duplicate usage row must be "+
			"reported, not silently dropped", len(out.Skipped))
	}
	if out.MetadataMerged != 1 {
		t.Errorf("MetadataMerged = %d, want 1 — the bag merges even when its "+
			"usage row is skipped", out.MetadataMerged)
	}
	if rows := usageRowsFor(t, conn, implID); len(rows) != 1 {
		t.Fatalf("ledger has %d rows, want 1 — the skipped duplicate must not "+
			"write", len(rows))
	}
	bag := stepMetadata(t, conn, implID)
	if got := bag["model_resolved"]; got != "claude-opus-5-5" {
		t.Errorf("model_resolved = %v, want claude-opus-5-5 — the observation "+
			"must land on a historical step even when its usage is skipped", got)
	}
	if got := bag["variant"]; got != "x" {
		t.Errorf("variant = %v, want x", got)
	}
}

// TestBackfillMetadataDropsOnALiveStep pins the drop: a step still held under
// its token takes no back-fill metadata, but its usage row lands, the batch
// succeeds, and the step is named on the outcome. The record is written under
// the holder's token, so the bag must not reach it.
func TestBackfillMetadataDropsOnALiveStep(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, implID, ClaimOptions{
		Owner: "w", NowMS: nowMS, Metadata: `{"variant":"x"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "tokens", Quantity: 1200,
		Metadata: `{"model_resolved":"claude-opus-5-5"}`,
	}}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage on a live step: %v", err)

	if rows := usageRowsFor(t, conn, implID); len(rows) != 1 {
		t.Errorf("wrote %d ledger rows, want 1 — the usage row must land even "+
			"when its bag is dropped", len(rows))
	}
	if out.Written != 1 {
		t.Errorf("Written = %d, want 1", out.Written)
	}
	if out.MetadataMerged != 0 {
		t.Errorf("MetadataMerged = %d, want 0 — no bag reached a record", out.MetadataMerged)
	}
	if len(out.MetadataDropped) != 1 {
		t.Fatalf("MetadataDropped names %d steps, want 1", len(out.MetadataDropped))
	}
	if got := out.MetadataDropped[0].Instance; got != "implement@0" {
		t.Errorf("MetadataDropped names %q, want implement@0", got)
	}
	bag := stepMetadata(t, conn, implID)
	if _, ok := bag["model_resolved"]; ok {
		t.Errorf("dropped bag reached a live step's record: %v", bag)
	}
	if got := bag["variant"]; got != "x" {
		t.Errorf("variant = %v, want x — the live step's own bag must survive", got)
	}
}

// TestBackfillMetadataDropsOnAReapedPendingStep is the shape that failed the
// whole dispatch close: a claim that was force-reaped, leaving the step pending
// for retry with attempt 1. The back-fill carries the dead spawn's usage and
// its metadata. The usage lands against attempt 1, the bag is dropped, and the
// step is named under MetadataDropped with its pending status. It is not named
// under Unclaimed, because a worker did claim it.
func TestBackfillMetadataDropsOnAReapedPendingStep(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, implID, ClaimOptions{Owner: "doomed", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = ForceReapStep(conn, implID, "spawn died; journal has usage", nowMS+1)
	testsupport.Must(t, err, "ForceReapStep: %v", err)

	step, err := db.GetStep(conn, implID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending || step.Attempt != 1 {
		t.Fatalf("premise: status %q attempt %d after the reap, want pending "+
			"attempt 1", step.Status, step.Attempt)
	}

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "output_tokens", Quantity: 48344,
		Metadata: `{"model_resolved":"claude-opus-5-5"}`,
	}}, "wave-journal:dead", "", nowMS+2)
	testsupport.Must(t, err, "backfill-usage on a reaped pending step: %v", err)

	rows := usageRowsFor(t, conn, implID)
	if len(rows) != 1 {
		t.Fatalf("wrote %d ledger rows, want 1", len(rows))
	}
	if len(out.MetadataDropped) != 1 {
		t.Fatalf("MetadataDropped names %d steps, want 1", len(out.MetadataDropped))
	}
	if d := out.MetadataDropped[0]; d.Status != db.StepPending {
		t.Errorf("MetadataDropped status = %q, want %q", d.Status, db.StepPending)
	}
	if len(out.Unclaimed) != 0 {
		t.Errorf("Unclaimed = %v, want empty — the step was claimed before its reap",
			out.Unclaimed)
	}
	if _, ok := stepMetadata(t, conn, implID)["model_resolved"]; ok {
		t.Error("dropped bag reached a pending step's record")
	}
}

// TestBackfillMetadataRefusesANonObjectBagOnALiveStep pins that the shape check
// does not depend on the step's status. A non-object bag refuses the whole
// batch on a live step too, and writes nothing, even though the same bag would
// have been dropped had it been a valid object.
func TestBackfillMetadataRefusesANonObjectBagOnALiveStep(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, implID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	_, err = e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "tokens", Quantity: 1200, Metadata: `[1]`,
	}}, "", "", nowMS)
	if err == nil {
		t.Fatal("a non-object bag was accepted on a live step; the shape must " +
			"refuse whatever the step's status")
	}
	if rows := usageRowsFor(t, conn, implID); len(rows) != 0 {
		t.Errorf("refused back-fill wrote %d ledger rows, want 0", len(rows))
	}
}
