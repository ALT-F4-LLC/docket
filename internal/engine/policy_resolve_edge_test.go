package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestResolveExecutorRefusesUnknownHint: a seat name absent from [executors]
// is a hard refusal, matching wave.js's resolve() — no synthesized default.
func TestResolveExecutorRefusesUnknownHint(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	if _, err := doc.ResolveExecutor("no-such-seat", 0, "", nil); err == nil {
		t.Error("want a refusal for a hint with no [executors] row")
	}
}

// TestResolveSeatRefusesUnknownSeat is the same refusal on the vote path.
func TestResolveSeatRefusesUnknownSeat(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	if _, err := doc.ResolveSeat("no-such-seat", nil); err == nil {
		t.Error("want a refusal for a voter with no [executors] row")
	}
}

// TestResolveExecutorRefusesDanglingVariant: an [executors] entry naming a
// variant that has no [variants] row is a policy-authoring bug, refused
// rather than silently treated as a chain end.
func TestResolveExecutorRefusesDanglingVariant(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }

[executors]
worker = { variant = "no-such-variant" }
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if _, err := doc.ResolveExecutor("worker", 0, "", nil); err == nil {
		t.Error("want a refusal when [executors].variant names an undeclared variant")
	}
}

// TestResolveExecutorRefusesDanglingEscalateTo: an escalate_to naming an
// undeclared variant is a typo'd chain link, refused mid-walk rather than
// treated as a natural chain end (a MISSING escalate_to, by contrast, is a
// legitimate chain end — see TestEscalationWalkFableStandingRowsDoNotMove and
// the chain-exhaustion cases the ported table already covers).
func TestResolveExecutorRefusesDanglingEscalateTo(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high", escalate_to = "no-such-variant" }

[executors]
worker = { variant = "tier-a" }
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if _, err := doc.ResolveExecutor("worker", 1, "", nil); err == nil {
		t.Error("want a refusal when escalate_to names an undeclared variant")
	}
}

// TestResolveExecutorNoPermittedModelIsRefused is the last-resort safety net:
// a never-list with no usable [escalation.fallback] leaves nothing permitted,
// which must refuse rather than silently return a forbidden model.
func TestResolveExecutorNoPermittedModelIsRefused(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
tier-a = { model = "fable", effort = "high" }

[executors]
worker = { variant = "tier-a", never = ["fable"] }
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if _, err := doc.ResolveExecutor("worker", 0, "", nil); err == nil {
		t.Error("want a refusal when the standing variant is forbidden and no fallback exists")
	}
}

// TestResolveSeatNeverRedirectsThroughFallback: a vote seat has no attempt or
// round to walk, but its STANDING variant is still checked against `never`
// and redirected through [escalation.fallback] exactly once.
func TestResolveSeatNeverRedirectsThroughFallback(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
banned = { model = "fable", effort = "high" }
allowed = { model = "opus", effort = "high" }

[executors]
seat-a = { variant = "banned", never = ["fable"] }

[escalation.fallback]
banned = "allowed"
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	got, err := doc.ResolveSeat("seat-a", nil)
	if err != nil {
		t.Fatalf("ResolveSeat: %v", err)
	}
	if got.Variant != "allowed" || got.Model != "opus" {
		t.Errorf("got %+v, want variant=allowed model=opus", got)
	}
}

// TestResolveSeatCeilingClampsTheStandingVariant: a sensitive seat standing
// beyond [security].ceiling is clamped to it directly, with no walk involved.
func TestResolveSeatCeilingClampsTheStandingVariant(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
low = { model = "opus", effort = "medium", escalate_to = "high" }
high = { model = "opus", effort = "high", escalate_to = "beyond" }
beyond = { model = "opus", effort = "max" }

[executors]
seat-a = { variant = "beyond" }

[security]
ceiling = "high"
nodes = ["seat-a"]
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	got, err := doc.ResolveSeat("seat-a", nil)
	if err != nil {
		t.Fatalf("ResolveSeat: %v", err)
	}
	if got.Variant != "high" {
		t.Errorf("Variant = %q, want the ceiling %q", got.Variant, "high")
	}
}

