package engine

import "testing"

// sizePolicyTOML exercises [sizes] against a chain that also carries
// [security], so a size-derived starting variant's interaction with the
// ceiling/never clamp is covered by the same fixture as the plain walk:
//
//   - worker stands on "small" by default (no size label).
//   - "trivial" maps to haiku-low, "large" maps to opus-high.
//   - opus-high is beyond [security].ceiling (sonnet-medium), so a sensitive
//     row asking for "large" still clamps to the ceiling.
const sizePolicyTOML = `
[policy]
version = 2

[variants]
haiku-low = { model = "haiku", effort = "low" }
sonnet-medium = { model = "sonnet", effort = "medium", escalate_to = "opus-high" }
opus-high = { model = "opus", effort = "high" }

[executors]
worker = { variant = "sonnet-medium" }

[sizes]
trivial = "haiku-low"
large = "opus-high"

[security]
ceiling = "sonnet-medium"
labels = ["sensitive"]
never = []
`

func TestResolveExecutorNoSizeLabelIsUnchanged(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", nil)
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "sonnet-medium" || got.Model != "sonnet" || got.Effort != "medium" {
		t.Errorf("got %+v, want the executor's own standing sonnet/medium/sonnet-medium", got)
	}
}

func TestResolveExecutorAppliesSizeLabel(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"trivial"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "haiku-low" || got.Model != "haiku" || got.Effort != "low" {
		t.Errorf("got %+v, want the [sizes]-mapped haiku/low/haiku-low", got)
	}
}

func TestResolveExecutorFirstMatchingSizeLabelWins(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	// "large" appears first in the issue's declared label order, so it wins
	// over "trivial" even though "trivial" would otherwise sort earlier.
	got, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"large", "trivial"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "opus-high" {
		t.Errorf("got %+v, want the first-declared label's opus-high", got)
	}

	reversed, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"trivial", "large"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if reversed.Variant != "haiku-low" {
		t.Errorf("got %+v, want the first-declared label's haiku-low", reversed)
	}
}

func TestResolveExecutorSizeLabelStillClampsToSecurityCeiling(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"large", "sensitive"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "sonnet-medium" {
		t.Errorf("got %+v, want the [security].ceiling sonnet-medium — "+
			"[sizes] must not bypass the ceiling on a sensitive row", got)
	}
}

func TestResolveExecutorRefusesUnknownSizeVariant(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }

[executors]
worker = { variant = "tier-a" }

[sizes]
trivial = "no-such-variant"
`
	doc, err := parsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	if _, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"trivial"}); err == nil {
		t.Error("want a refusal for a [sizes] entry naming a variant with no [variants] row")
	}
}

func TestResolveSeatNoSizeLabelIsUnchanged(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveSeat("worker", nil)
	if err != nil {
		t.Fatalf("ResolveSeat: %v", err)
	}
	if got.Variant != "sonnet-medium" {
		t.Errorf("got %+v, want the seat's own standing sonnet-medium", got)
	}
}

func TestResolveSeatAppliesSizeLabel(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveSeat("worker", []string{"trivial"})
	if err != nil {
		t.Fatalf("ResolveSeat: %v", err)
	}
	if got.Variant != "haiku-low" {
		t.Errorf("got %+v, want the [sizes]-mapped haiku-low", got)
	}
}

func TestResolveSeatSizeLabelStillClampsToSecurityCeiling(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveSeat("worker", []string{"large", "sensitive"})
	if err != nil {
		t.Fatalf("ResolveSeat: %v", err)
	}
	if got.Variant != "sonnet-medium" {
		t.Errorf("got %+v, want the [security].ceiling sonnet-medium", got)
	}
}

func TestParsePolicyAcceptsAbsentSizes(t *testing.T) {
	// minimalPolicyTOML (policy_walk_test.go) declares no [sizes] table at
	// all — dormancy: a policy that never opts in parses and resolves
	// exactly as it did before this field existed.
	doc, err := parsePolicy([]byte(minimalPolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if len(doc.Sizes) != 0 {
		t.Errorf("Sizes = %+v, want empty for a policy with no [sizes] table", doc.Sizes)
	}

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", []string{"trivial"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "tier-a" {
		t.Errorf("got %+v, want the executor's own standing tier-a "+
			"(a label naming no [sizes] entry must not change resolution)", got)
	}
}
