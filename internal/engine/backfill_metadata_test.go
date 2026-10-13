package engine

import (
	"testing"

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

// TestBackfillMetadataRefusesALiveStep pins the refusal: a step still held
// under its token takes no back-fill metadata, and nothing is written, not even
// the usage row that came with it.
func TestBackfillMetadataRefusesALiveStep(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, implID, ClaimOptions{
		Owner: "w", NowMS: nowMS, Metadata: `{"variant":"x"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	_, err = e.BackfillUsage(conn, runID, []BackfillRow{{
		Step: implID, Unit: "tokens", Quantity: 1200,
		Metadata: `{"model_resolved":"claude-opus-5-5"}`,
	}}, "", "", nowMS)
	if err == nil {
		t.Fatal("a back-fill carrying metadata landed on a live step; it must " +
			"refuse, because the record is written under the holder's token")
	}
	if rows := usageRowsFor(t, conn, implID); len(rows) != 0 {
		t.Errorf("refused back-fill wrote %d ledger rows, want 0", len(rows))
	}
	bag := stepMetadata(t, conn, implID)
	if _, ok := bag["model_resolved"]; ok {
		t.Errorf("refused back-fill wrote model_resolved onto a live step: %v", bag)
	}
}