// TestResolveExecutorSensitiveByLabelMergesGlobalNever: a row that is not
// itself pinned by [executors].never still picks up [security].never when its
// issue carries one of [security].labels.
//
// The chain climbs onto a "gemini" model rather than "fable" on purpose: a
// walk that lands on fable triggers the SEPARATE fable-gate check
// (fableEligible) regardless of any never-list, which would confound what
// this test isolates — the label-triggered never-list merge on its own.
func TestResolveExecutorSensitiveByLabelMergesGlobalNever(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
start = { model = "opus", effort = "medium", escalate_to = "gemini-tier" }
gemini-tier = { model = "gemini", effort = "high" }

[executors]
worker = { variant = "start" }

[security]
ceiling = "gemini-tier"
labels = ["sensitive"]
never = ["gemini"]

[escalation.fallback]
gemini-tier = "start"
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	// Unlabelled: never is empty, the hop lands on gemini-tier's model freely.
	got, err := doc.ResolveExecutor("worker", 1, "", nil)
	if err != nil {
		t.Fatalf("ResolveExecutor (unlabelled): %v", err)
	}
	if got.Model != "gemini" {
		t.Errorf("unlabelled worker attempt:1 model = %q, want gemini (no [security].never merged)", got.Model)
	}

	// Labelled sensitive: [security].never merges in, so the same hop never
	// lands on the forbidden model.
	got, err = doc.ResolveExecutor("worker", 1, "", []string{"sensitive"})
	if err != nil {
		t.Fatalf("ResolveExecutor (labelled): %v", err)
	}
	if got.Model == "gemini" {
		t.Errorf("sensitive worker attempt:1 resolved to gemini; [security].never must have redirected it")
	}
}

// TestFableEligibleDistinguishesReapedFromFailed pins the
// "failed-top-opus-round" fable gate to RECORDED FAILURES, not spent claims.
// Two rows share Attempt=1 and the same round instance; one claim was reaped,
// the other failed. Only the failed row may stay on Fable.
//
// The fixture is self-contained because escalationWalkPolicy cannot reach the
// gate here: its opus-xhigh seats are [security] nodes with never = ["fable"],
// so the hop onto fable-xhigh is redirected mid-walk and the post-walk fable
// check never runs. With FailedAttempts=0 the only hop comes from the round
// ordinal, so worker is a round executor and runs at worker@2. opus-max is
// reachable only through [escalation.fallback], so a row resolving to it
// proves the walk landed on fable-xhigh and the gate sent it back.
func TestFableEligibleDistinguishesReapedFromFailed(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
opus-xhigh = { model = "opus", effort = "xhigh", escalate_to = "fable-xhigh" }
fable-xhigh = { model = "fable", effort = "xhigh" }
opus-max = { model = "opus", effort = "max" }

[executors]
worker = { variant = "opus-xhigh" }

[escalation]
on_round = "one-hop"
round_executors = ["worker"]
fable_gates = ["failed-top-opus-round"]

[escalation.fallback]
fable-xhigh = "opus-max"
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	cases := []struct {
		name           string
		failedAttempts int
		reapedClaims   int
		wantVariant    string
		wantModel      string
	}{
		{"reaped claim stays off fable", 0, 1, "opus-max", "opus"},
		{"failed claim moves onto fable", 1, 0, "fable-xhigh", "fable"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			row := &model.StepRow{
				Step:           "STEP-1",
				Instance:       "worker@2",
				Executor:       "worker",
				Attempt:        1,
				FailedAttempts: tt.failedAttempts,
				ReapedClaims:   tt.reapedClaims,
			}
			if err := resolveRowRouting(doc, row); err != nil {
				t.Fatalf("resolveRowRouting: %v", err)
			}
			if row.Variant != tt.wantVariant || row.Model != tt.wantModel {
				t.Errorf("row resolved to %s/%s, want %s/%s (attempt 1, failed %d, reaped %d)",
					row.Model, row.Variant, tt.wantModel, tt.wantVariant,
					tt.failedAttempts, tt.reapedClaims)
			}

			direct, err := doc.ResolveExecutor(row.Executor, row.FailedAttempts, row.Instance, row.Labels)
			if err != nil {
				t.Fatalf("ResolveExecutor: %v", err)
			}
			if row.Model != direct.Model || row.Effort != direct.Effort || row.Variant != direct.Variant {
				t.Errorf("row routing %s/%s/%s disagrees with ResolveExecutor %s/%s/%s",
					row.Model, row.Effort, row.Variant,
					direct.Model, direct.Effort, direct.Variant)
			}
		})
	}
}
