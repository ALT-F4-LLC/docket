package engine

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

// POLICY.TOML (DKT-1282) — the engine's own reading of the routing policy a
// run pins at activation, replacing dotfiles' wave.js and tribunal.js: both
// carried a byte-identical hand-rolled TOML-subset parser and resolve() /
// resolveSeat() walk, kept in sync only by a repo-external test
// (tests/workflow-sync.test.sh diffing their SYNC-marked regions). The
// 76%-condensed policy, the wave routed on an unpinned policy, and the
// 44-char drop denied twice all trace to the policy crossing the
// engine/wave boundary as a ~28k-char text blob a second parser re-derived.
//
// The engine already pins policy.toml's bytes at activation — it lands in
// the generic file-pin bucket scanConfigDirs gives every instance-config file
// it does not otherwise register (autoregister.go F4) — so THIS is the parser
// with authority: policyForRun (policy_resolve.go) reads the pinned bytes
// back and verifies them against the hash activation recorded, the same
// ladder readPinnedPacketFile uses for every other pinned instance file.
//
// This file is the GRAMMAR (decode + shape check); policy_resolve.go is the
// WALK (attempt/round escalation, [security] never/ceiling, [escalation]
// fallback) — a direct port of wave.js's resolve()/resolveSeat(), field for
// field and branch for branch, so a run's escalation ladder matches
// tests/wave-escalation-walk.test.sh's expectations for the same policy
// (DKT-1282 AC2).

// policyVersionFloor is the lowest [policy].version this engine understands —
// wave.js's assertPolicyShape's POLICY_VERSION_FLOOR, ported.
const policyVersionFloor = 2

// policyVariant is one `[variants].<name>` inline table.
type policyVariant struct {
	Model      string `toml:"model"`
	Effort     string `toml:"effort"`
	EscalateTo string `toml:"escalate_to"`
}

// policyExecutor is one `[executors].<name>` inline table — a seat's standing
// variant, plus its own model exclusions.
type policyExecutor struct {
	Variant string   `toml:"variant"`
	Never   []string `toml:"never"`
	Reason  string   `toml:"reason"`
}

// policySecurity is `[security]`: the sensitivity classifier (by seat name or
// issue label) and what a sensitive row may never resolve to.
type policySecurity struct {
	Ceiling string   `toml:"ceiling"`
	Labels  []string `toml:"labels"`
	Never   []string `toml:"never"`
	Nodes   []string `toml:"nodes"`
	Reason  string   `toml:"reason"`
}

// policyEscalation is `[escalation]` plus its nested `[escalation.fallback]`
// substitute map.
type policyEscalation struct {
	OnFailure      string            `toml:"on_failure"`
	OnRound        string            `toml:"on_round"`
	RoundExecutors []string          `toml:"round_executors"`
	FableGates     []string          `toml:"fable_gates"`
	Fallback       map[string]string `toml:"fallback"`
}

// policyMeta is `[policy]`.
type policyMeta struct {
	Version int `toml:"version"`
}

// policyDoc is policy.toml's grammar, field for field.
type policyDoc struct {
	Policy     policyMeta                `toml:"policy"`
	Variants   map[string]policyVariant  `toml:"variants"`
	Executors  map[string]policyExecutor `toml:"executors"`
	Security   policySecurity            `toml:"security"`
	Escalation policyEscalation          `toml:"escalation"`
}

// parsePolicy decodes policy.toml and applies wave.js's assertPolicyShape
// gate: a version below the floor, or an empty [executors]/[variants], is a
// hard refusal rather than a partial policy silently routing nothing.
func parsePolicy(src []byte) (*policyDoc, error) {
	var doc policyDoc
	if _, err := toml.Decode(string(src), &doc); err != nil {
		return nil, fmt.Errorf("parsing policy.toml: %w", err)
	}
	if doc.Policy.Version < policyVersionFloor {
		return nil, fmt.Errorf(
			"policy.toml: [policy].version must be >= %d, got %d",
			policyVersionFloor, doc.Policy.Version)
	}
	if len(doc.Executors) == 0 {
		return nil, fmt.Errorf("policy.toml: [executors] is empty")
	}
	if len(doc.Variants) == 0 {
		return nil, fmt.Errorf("policy.toml: [variants] is empty")
	}
	return &doc, nil
}
