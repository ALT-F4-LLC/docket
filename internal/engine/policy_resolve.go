package engine

import (
	"fmt"
	"regexp"
)

// PolicyAssignment is the {model, effort, variant} triple ResolveExecutor and
// ResolveSeat compute (DKT-1282 AC1) — `next --run`'s wire shape for it.
type PolicyAssignment struct {
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Variant string `json:"variant,omitempty"`
}

// investigatorClassExecutors is wave.js's INVESTIGATOR_CLASS: the seats the
// "investigator-class" fable-gate exempts from the post-walk Fable redirect.
var investigatorClassExecutors = []string{"investigate", "research"}

// roundOrdinal is wave.js's round-parsing regex: a trailing `@N` or `@N#k` on
// an instance name (`fix@3`, `review@2#1`) names round N.
var roundOrdinal = regexp.MustCompile(`@(\d+)(?:#\d+)?$`)

// ResolveExecutor resolves one executor row's {model, effort, variant} — a
// port of wave.js's resolve(): the row's [executors] entry, walked forward
// through [variants].escalate_to by (attempt + round) hops, redirected around
// any [security]-forbidden model, and clamped to [security].ceiling on a
// sensitive row.
//
// attempt is the row's Attempt field verbatim — 0 on a step never claimed,
// N after N prior claims, matching wave.js's row.attempt exactly (DKT-1282
// AC2: "attempt N resolves to the ladder's Nth variant, never revisiting an
// abandoned one" is this loop's forward-only walk). instance is the step's
// instance name, for round parsing. labels is the issue's snapshotted labels.
func (p *policyDoc) ResolveExecutor(hint string, attempt int, instance string, labels []string) (PolicyAssignment, error) {
	found, ok := p.Executors[hint]
	if !ok {
		return PolicyAssignment{}, fmt.Errorf("executor hint %q has no [executors] row", hint)
	}

	variant := found.Variant
	if _, ok := p.Variants[variant]; !ok {
		return PolicyAssignment{}, fmt.Errorf(
			"[executors].%s names variant %q, which has no [variants] row", hint, variant)
	}
	never := append([]string(nil), found.Never...)

	sensitive := p.isSensitive(hint, labels)
	if sensitive {
		never = append(never, p.Security.Never...)
	}
	ceiling := ""
	if sensitive {
		ceiling = p.Security.Ceiling
	}
	beyond, err := p.beyondCeiling(ceiling)
	if err != nil {
		return PolicyAssignment{}, err
	}
	if ceiling != "" && beyond[variant] {
		variant = ceiling
	}
	standing := variant

	if attempt < 0 {
		attempt = 0
	}
	hops := attempt + p.roundHops(hint, instance)

	for range hops {
		cur := p.Variants[variant]
		if cur.EscalateTo == "" {
			// Top of the chain: excess hops are absorbed, not an error.
			break
		}
		next, ok := p.Variants[cur.EscalateTo]
		if !ok {
			return PolicyAssignment{}, fmt.Errorf(
				"[variants].%s names escalate_to %q, which has no [variants] row",
				variant, cur.EscalateTo)
		}
		if ceiling != "" && beyond[cur.EscalateTo] {
			variant = ceiling
			break
		}
		if contains(never, next.Model) {
			redirected, stuck := p.fallbackRedirect(cur.EscalateTo, never, variant)
			if stuck {
				break
			}
			if ceiling != "" && beyond[redirected] {
				variant = ceiling
				break
			}
			variant = redirected
			continue
		}
		variant = cur.EscalateTo
	}

	// Post-walk fable-gate check: a walk that moved off `standing` and landed
	// on a fable variant is redirected once more unless one of
	// [escalation].fable_gates exempts this row.
	if variant != standing && p.Variants[variant].Model == "fable" &&
		!p.fableEligible(hint, attempt, standing, labels) {
		if fb, ok := p.Escalation.Fallback[variant]; ok {
			if _, ok := p.Variants[fb]; ok {
				variant = fb
			}
		}
	}

	variant, spec, err := p.finalize(variant, never)
	if err != nil {
		return PolicyAssignment{}, fmt.Errorf("resolving %q: %w", hint, err)
	}
	return PolicyAssignment{Model: spec.Model, Effort: spec.Effort, Variant: variant}, nil
}

// ResolveSeat resolves one vote step's voter to {model, effort, variant} — a
// port of wave.js's resolveSeat(): the seat's declared STANDING variant only
// (a vote seat has no attempt or round to walk), clamped and redirected by
// the same [security] rules ResolveExecutor applies.
func (p *policyDoc) ResolveSeat(seat string, labels []string) (PolicyAssignment, error) {
	found, ok := p.Executors[seat]
	if !ok {
		return PolicyAssignment{}, fmt.Errorf("voter %q has no [executors] row", seat)
	}

	variant := found.Variant
	if _, ok := p.Variants[variant]; !ok {
		return PolicyAssignment{}, fmt.Errorf(
			"[executors].%s names variant %q, which has no [variants] row", seat, variant)
	}
	never := append([]string(nil), found.Never...)

	sensitive := p.isSensitive(seat, labels)
	if sensitive {
		never = append(never, p.Security.Never...)
	}
	ceiling := ""
	if sensitive {
		ceiling = p.Security.Ceiling
	}
	beyond, err := p.beyondCeiling(ceiling)
	if err != nil {
		return PolicyAssignment{}, err
	}
	if ceiling != "" && beyond[variant] {
		variant = ceiling
	}

	variant, spec, err := p.finalize(variant, never)
	if err != nil {
		return PolicyAssignment{}, fmt.Errorf("resolving voter %q: %w", seat, err)
	}
	return PolicyAssignment{Model: spec.Model, Effort: spec.Effort, Variant: variant}, nil
}

