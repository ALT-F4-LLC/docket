package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// RESOLVED METADATA ON A VARIANT-BEARING STEP.
//
// From 2026-09-05 every executor row recorded `model_resolved` and
// `effort_resolved` as "unknown" while the same rows began carrying a
// `variant` key. The engine never reads these keys, so the regression lives in
// the writer, not in the merge. This test pins the engine's half of the
// contract: a claim that records the variant and the unknown placeholders, then
// a completion that reports the served model and effort, leaves the resolved
// values and the variant both on the row.
//
// GENERICITY: the keys are opaque to core. The test checks that the bag is
// stored verbatim, which is the only property core owns here.
func TestVariantBagKeepsResolvedFieldsAfterCompletion(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")

	// The claim-time bag, shaped as the dispatcher writes it: routing request,
	// variant, and placeholders for what the runtime has not yet reported.
	claim, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"variant":"x","model_requested":"opus-medium","effort_requested":"medium","model_resolved":"unknown","effort_resolved":"unknown"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
		Metadata: `{"model_resolved":"claude-opus-5-5","effort_resolved":"high"}`,
	})
	testsupport.Must(t, err, "complete: %v", err)

	bag := stepMetadata(t, conn, stepID)
	if got := bag["model_resolved"]; got != "claude-opus-5-5" {
		t.Errorf("model_resolved = %v, want claude-opus-5-5 — the completion's "+
			"observation did not reach a variant-bearing row", got)
	}
	if got := bag["effort_resolved"]; got != "high" {
		t.Errorf("effort_resolved = %v, want high", got)
	}
	if got := bag["variant"]; got != "x" {
		t.Errorf("variant = %v, want x — the merge dropped the claim-time key", got)
	}
	if got := bag["model_requested"]; got != "opus-medium" {
		t.Errorf("model_requested = %v, want opus-medium — the request must survive "+
			"the completion for a later audit to pair it with the resolution", got)
	}
}
