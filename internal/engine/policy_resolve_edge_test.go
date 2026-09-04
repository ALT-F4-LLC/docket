package engine

import "testing"

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
