package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestResolverKeysHopsOnRecordedFailures pins the escalation walk: it
// advances one hop per RECORDED FAILURE, not per spent claim. A claim the
// lease reaped (session kill, dead relay, lapsed lease) measured nothing, so
// the re-run must resolve to the tier it was reaped from.
//
// Each case is driven through resolveRowRouting — the dispatch/next/step row
// path in policy_pin.go — and cross-checked against ResolveExecutor directly,
// so the row's routing and the resolver cannot drift apart.
func TestResolverKeysHopsOnRecordedFailures(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)

	cases := []struct {
		name           string
		attempt        int
		failedAttempts int
		reapedClaims   int
		want           string
	}{
		{"never claimed", 0, 0, 0, "sonnet-medium"},
		{"one claim, still live", 1, 0, 0, "sonnet-medium"},
		{"one reaped claim", 2, 0, 1, "sonnet-medium"},
		{"two reaped claims", 3, 0, 2, "sonnet-medium"},
		{"one failed claim", 1, 1, 0, "opus-medium"},
		{"one reaped and one failed claim", 2, 1, 1, "opus-medium"},
		{"two reaped and one failed claim", 3, 1, 2, "opus-medium"},
		{"two failed claims", 2, 2, 0, "opus-high"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			row := &model.StepRow{
				Step:           "STEP-1",
				Instance:       "implement@0",
				Executor:       "implement",
				Attempt:        tt.attempt,
				FailedAttempts: tt.failedAttempts,
				ReapedClaims:   tt.reapedClaims,
			}
			if err := resolveRowRouting(doc, row); err != nil {
				t.Fatalf("resolveRowRouting: %v", err)
			}
			if row.Variant != tt.want {
				t.Errorf("dispatch row resolved to variant %q, want %q "+
					"(attempt %d, failed %d, reaped %d)",
					row.Variant, tt.want, tt.attempt, tt.failedAttempts, tt.reapedClaims)
			}

			direct, err := doc.ResolveExecutor(row.Executor, tt.failedAttempts, row.Instance, row.Labels)
			if err != nil {
				t.Fatalf("ResolveExecutor: %v", err)
			}
			if row.Model != direct.Model || row.Effort != direct.Effort || row.Variant != direct.Variant {
				t.Errorf("dispatch row routing %s/%s/%s disagrees with ResolveExecutor %s/%s/%s",
					row.Model, row.Effort, row.Variant,
					direct.Model, direct.Effort, direct.Variant)
			}
		})
	}
}
