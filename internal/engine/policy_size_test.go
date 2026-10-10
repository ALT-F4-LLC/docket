package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// sizePolicyTOML exercises [sizes] against a chain that also carries
// [security], so a size-derived starting variant's interaction with the
// ceiling/never clamp is covered by the same fixture as the plain walk:
//
//   - worker stands on "small" by default (no size label).
//   - "trivial" maps to haiku-low, "needs-design" maps to opus-high.
//   - opus-high is beyond [security].ceiling (sonnet-medium), so a sensitive
//     row asking for "needs-design" still clamps to the ceiling.
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
needs-design = "opus-high"

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

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", "", nil)
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

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"trivial"})
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

	// "needs-design" appears first in the issue's declared label order, so it wins
	// over "trivial" even though "trivial" would otherwise sort earlier.
	got, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"needs-design", "trivial"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "opus-high" {
		t.Errorf("got %+v, want the first-declared label's opus-high", got)
	}

	reversed, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"trivial", "needs-design"})
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

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"needs-design", "sensitive"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "sonnet-medium" {
		t.Errorf("got %+v, want the [security].ceiling sonnet-medium — "+
			"[sizes] must not bypass the ceiling on a sensitive row", got)
	}
}

func TestResolveExecutorRefusesUnknownSizeVariant(t *testing.T) {
	// parsePolicy now refuses this [sizes] entry, so the doc is built
	// directly to keep the resolve-time guard covered.
	doc := &policyDoc{
		Variants:  map[string]policyVariant{"tier-a": {Model: "opus", Effort: "high"}},
		Executors: map[string]policyExecutor{"worker": {Variant: "tier-a"}},
		Sizes:     map[string]string{"trivial": "no-such-variant"},
	}

	if _, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"trivial"}); err == nil {
		t.Error("want a refusal for a [sizes] entry naming a variant with no [variants] row")
	}
}

func TestResolveSeatNoSizeLabelIsUnchanged(t *testing.T) {
	doc, err := parsePolicy([]byte(sizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	got, err := doc.ResolveSeat("worker", "", nil)
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

	got, err := doc.ResolveSeat("worker", "", []string{"trivial"})
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

	got, err := doc.ResolveSeat("worker", "", []string{"needs-design", "sensitive"})
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

	got, err := doc.ResolveExecutor("worker", 0, "worker@0", "", []string{"trivial"})
	if err != nil {
		t.Fatalf("ResolveExecutor: %v", err)
	}
	if got.Variant != "tier-a" {
		t.Errorf("got %+v, want the executor's own standing tier-a "+
			"(a label naming no [sizes] entry must not change resolution)", got)
	}
}

// frozenSizePolicyTOML maps a frozen Size, a size label, and unknown to three
// distinct variants, none of them worker's own sonnet-medium, so each test
// can tell which input chose the starting variant.
const frozenSizePolicyTOML = `
[policy]
version = 2

[variants]
haiku-low = { model = "haiku", effort = "low" }
sonnet-low = { model = "sonnet", effort = "low" }
sonnet-medium = { model = "sonnet", effort = "medium" }
opus-high = { model = "opus", effort = "high" }

[executors]
worker = { variant = "sonnet-medium" }

[sizes]
small = "haiku-low"
trivial = "opus-high"
unknown = "sonnet-low"
`

// onlySmallPolicyTOML maps only small, so a frozen bounded Size has no
// [sizes] entry of its own.
const onlySmallPolicyTOML = `
[policy]
version = 2

[variants]
haiku-low = { model = "haiku", effort = "low" }
sonnet-medium = { model = "sonnet", effort = "medium" }

[executors]
worker = { variant = "sonnet-medium" }

[sizes]
small = "haiku-low"
`

func TestFrozenSizeRouting(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		size   string
		labels []string
		want   string
	}{
		{"mapped size with no size label", frozenSizePolicyTOML, "small", nil, "haiku-low"},
		{"mapped size beats a mapped size label", frozenSizePolicyTOML, "small", []string{"trivial"}, "haiku-low"},
		{"unknown size falls back to the size label", frozenSizePolicyTOML, "unknown", []string{"trivial"}, "opus-high"},
		{"unmapped size falls back to the size label", onlySmallPolicyTOML, "bounded", []string{"small"}, "haiku-low"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := parsePolicy([]byte(tt.policy))
			if err != nil {
				t.Fatalf("parsePolicy: %v", err)
			}

			executor, err := doc.ResolveExecutor("worker", 0, "worker@0", tt.size, tt.labels)
			if err != nil {
				t.Fatalf("ResolveExecutor: %v", err)
			}
			if executor.Variant != tt.want {
				t.Errorf("ResolveExecutor(size %q, labels %v) variant = %q, want %q",
					tt.size, tt.labels, executor.Variant, tt.want)
			}

			seat, err := doc.ResolveSeat("worker", tt.size, tt.labels)
			if err != nil {
				t.Fatalf("ResolveSeat: %v", err)
			}
			if seat.Variant != tt.want {
				t.Errorf("ResolveSeat(size %q, labels %v) variant = %q, want %q",
					tt.size, tt.labels, seat.Variant, tt.want)
			}
		})
	}
}

func TestResolveRowRoutingAppliesFrozenSize(t *testing.T) {
	doc, err := parsePolicy([]byte(frozenSizePolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}

	executorRow := &model.StepRow{Step: "STEP-1", Instance: "worker@0", Executor: "worker", Size: "small"}
	if err := resolveRowRouting(doc, executorRow); err != nil {
		t.Fatalf("resolveRowRouting executor row: %v", err)
	}
	if executorRow.Variant != "haiku-low" {
		t.Errorf("executor row variant = %q, want small's haiku-low", executorRow.Variant)
	}

	voteRow := &model.StepRow{Step: "STEP-2", Instance: "vote@0", Voters: []string{"worker", "worker"}, Size: "small"}
	if err := resolveRowRouting(doc, voteRow); err != nil {
		t.Fatalf("resolveRowRouting vote row: %v", err)
	}
	if len(voteRow.VoterAssignments) != 2 {
		t.Fatalf("vote row has %d voter assignments, want 2", len(voteRow.VoterAssignments))
	}
	for _, a := range voteRow.VoterAssignments {
		if a.Variant != "haiku-low" {
			t.Errorf("voter %q variant = %q, want small's haiku-low", a.Voter, a.Variant)
		}
	}
}
