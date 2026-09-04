package engine

import (
	"strconv"
	"testing"
)

// TestPolicyResolveMatchesWaveEscalationWalk ports the table and structural
// assertions from dotfiles' tests/wave-escalation-walk.test.sh (DKT-1282 AC2)
// against the SAME policy content (src/user/docket/config/policy.toml,
// semantics preserved, doc comments stripped since they carry no behavior) —
// so a step's attempt-based escalation ladder resolves identically here to
// what that suite asserts of wave.js's resolve().
//
// Fixture note: escalationWalkPolicy below carries policy.toml's
// [variants]/[security]/[escalation] tables VERBATIM (the chain topology and
// the security config, both unmoved since the JS suite's TABLE was written),
// and an [executors] table whose seats stand at the SAME tiers the JS
// suite's own TABLE/ROUNDS values assert of them — the real policy.toml has
// since down-tiered several of those seats (fix and implement moved off
// sonnet-medium, investigate/threat-model/tdd-author-security/
// tribunal-security moved off their higher standing tiers), which is
// authoring drift in a document read-only to this repo, not a defect in
// either the JS suite or this port: the JS suite's own comment already
// anticipates it ("the assertions are meant to fail loudly when a policy
// edit moves a cell"). This fixture is pinned to the tiers the JS TABLE
// documents so the two stay comparable; it is not a live mirror of
// dotfiles' policy.toml and is not expected to track its future edits.
const escalationWalkPolicy = `
[policy]
version = 22

[variants]
fable-high = { model = "fable", effort = "high" }
fable-max = { model = "fable", effort = "max" }
fable-medium = { model = "fable", effort = "medium" }
fable-xhigh = { model = "fable", effort = "xhigh" }
opus-high = { model = "opus", effort = "high", escalate_to = "fable-high" }
opus-medium = { model = "opus", effort = "medium", escalate_to = "fable-medium" }
opus-xhigh  = { model = "opus", effort = "xhigh", escalate_to = "fable-xhigh" }
opus-max = { model = "opus", effort = "max", escalate_to = "fable-max" }
sonnet-high = { model = "sonnet", effort = "high", escalate_to = "opus-high" }
sonnet-max = { model = "sonnet", effort = "max", escalate_to = "opus-max" }
sonnet-medium = { model = "sonnet", effort = "medium", escalate_to = "opus-medium" }
sonnet-xhigh = { model = "sonnet", effort = "xhigh", escalate_to = "opus-xhigh" }

[executors]
adr-author = { variant = "fable-high" }
design-qa = { variant = "opus-high" }
dispose = { variant = "opus-high" }
drain-highs = { variant = "sonnet-medium" }
fix = { variant = "sonnet-medium" }
implement = { variant = "sonnet-medium" }
investigate = { variant = "fable-xhigh" }
judge-architecture = { variant = "opus-high" }
judge-correctness = { variant = "opus-high" }
judge-design = { variant = "opus-high" }
judge-security = { variant = "opus-high", never = ["fable"], reason = "security-classifier-reroute" }
judge-simplicity = { variant = "opus-medium" }
judge-testing = { variant = "opus-high" }
prd-author = { variant = "fable-medium" }
research = { variant = "fable-medium" }
spec-author-architecture = { variant = "opus-high" }
spec-author-code-quality = { variant = "opus-high" }
spec-author-operations = { variant = "opus-high" }
spec-author-performance = { variant = "opus-high" }
spec-author-review-strategy = { variant = "opus-high" }
spec-author-security = { variant = "opus-high", never = ["fable"], reason = "security-classifier-reroute" }
spec-author-testing = { variant = "opus-high" }
synthesize-findings = { variant = "sonnet-medium" }
tdd-author = { variant = "fable-high" }
tdd-author-security = { variant = "opus-xhigh", never = ["fable"], reason = "security-classifier-reroute" }
threat-model = { variant = "opus-xhigh", never = ["fable"], reason = "security-classifier-reroute" }
tribunal-architecture = { variant = "fable-high" }
tribunal-correctness = { variant = "fable-high" }
tribunal-design = { variant = "fable-high" }
tribunal-security = { variant = "opus-xhigh", never = ["fable"], reason = "security-classifier-reroute" }
ux-spec-author = { variant = "fable-medium" }
verify-ac = { variant = "opus-high" }

[security]
ceiling = "opus-max"
labels = ["security-change", "security-load-bearing", "security"]
never = ["fable"]
nodes = ["judge-security", "threat-model", "tdd-author-security", "spec-author-security", "tribunal-security"]
reason = "..."

[escalation]
on_failure = "one-hop"
on_round = "one-hop"
round_executors = ["fix", "dispose"]
fable_gates = [
  "failed-top-opus-round",
  "investigator-class",
  "novel-architecture",
]

[escalation.fallback]
fable-high = "opus-xhigh"
fable-max = "opus-max"
fable-medium = "opus-high"
fable-xhigh = "opus-max"
`