// isSensitive reports whether a row is subject to [security]'s ceiling and
// merged never-list: named directly in [security].nodes, or carrying any of
// [security].labels.
func (p *policyDoc) isSensitive(name string, labels []string) bool {
	if contains(p.Security.Nodes, name) {
		return true
	}
	for _, l := range p.Security.Labels {
		if contains(labels, l) {
			return true
		}
	}
	return false
}

// beyondCeiling is every variant name reachable FORWARD from `ceiling` via
// escalate_to — the set a ceiling-bound row must never resolve into, clamped
// back to `ceiling` itself instead. Empty ceiling means no bound at all.
func (p *policyDoc) beyondCeiling(ceiling string) (map[string]bool, error) {
	beyond := make(map[string]bool)
	if ceiling == "" {
		return beyond, nil
	}
	c, ok := p.Variants[ceiling]
	if !ok {
		return nil, fmt.Errorf("[security].ceiling names variant %q, which has no [variants] row", ceiling)
	}
	for c.EscalateTo != "" && !beyond[c.EscalateTo] {
		beyond[c.EscalateTo] = true
		next, ok := p.Variants[c.EscalateTo]
		if !ok {
			break
		}
		c = next
	}
	return beyond, nil
}

// fallbackRedirect is the per-hop never-list substitute: the hop that would
// have landed on `target` (a variant name) is redirected through
// [escalation.fallback][target] instead. stuck is true when no usable
// fallback exists — the walk stops in place rather than landing on a
// forbidden model or looping.
func (p *policyDoc) fallbackRedirect(target string, never []string, current string) (redirected string, stuck bool) {
	fb, ok := p.Escalation.Fallback[target]
	if !ok {
		return "", true
	}
	fbSpec, ok := p.Variants[fb]
	if !ok || contains(never, fbSpec.Model) || fb == current {
		return "", true
	}
	return fb, false
}

// roundHops is wave.js's roundHops(): 0 unless [escalation].on_round is
// literally "one-hop", `hint` is listed in [escalation].round_executors, and
// `instance` carries a round ordinal — in which case round N contributes N-1
// hops (a fresh round always starts a step at attempt 0, so without this a
// multi-round fix-loop would never escalate).
func (p *policyDoc) roundHops(hint, instance string) int {
	if p.Escalation.OnRound != "one-hop" {
		return 0
	}
	if !contains(p.Escalation.RoundExecutors, hint) {
		return 0
	}
	m := roundOrdinal.FindStringSubmatch(instance)
	if m == nil {
		return 0
	}
	round := 0
	for _, c := range m[1] {
		round = round*10 + int(c-'0')
	}
	if round > 1 {
		return round - 1
	}
	return 0
}

// fableEligible is wave.js's fableEligible(): whether a row that walked onto
// a Fable variant is exempt from the post-walk redirect back off it.
func (p *policyDoc) fableEligible(hint string, attempt int, standing string, labels []string) bool {
	for _, gate := range p.Escalation.FableGates {
		switch gate {
		case "investigator-class":
			if contains(investigatorClassExecutors, hint) {
				return true
			}
		case "novel-architecture":
			if contains(labels, "novel-architecture") {
				return true
			}
		case "failed-top-opus-round":
			if spec, ok := p.Variants[standing]; ok && attempt > 0 &&
				spec.Model == "opus" && (spec.Effort == "xhigh" || spec.Effort == "max") {
				return true
			}
		}
	}
	return false
}

// finalize is the last-resort safety net both resolvers end on: if the
// landed variant's model is still forbidden, one more redirect through
// [escalation.fallback]; a policy that leaves no permitted model reachable is
// a hard refusal rather than a silently forbidden model on the wire. It
// returns the FINAL variant name alongside its spec, since a redirect here
// changes which variant the row actually resolved to.
func (p *policyDoc) finalize(variant string, never []string) (string, policyVariant, error) {
	spec, ok := p.Variants[variant]
	if !ok {
		return "", policyVariant{}, fmt.Errorf("variant %q has no [variants] row", variant)
	}
	if !contains(never, spec.Model) {
		return variant, spec, nil
	}
	fb, ok := p.Escalation.Fallback[variant]
	if !ok {
		return "", policyVariant{}, fmt.Errorf("no permitted model reachable from %q", variant)
	}
	fbSpec, ok := p.Variants[fb]
	if !ok || contains(never, fbSpec.Model) {
		return "", policyVariant{}, fmt.Errorf("no permitted model reachable from %q", variant)
	}
	return fb, fbSpec, nil
}