func mustParseEscalationWalkPolicy(t *testing.T) *policyDoc {
	t.Helper()
	doc, err := parsePolicy([]byte(escalationWalkPolicy))
	if err != nil {
		t.Fatalf("parsing the escalation-walk fixture: %v", err)
	}
	return doc
}

func variantAt(t *testing.T, doc *policyDoc, executor string, attempt int, labels []string) string {
	t.Helper()
	got, err := doc.ResolveExecutor(executor, attempt, "", labels)
	if err != nil {
		return "THREW: " + err.Error()
	}
	return got.Variant
}

func modelAt(t *testing.T, doc *policyDoc, executor string, attempt int, labels []string) string {
	t.Helper()
	got, err := doc.ResolveExecutor(executor, attempt, "", labels)
	if err != nil {
		return "THREW: " + err.Error()
	}
	return got.Model
}

func variantAtRound(t *testing.T, doc *policyDoc, executor, instance string, attempt int, labels []string) string {
	t.Helper()
	got, err := doc.ResolveExecutor(executor, attempt, instance, labels)
	if err != nil {
		return "THREW: " + err.Error()
	}
	return got.Variant
}

// TestEscalationWalkTable is the JS suite's TABLE, ported verbatim: one row
// per (executor, labels), one cell per attempt 0..5.
func TestEscalationWalkTable(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)

	cases := []struct {
		executor string
		labels   []string
		want     [6]string
	}{
		{"fix", nil, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-high", "opus-high", "opus-high"}},
		{"implement", nil, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-high", "opus-high", "opus-high"}},
		{"fix", []string{"security"}, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "opus-max"}},
		{"implement", []string{"security-change"}, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "opus-max"}},
		{"threat-model", nil, [6]string{"opus-xhigh", "opus-max", "opus-max", "opus-max", "opus-max", "opus-max"}},
		{"tdd-author-security", nil, [6]string{"opus-xhigh", "opus-max", "opus-max", "opus-max", "opus-max", "opus-max"}},
		{"tribunal-security", nil, [6]string{"opus-xhigh", "opus-max", "opus-max", "opus-max", "opus-max", "opus-max"}},
		{"judge-security", nil, [6]string{"opus-high", "opus-xhigh", "opus-max", "opus-max", "opus-max", "opus-max"}},
		{"investigate", nil, [6]string{"fable-xhigh", "fable-xhigh", "fable-xhigh", "fable-xhigh", "fable-xhigh", "fable-xhigh"}},
		{"design-qa", nil, [6]string{"opus-high", "opus-xhigh", "opus-xhigh", "opus-xhigh", "opus-xhigh", "opus-xhigh"}},
		{"judge-simplicity", nil, [6]string{"opus-medium", "opus-high", "opus-high", "opus-high", "opus-high", "opus-high"}},
		{"synthesize-findings", []string{"novel-architecture"}, [6]string{"sonnet-medium", "opus-medium", "fable-medium", "fable-medium", "fable-medium", "fable-medium"}},
	}

	for _, tc := range cases {
		for attempt, want := range tc.want {
			got := variantAt(t, doc, tc.executor, attempt, tc.labels)
			if got != want {
				t.Errorf("%s%v attempt:%d = %q, want %q", tc.executor, tc.labels, attempt, got, want)
			}
		}
	}
}

// TestEscalationWalkFirstRetryIsOneHop is the off-by-one regression fence
// (DOT-651): attempt:1 must be exactly one escalate_to hop off standing (or
// its fallback redirect), and must never re-run at standing.
func TestEscalationWalkFirstRetryIsOneHop(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	for _, hint := range []string{"implement", "fix", "judge-simplicity"} {
		standing := doc.Executors[hint].Variant
		oneHop := doc.Variants[standing].EscalateTo
		fb := doc.Escalation.Fallback[oneHop]
		landed := variantAt(t, doc, hint, 1, nil)
		if landed != oneHop && landed != fb {
			t.Errorf("%s attempt:1 = %q, want one hop off %s (%s) or its fallback (%s)",
				hint, landed, standing, oneHop, fb)
		}
		if landed == standing {
			t.Errorf("%s attempt:1 re-ran at its standing variant %s (the off-by-one)", hint, standing)
		}
	}
}

// TestEscalationWalkAttemptZeroIsStanding: a step never claimed resolves at
// its declared standing variant — the Go equivalent of the JS suite's
// "missing attempt field" case, since model.StepRow.Attempt has no undefined
// state; its zero value already means "never claimed".
func TestEscalationWalkAttemptZeroIsStanding(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	for _, hint := range []string{"implement", "judge-security", "investigate", "design-qa"} {
		want := doc.Executors[hint].Variant
		if got := variantAt(t, doc, hint, 0, nil); got != want {
			t.Errorf("%s attempt:0 = %q, want its standing variant %q", hint, got, want)
		}
	}
	if got := variantAt(t, doc, "fix", 0, []string{"security"}); got != doc.Executors["fix"].Variant {
		t.Errorf("fix (security) attempt:0 = %q, want its standing variant", got)
	}
}

// TestEscalationWalkNeverListedExecutorsNeverReachFable is the DOT-650 fence:
// a [security].nodes/[security].never member climbs through the fallback and
// caps at the ceiling, never resolving to the forbidden model, well past
// where the chain runs out of hops.
func TestEscalationWalkNeverListedExecutorsNeverReachFable(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	for attempt := 0; attempt <= 12; attempt++ {
		if got := modelAt(t, doc, "judge-security", attempt, nil); got == "fable" {
			t.Errorf("judge-security attempt:%d resolved to fable", attempt)
		}
	}

	ceiling := doc.Security.Ceiling
	for _, hint := range []string{
		"judge-security", "threat-model", "tdd-author-security",
		"spec-author-security", "tribunal-security",
	} {
		if got := variantAt(t, doc, hint, 12, nil); got != ceiling {
			t.Errorf("%s attempt:12 = %q, want the ceiling %q", hint, got, ceiling)
		}
	}
	if got := variantAt(t, doc, "fix", 12, []string{"security"}); got != ceiling {
		t.Errorf("fix (security) attempt:12 = %q, want the ceiling %q", got, ceiling)
	}
	if got := variantAt(t, doc, "implement", 12, []string{"security-change"}); got != ceiling {
		t.Errorf("implement (security-change) attempt:12 = %q, want the ceiling %q", got, ceiling)
	}
}

// TestEscalationWalkFableStandingRowsDoNotMove: a row whose STANDING variant
// is already Fable has no escalate_to to walk and the fable gate is only
// consulted when the walk actually moved off standing, so it never moves.
func TestEscalationWalkFableStandingRowsDoNotMove(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	for hint, exec := range doc.Executors {
		standing, ok := doc.Variants[exec.Variant]
		if !ok || standing.Model != "fable" {
			continue
		}
		for _, attempt := range []int{0, 1, 2, 7} {
			if got := variantAt(t, doc, hint, attempt, nil); got != exec.Variant {
				t.Errorf("fable-standing %s attempt:%d = %q, want %q (stays put)", hint, attempt, got, exec.Variant)
			}
		}
	}
}

// TestEscalationWalkRoundsComposeWithAttempt ports the ROUNDS/DISPOSE_ROUNDS
// tables (DOT-724): a fix-loop round is a fresh step id at attempt 0, so
// [escalation].on_round/round_executors is what makes consecutive rounds
// climb the chain instead of re-running at standing forever, and round hops
// compose with attempt hops when a round also burns a claim.
func TestEscalationWalkRoundsComposeWithAttempt(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)

	fixRounds := []struct {
		labels []string
		want   [6]string
	}{
		{nil, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-high", "opus-high", "opus-high"}},
		{[]string{"security-load-bearing"}, [6]string{"sonnet-medium", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "opus-max"}},
	}
	for _, tc := range fixRounds {
		for i, want := range tc.want {
			round := i + 1
			instance := fmtRoundInstance("fix", round)
			got := variantAtRound(t, doc, "fix", instance, 0, tc.labels)
			if got != want {
				t.Errorf("fix%v %s attempt:0 = %q, want %q", tc.labels, instance, got, want)
			}
		}
	}

	if got := variantAtRound(t, doc, "fix", "fix@2", 1, nil); got != "opus-high" {
		t.Errorf("fix@2 attempt:1 (round+attempt compose) = %q, want opus-high", got)
	}

	disposeRounds := []struct {
		labels []string
		want   [6]string
	}{
		{nil, [6]string{"opus-high", "opus-xhigh", "opus-xhigh", "opus-xhigh", "opus-xhigh", "opus-xhigh"}},
		{[]string{"security-load-bearing"}, [6]string{"opus-high", "opus-xhigh", "opus-max", "opus-max", "opus-max", "opus-max"}},
	}
	for _, tc := range disposeRounds {
		for i, want := range tc.want {
			round := i + 1
			instance := fmtRoundInstance("dispose", round)
			got := variantAtRound(t, doc, "dispose", instance, 0, tc.labels)
			if got != want {
				t.Errorf("dispose%v %s attempt:0 = %q, want %q", tc.labels, instance, got, want)
			}
		}
	}
	if got := variantAtRound(t, doc, "dispose", "dispose@2", 1, nil); got != "opus-xhigh" {
		t.Errorf("dispose@2 attempt:1 (round+attempt compose) = %q, want opus-xhigh", got)
	}
	if got := variantAtRound(t, doc, "dispose", "", 0, nil); got != "opus-high" {
		t.Errorf("dispose with no instance attempt:0 = %q, want its standing variant opus-high", got)
	}
	if got := variantAtRound(t, doc, "fix", "", 0, nil); got != "sonnet-medium" {
		t.Errorf("fix with no instance attempt:0 = %q, want its standing variant sonnet-medium", got)
	}
}

// TestEscalationWalkRoundScopeIsPerExecutor: every per-round step of a loop
// shares the round's instance ordinal, but only [escalation].round_executors
// members move on it — a judge or synthesize step reviewing round 9 stands
// exactly where it stood at round 0.
func TestEscalationWalkRoundScopeIsPerExecutor(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	cases := []struct {
		executor, instance string
		attempt            int
		want               string
	}{
		{"judge-correctness", "review@9#2", 0, "opus-high"},
		{"judge-security", "review@9#3", 0, "opus-high"},
		{"synthesize-findings", "synthesize@9", 0, "sonnet-medium"},
		{"verify-ac", "verify@9", 0, "opus-high"},
		{"implement", "implement@0", 0, "sonnet-medium"},
		{"implement", "implement@0", 1, "opus-medium"},
	}
	for _, tc := range cases {
		got := variantAtRound(t, doc, tc.executor, tc.instance, tc.attempt, nil)
		if got != tc.want {
			t.Errorf("%s %s attempt:%d = %q, want %q", tc.executor, tc.instance, tc.attempt, got, tc.want)
		}
	}
}

// TestEscalationWalkNeverRevisitsAnAbandonedVariant is DKT-1282 AC2's own
// wording, checked directly: the walk is monotone in attempt — it may hold at
// a tier across several attempts, but it must never fall back to one it has
// already left.
func TestEscalationWalkNeverRevisitsAnAbandonedVariant(t *testing.T) {
	doc := mustParseEscalationWalkPolicy(t)
	cases := []struct {
		hint   string
		labels []string
	}{
		{"implement", nil},
		{"fix", []string{"security"}},
		{"judge-security", nil},
	}
	for _, tc := range cases {
		var seen []string
		for attempt := 0; attempt <= 8; attempt++ {
			v := variantAt(t, doc, tc.hint, attempt, tc.labels)
			if len(seen) > 0 {
				last := seen[len(seen)-1]
				if last != v {
					for _, prior := range seen {
						if prior == v && prior != last {
							t.Errorf("%s%v revisited abandoned variant %q at attempt %d: %v",
								tc.hint, tc.labels, v, attempt, append(seen, v))
						}
					}
				}
			}
			seen = append(seen, v)
		}
	}
}

func fmtRoundInstance(executor string, round int) string {
	return executor + "@" + strconv.Itoa(round)
}
