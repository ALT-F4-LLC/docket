package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"

	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// payloadSet decodes a JSON array literal into the payload shape a threshold
// aggregates over, so the cases below read as the data an author would write.
func payloadSet(t *testing.T, raw string) []map[string]any {
	t.Helper()
	if raw == "" {
		return nil
	}
	var out []map[string]any
	err := json.Unmarshal([]byte(raw), &out)
	testsupport.Must(t, err, "decoding payload %s: %v", raw, err)
	return out
}

// evalOne evaluates a single-routing threshold with NO resolver — the S3 shape,
// which is T3 case (i): a step declaring no `payload`.
//
// Every S3-era case below still calls it, and that is the point: the survival
// suite is not a re-implementation of the old table, it IS the old table,
// evaluated by the new engine with nothing passed in.
func evalOne(t *testing.T, predicate, raw string) ThresholdResult {
	t.Helper()
	return evalWith(t, nil, predicate, raw)
}

// evalWith is evalOne under a declared order.
func evalWith(
	t *testing.T, order OrderResolver, predicate, raw string,
) ThresholdResult {
	t.Helper()
	threshold := map[string]string{workflow.OnFailFixLoop: predicate}
	result, err := EvaluateThreshold(
		"step@0", threshold, ThresholdOrder(threshold), payloadSet(t, raw), order)
	testsupport.Must(t, err, "EvaluateThreshold(%q): %v", predicate, err)
	return result
}

// fakeOrder is a resolver built from literal declarations, so the ordered cases
// below read as the schema an author would register and need no database.
//
// It implements OrderResolver by hand rather than compiling a document, because
// what these tests exercise is the EVALUATOR's use of an order — internal/schema
// has its own tables for the derivation.
type fakeOrder struct {
	order map[string][]string
	types map[string]string
	// conservative is the declared `conservative_end` per field. Empty for
	// every fixture but the aggregate tie-break's own, which is the point:
	// declaring nothing is the overwhelmingly common case and it must keep
	// behaving exactly as it did before the key existed.
	conservative map[string]string
}

func (f fakeOrder) Position(field, value string) (int, bool) {
	values, ok := f.order[field]
	if !ok {
		return 0, false
	}
	for i, v := range values {
		if v == value {
			return i, true
		}
	}
	return 0, false
}

func (f fakeOrder) Ordered(field string) bool {
	_, ok := f.order[field]
	return ok
}

func (f fakeOrder) FieldType(field string) string { return f.types[field] }

func (f fakeOrder) Conservative(field string) string { return f.conservative[field] }

// severityOrder is the fixture's own five-value order, used by the T2 cases.
// `severity` is an INSTANCE TOKEN here exactly as it is in the committed
// fixture: core learns the order because this table declares it.
var severityOrder = fakeOrder{
	order: map[string][]string{
		"severity": {"info", "low", "medium", "high", "blocker"},
	},
	types: map[string]string{"severity": "string"},
}

// ---------------------------------------------------------------------------
// T1 — equality evaluates at S3
// ---------------------------------------------------------------------------

// TestThresholdT1Equality is §6.14 T1: `==` and `!=` compare as strings after
// JSON scalar normalization. No schema is required to know whether two values
// are the same value.
func TestThresholdT1Equality(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		payloads  string
		want      string
		why       string
	}{
		{
			name:      "any equality matches",
			predicate: "any(status == unmet)",
			payloads:  `[{"status":"met"},{"status":"unmet"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "any equality with no match passes",
			predicate: "any(status == unmet)",
			payloads:  `[{"status":"met"},{"status":"met"}]`,
			want:      RoutingPass,
			why:       "§11.2: no match ⇒ pass",
		},
		{
			name:      "all equality holds",
			predicate: "all(status == met)",
			payloads:  `[{"status":"met"},{"status":"met"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "all equality fails on one dissenter",
			predicate: "all(status == met)",
			payloads:  `[{"status":"met"},{"status":"unmet"}]`,
			want:      RoutingPass,
		},
		{
			name:      "inequality matches a different value",
			predicate: "any(status != met)",
			payloads:  `[{"status":"unmet"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "count>=n reaches its threshold",
			predicate: "count>=2(status == unmet)",
			payloads:  `[{"status":"unmet"},{"status":"unmet"},{"status":"met"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "count>=n falls short",
			predicate: "count>=3(status == unmet)",
			payloads:  `[{"status":"unmet"},{"status":"unmet"},{"status":"met"}]`,
			want:      RoutingPass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalOne(t, tc.predicate, tc.payloads)
			if result.Routing != tc.want {
				t.Errorf("routing = %q, want %q — %s", result.Routing, tc.want, tc.why)
			}
			if result.Parked {
				t.Error("an equality comparison parked; T1 makes it evaluable at S3")
			}
		})
	}
}

// TestThresholdT1ScalarNormalization is T1's normalization clause: numbers
// canonicalized, booleans `true`/`false`.
//
// The number case is the one that bites: encoding/json decodes every number to
// float64, so `{"count": 1}` would render as "1e+00" under a naive fmt and
// match nothing an author would write.
func TestThresholdT1ScalarNormalization(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		payloads  string
		want      bool
	}{
		{"integer matches its literal", "any(count == 1)", `[{"count":1}]`, true},
		{"float 1.0 matches the literal 1", "any(count == 1)", `[{"count":1.0}]`, true},
		{"fractional value matches", "any(ratio == 0.5)", `[{"ratio":0.5}]`, true},
		{"a different number does not match", "any(count == 1)", `[{"count":2}]`, false},
		{"boolean true", "any(ok == true)", `[{"ok":true}]`, true},
		{"boolean false", "any(ok == false)", `[{"ok":false}]`, true},
		{"boolean mismatch", "any(ok == true)", `[{"ok":false}]`, false},
		{"negative number", "any(delta == -3)", `[{"delta":-3}]`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalOne(t, tc.predicate, tc.payloads)
			matched := result.Routing == workflow.OnFailFixLoop
			if matched != tc.want {
				t.Errorf("%s over %s matched = %v, want %v",
					tc.predicate, tc.payloads, matched, tc.want)
			}
		})
	}
}

// TestThresholdT1NullNeverEqualsAnything is T1's null rule, stated exactly:
// null is never equal to anything INCLUDING null.
//
// The `!=` half is the counter-intuitive one and is asserted deliberately: "not
// equal" asserts two values differ, and there is no value present to differ, so
// it is false too. Treating an absence as unequal-to-everything would make
// `any(field != x)` true for a payload set that never mentioned the field.
func TestThresholdT1NullNeverEqualsAnything(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		payloads  string
	}{
		{"null == a literal", "any(status == met)", `[{"status":null}]`},
		{"null == null literal", "any(status == null)", `[{"status":null}]`},
		{"null != a literal", "any(status != met)", `[{"status":null}]`},
		{"an ABSENT field == a literal", "any(status == met)", `[{"other":"x"}]`},
		{"an ABSENT field != a literal", "any(status != met)", `[{"other":"x"}]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalOne(t, tc.predicate, tc.payloads)
			if result.Routing != RoutingPass {
				t.Errorf("%s over %s routed %q, want %q — null is never equal to "+
					"anything, including null, and an absent field is the same fact",
					tc.predicate, tc.payloads, result.Routing, RoutingPass)
			}
		})
	}

	// And `all` over a set whose only element is null is FALSE, not vacuously
	// true: the element exists and does not satisfy the predicate.
	result := evalOne(t, "all(status == met)", `[{"status":null}]`)
	if result.Routing != RoutingPass {
		t.Errorf("all(status == met) over [null] routed %q; the element exists "+
			"and fails, so the aggregation is false", result.Routing)
	}
}

// ---------------------------------------------------------------------------
// T2/T3 — ordered comparisons do not evaluate, and are never guessed
// ---------------------------------------------------------------------------

// TestThresholdT3ParksOnAnOrderedComparison is §6.14 T3: an ordered comparison
// ACTUALLY ATTEMPTED against a schema-less field parks `waiting-human` with a
// recorded reason.
//
// The reason must name the step, the routing key, the predicate verbatim, and
// the cause — asserted by substring, per §6.16 — so an operator resolving the
// park can see exactly what could not be decided.
func TestThresholdT3ParksOnAnOrderedComparison(t *testing.T) {
	for _, op := range []string{">=", ">", "<=", "<"} {
		t.Run(op, func(t *testing.T) {
			predicate := "any(severity " + op + " high)"
			result := evalOne(t, predicate, `[{"severity":"low"}]`)

			if result.Routing != workflow.OnFailWaitingHuman {
				t.Fatalf("routing = %q, want %q — an undecidable ordered comparison "+
					"must PARK, never silently pass",
					result.Routing, workflow.OnFailWaitingHuman)
			}
			if !result.Parked {
				t.Error("Parked = false; the caller cannot distinguish this from a " +
					"threshold that deliberately routed waiting-human")
			}

			// "which stage 5 supplies" is GONE from this list, and its removal
			// is T3's own instruction rather than a relaxed assertion: stage 5
			// has shipped, and a message promising a future stage after that
			// stage has landed is worse than no message. What replaces it — the
			// remedy that is now actually available — is asserted by
			// TestT3NamesWhichOfItsThreeCases below.
			for _, want := range []string{
				"step@0",                   // the step
				workflow.OnFailFixLoop,     // the routing key
				"severity " + op + " high", // the predicate, verbatim
				"ordered_enum",             // the cause
				"§11.2",                    // the spec reference
				"declares no `payload`",    // WHICH of the three cases
			} {
				if !strings.Contains(result.Reason, want) {
					t.Errorf("the reason does not name %q:\n%s", want, result.Reason)
				}
			}
		})
	}
}

// TestThresholdNeverGuessesAnOrder is the negative half of T3, and the reason
// T3 exists: the engine must not invent an ordering in EITHER direction.
//
// A lexicographic implementation would rank "low" < "high" (l > h is false), an
// enum-declaration one might rank them the other way. Both are guesses, and a
// guessed order is a SILENT MISROUTE — strictly worse than a park an operator
// can see. The test asserts the park regardless of which way a guess would fall.
func TestThresholdNeverGuessesAnOrder(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		payloads  string
	}{
		{
			name:      "a lexicographic guess would say TRUE",
			predicate: "any(severity >= high)",
			payloads:  `[{"severity":"low"}]`, // "low" >= "high" lexicographically
		},
		{
			name:      "a lexicographic guess would say FALSE",
			predicate: "any(severity >= low)",
			payloads:  `[{"severity":"high"}]`, // "high" >= "low" is false
		},
		{
			name:      "numeric fields are not exempt",
			predicate: "any(count >= 5)",
			payloads:  `[{"count":10}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalOne(t, tc.predicate, tc.payloads)
			if !result.Parked {
				t.Errorf("%s over %s did not park (routed %q) — the engine guessed "+
					"an order rather than refusing to",
					tc.predicate, tc.payloads, result.Routing)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T4 — the empty set, evaluated BEFORE T3
// ---------------------------------------------------------------------------

// TestThresholdT4EmptySet is §6.14 T4: the ordinary mathematical convention.
func TestThresholdT4EmptySet(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		want      string
		why       string
	}{
		{
			name: "any over zero payloads is FALSE", predicate: "any(status == unmet)",
			want: RoutingPass,
			why:  "no elements ⇒ no witness ⇒ false, whatever the predicate is",
		},
		{
			name: "all over zero payloads is TRUE", predicate: "all(status == met)",
			want: workflow.OnFailFixLoop,
			why:  "no elements ⇒ no violation ⇒ true, whatever the predicate is",
		},
		{
			name: "count>=0 over zero payloads is TRUE", predicate: "count>=0(status == met)",
			want: workflow.OnFailFixLoop,
			why:  "count>=n holds iff n == 0",
		},
		{
			name: "count>=1 over zero payloads is FALSE", predicate: "count>=1(status == met)",
			want: RoutingPass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalOne(t, tc.predicate, "")
			if result.Routing != tc.want {
				t.Errorf("routing = %q, want %q — %s", result.Routing, tc.want, tc.why)
			}
		})
	}
}

// TestThresholdT4PrecedesT3 is THE clause that makes S3 runnable, and the exact
// case phase 4's QA loop depends on.
//
// `any(severity >= high)` over an EMPTY payload set must return FALSE — not
// park. T4 short-circuits the aggregation before the ordered comparison is ever
// attempted, so no guess is made and no park occurs. This is the fixture's
// `reconcile` step at S3: its stub payload is empty (§6.13), so it flows through
// legally.
//
// If T3 were evaluated first this would park, the fixture's loop would never
// reach `verify`, and phase 4's QA would deadlock — so the ordering is asserted
// directly rather than inferred from the loop passing.
func TestThresholdT4PrecedesT3(t *testing.T) {
	result := evalOne(t, "any(severity >= high)", "")

	if result.Parked {
		t.Fatalf("an ordered comparison over an EMPTY payload set parked:\n%s\n\n"+
			"T4 must be evaluated BEFORE T3: `any(P)` over zero elements is false "+
			"without consulting P, so no ordered comparison is ever attempted. "+
			"This is the exact case the phase-4 QA loop depends on.", result.Reason)
	}
	if result.Routing != RoutingPass {
		t.Errorf("routing = %q, want %q", result.Routing, RoutingPass)
	}

	// The `all` counterpart: true over an empty set, again without consulting P.
	threshold := map[string]string{workflow.OnFailFixLoop: "all(severity >= high)"}
	allResult, err := EvaluateThreshold("step@0", threshold, ThresholdOrder(threshold), nil, nil)
	testsupport.Must(t, err, "EvaluateThreshold: %v", err)
	if allResult.Parked {
		t.Error("all(P) over an empty set parked; it is true without consulting P")
	}
	if allResult.Routing != workflow.OnFailFixLoop {
		t.Errorf("all over an empty set routed %q, want the match", allResult.Routing)
	}
}

// ---------------------------------------------------------------------------
// Ordering and the no-match default
// ---------------------------------------------------------------------------

// TestThresholdFirstMatchWins is §11.2's "evaluated top-to-bottom, first match
// routes", over a multi-routing table — the fixture's `verify` shape.
func TestThresholdFirstMatchWins(t *testing.T) {
	threshold := map[string]string{
		workflow.OnFailFixLoop:      "any(status == unmet)",
		workflow.OnFailWaitingHuman: "any(status == unverifiable)",
	}
	order := ThresholdOrder(threshold)

	// `fix-loop` is first in §11.2's sequence, so a payload matching BOTH routes
	// there.
	both := payloadSet(t, `[{"status":"unmet"},{"status":"unverifiable"}]`)
	result, err := EvaluateThreshold("verify@0", threshold, order, both, nil)
	testsupport.Must(t, err, "EvaluateThreshold: %v", err)
	if result.Routing != workflow.OnFailFixLoop {
		t.Errorf("routing = %q, want %q — first match wins",
			result.Routing, workflow.OnFailFixLoop)
	}

	// Only the second matches.
	onlySecond := payloadSet(t, `[{"status":"unverifiable"}]`)
	result, err = EvaluateThreshold("verify@0", threshold, order, onlySecond, nil)
	testsupport.Must(t, err, "EvaluateThreshold: %v", err)
	if result.Routing != workflow.OnFailWaitingHuman {
		t.Errorf("routing = %q, want %q", result.Routing, workflow.OnFailWaitingHuman)
	}

	// Neither matches ⇒ pass.
	neither := payloadSet(t, `[{"status":"met"}]`)
	result, err = EvaluateThreshold("verify@0", threshold, order, neither, nil)
	testsupport.Must(t, err, "EvaluateThreshold: %v", err)
	if result.Routing != RoutingPass {
		t.Errorf("routing = %q, want %q (§11.2: no match ⇒ pass)",
			result.Routing, RoutingPass)
	}
}

// TestThresholdOrderIsTotalAndStable pins that the evaluation order does not
// vary between runs. A first-match rule over a Go map's range order would route
// differently on two identical runs, which is a determinism failure that would
// surface as an unreproducible misroute rather than as a test failure.
func TestThresholdOrderIsTotalAndStable(t *testing.T) {
	threshold := map[string]string{
		workflow.OnFailWaitingHuman: "any(a == b)",
		workflow.OnFailFixLoop:      "any(c == d)",
		RoutingPass:                 "any(e == f)",
		"interposed-gate":           "any(g == h)",
		"another-gate":              "any(i == j)",
	}

	first := ThresholdOrder(threshold)
	for range 50 {
		again := ThresholdOrder(threshold)
		if len(again) != len(first) {
			t.Fatalf("order length varies: %v vs %v", first, again)
		}
		for i := range first {
			if again[i] != first[i] {
				t.Fatalf("order varies between calls: %v vs %v", first, again)
			}
		}
	}

	// The closed vocabulary comes first, in §11.2's own sequence.
	want := []string{workflow.OnFailFixLoop, workflow.OnFailWaitingHuman, RoutingPass}
	for i, routing := range want {
		if first[i] != routing {
			t.Errorf("order[%d] = %q, want %q — the closed routings precede "+
				"interposed step names", i, first[i], routing)
		}
	}
	// Then step names, sorted.
	if first[3] != "another-gate" || first[4] != "interposed-gate" {
		t.Errorf("interposed routings are not sorted: %v", first[3:])
	}
}

// TestParsePredicateSharesTheValidationGrammar pins that evaluation and V21 go
// through ONE regex. Two spellings of the same grammar would drift on the first
// operator someone added — a predicate V21 accepted and the evaluator rejected
// would be a workflow that registers and then wedges its run.
func TestParsePredicateSharesTheValidationGrammar(t *testing.T) {
	valid := []string{
		"any(severity >= high)",
		"all(status == met)",
		"count>=2(status != unmet)",
		"any(field.with.dots == x)",
		"  any( spaced == out )  ",
	}
	for _, src := range valid {
		if _, err := workflow.ParsePredicate(src); err != nil {
			t.Errorf("ParsePredicate(%q) = %v; V21 accepts this shape", src, err)
		}
	}

	invalid := []string{
		"any(severity)",
		"maybe(severity == high)",
		"any(severity ~= high)",
		"severity == high",
		"",
	}
	for _, src := range invalid {
		if _, err := workflow.ParsePredicate(src); err == nil {
			t.Errorf("ParsePredicate(%q) succeeded; V21 rejects this shape", src)
		}
	}

	// The decomposition itself.
	p, err := workflow.ParsePredicate("count>=3(severity >= high)")
	testsupport.Must(t, err, "ParsePredicate: %v", err)
	if p.Agg != workflow.AggCount || p.Count != 3 {
		t.Errorf("agg = %q count = %d, want count/3", p.Agg, p.Count)
	}
	if p.Field != "severity" || p.Op != workflow.OpGE || p.Literal != "high" {
		t.Errorf("decomposed to (%q %q %q), want (severity >= high)",
			p.Field, p.Op, p.Literal)
	}
	if !p.Ordered() {
		t.Error(">= is not reported as an ordered comparison")
	}
	if eq, _ := workflow.ParsePredicate("any(x == y)"); eq.Ordered() {
		t.Error("== is reported as an ordered comparison; equality needs no schema")
	}
}

// ---------------------------------------------------------------------------
// T2 — ordered comparisons come alive, over a USER-REGISTERED order (§5)
// ---------------------------------------------------------------------------

// TestThresholdT2EvaluatesOverPosition is the sentence this whole stage exists
// to make true: `any(severity >= high)` routes because a registered document
// said `high` comes after `medium`, and for no other reason.
//
// `a >= b` is `Position(field, a) >= Position(field, b)`. There is no other
// definition, and the cases below include the pair a lexicographic
// implementation gets backwards — "low" >= "high" is TRUE as strings and FALSE
// as positions — so an implementation that fell back to string comparison fails
// here rather than misrouting a real run.
func TestThresholdT2EvaluatesOverPosition(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		payloads  string
		want      string
		why       string
	}{
		{
			name:      "any >= matches at the literal",
			predicate: "any(severity >= high)",
			payloads:  `[{"severity":"low"},{"severity":"high"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "any >= matches above the literal",
			predicate: "any(severity >= high)",
			payloads:  `[{"severity":"blocker"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "any >= does not match below it",
			predicate: "any(severity >= high)",
			payloads:  `[{"severity":"low"},{"severity":"medium"}]`,
			want:      RoutingPass,
			why: "a lexicographic implementation would say low >= high and " +
				"route here; positions say otherwise",
		},
		{
			name:      "strict > excludes the literal itself",
			predicate: "any(severity > high)",
			payloads:  `[{"severity":"high"}]`,
			want:      RoutingPass,
		},
		{
			name:      "strict > matches above it",
			predicate: "any(severity > high)",
			payloads:  `[{"severity":"blocker"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "<= includes the literal",
			predicate: "all(severity <= medium)",
			payloads:  `[{"severity":"info"},{"severity":"medium"}]`,
			want:      workflow.OnFailFixLoop,
		},
		{
			name:      "< excludes it",
			predicate: "all(severity < medium)",
			payloads:  `[{"severity":"info"},{"severity":"medium"}]`,
			want:      RoutingPass,
		},
		{
			name:      "count>=n over positions",
			predicate: "count>=2(severity >= medium)",
			payloads: `[{"severity":"low"},{"severity":"medium"},` +
				`{"severity":"blocker"}]`,
			want: workflow.OnFailFixLoop,
		},
		{
			name:      "count>=n falls short",
			predicate: "count>=3(severity >= medium)",
			payloads: `[{"severity":"low"},{"severity":"medium"},` +
				`{"severity":"blocker"}]`,
			want: RoutingPass,
		},
		{
			name:      "an absent field is a non-match, not a park",
			predicate: "any(severity >= high)",
			payloads:  `[{"other":"thing"}]`,
			want:      RoutingPass,
			why: "a missing field is a fact about the ELEMENT; an undeclared " +
				"order is a fact about the SCHEMA, and only the second parks",
		},
		{
			name:      "an explicit null is a non-match, not a park",
			predicate: "any(severity >= high)",
			payloads:  `[{"severity":null}]`,
			want:      RoutingPass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalWith(t, severityOrder, tc.predicate, tc.payloads)
			if result.Parked {
				t.Fatalf("parked with a declared order in force:\n%s", result.Reason)
			}
			if result.Routing != tc.want {
				t.Errorf("routing = %q, want %q. %s", result.Routing, tc.want, tc.why)
			}
		})
	}
}

// TestThresholdT2IsIndependentOfPayloadOrder is §9 item 5 at the threshold: the
// same members in any input order reach the same verdict.
func TestThresholdT2IsIndependentOfPayloadOrder(t *testing.T) {
	permutations := []string{
		`[{"severity":"low"},{"severity":"high"},{"severity":"info"}]`,
		`[{"severity":"high"},{"severity":"info"},{"severity":"low"}]`,
		`[{"severity":"info"},{"severity":"low"},{"severity":"high"}]`,
	}
	for _, raw := range permutations {
		result := evalWith(t, severityOrder, "any(severity >= high)", raw)
		if result.Routing != workflow.OnFailFixLoop {
			t.Errorf("%s routed %q; the verdict must not depend on input order",
				raw, result.Routing)
		}
	}
}

// TestT3NamesWhichOfItsThreeCases is §5 T3 narrowed: the park survives, and the
// reason says WHICH of the three it was, because the three need three different
// remedies — register a schema, add an `ordered_enum`, or fix a value.
func TestT3NamesWhichOfItsThreeCases(t *testing.T) {
	unorderedField := fakeOrder{
		order: map[string][]string{"other": {"a", "b"}},
		types: map[string]string{"severity": "string"},
	}

	cases := []struct {
		name     string
		order    OrderResolver
		payloads string
		want     string
	}{
		{
			name:     "(i) the step declares no payload",
			order:    nil,
			payloads: `[{"severity":"low"}]`,
			want:     "declares no `payload`",
		},
		{
			name:     "(ii) the declared field carries no ordered_enum",
			order:    unorderedField,
			payloads: `[{"severity":"low"}]`,
			want:     "no `ordered_enum` on this field",
		},
		{
			name:     "(iii) a value is outside the declared order",
			order:    severityOrder,
			payloads: `[{"severity":"urgent"}]`,
			want:     "not in the order the step's schema declares",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := evalWith(t, tc.order, "any(severity >= high)", tc.payloads)
			if !result.Parked {
				t.Fatalf("routed %q instead of parking", result.Routing)
			}
			if result.Routing != workflow.OnFailWaitingHuman {
				t.Errorf("routing = %q, want %q",
					result.Routing, workflow.OnFailWaitingHuman)
			}
			if !strings.Contains(result.Reason, tc.want) {
				t.Errorf("the reason does not name this case (%q):\n%s",
					tc.want, result.Reason)
			}
			// And no message promises a stage that has shipped.
			if strings.Contains(result.Reason, "stage 5 supplies") {
				t.Error("the reason still promises stage 5, which has landed")
			}
		})
	}

	// A LITERAL outside the declared order parks too, and does so once rather
	// than per element: it is a property of the predicate, not of the payload.
	result := evalWith(t, severityOrder, "any(severity >= urgent)",
		`[{"severity":"low"},{"severity":"high"}]`)
	if !result.Parked || !strings.Contains(result.Reason, `"urgent"`) {
		t.Errorf("a literal outside the order did not park naming it: %+v", result)
	}
}

// TestOrderedComparisonNeverGuesses is §5's named obligation: every operator,
// driven against a field that is ordered, unordered, and absent. The unordered
// and absent cases must PARK rather than produce a verdict.
//
// A comparison that returned an answer here would be core inventing an order,
// which is the one thing this stage exists not to do.
func TestOrderedComparisonNeverGuesses(t *testing.T) {
	absent := fakeOrder{order: map[string][]string{}}
	unordered := fakeOrder{
		order: map[string][]string{"unrelated": {"a", "b"}},
		types: map[string]string{"severity": "string"},
	}

	for _, op := range []string{">=", ">", "<=", "<"} {
		predicate := "any(severity " + op + " high)"

		// Ordered: a verdict, either way, and never a park.
		for _, payload := range []string{
			`[{"severity":"blocker"}]`, `[{"severity":"info"}]`,
		} {
			if got := evalWith(t, severityOrder, predicate, payload); got.Parked {
				t.Errorf("%s over %s parked despite a declared order:\n%s",
					predicate, payload, got.Reason)
			}
		}

		// Unordered and absent: a park, never a verdict.
		for name, order := range map[string]OrderResolver{
			"unordered": unordered, "absent": absent, "no schema at all": nil,
		} {
			got := evalWith(t, order, predicate, `[{"severity":"high"}]`)
			if !got.Parked {
				t.Errorf("%s with an %s field routed %q; core guessed an order",
					predicate, name, got.Routing)
			}
		}
	}
}

// TestS3ThresholdSemanticsAreUnchangedForSchemalessFields is §5's survival
// suite: the full S3 case table, re-run against the S5 evaluator with an EMPTY
// resolver, asserting identical results.
//
// The table is restated here rather than referenced because that is what makes
// it a survival suite: if the S3 tests above were edited into agreement with a
// changed evaluator, this one would still hold the original answers.
func TestS3ThresholdSemanticsAreUnchangedForSchemalessFields(t *testing.T) {
	cases := []struct {
		predicate string
		payloads  string
		want      string
		parked    bool
	}{
		// T1, verbatim from S3.
		{"any(status == unmet)", `[{"status":"met"},{"status":"unmet"}]`,
			workflow.OnFailFixLoop, false},
		{"any(status == unmet)", `[{"status":"met"}]`, RoutingPass, false},
		{"all(status == met)", `[{"status":"met"},{"status":"met"}]`,
			workflow.OnFailFixLoop, false},
		{"all(status == met)", `[{"status":"met"},{"status":"unmet"}]`,
			RoutingPass, false},
		{"any(status != met)", `[{"status":"unmet"}]`, workflow.OnFailFixLoop, false},
		{"count>=2(status == unmet)",
			`[{"status":"unmet"},{"status":"unmet"},{"status":"met"}]`,
			workflow.OnFailFixLoop, false},
		// T1's scalar normalization, unchanged with no declared type.
		{"any(count == 1)", `[{"count":1}]`, workflow.OnFailFixLoop, false},
		{"any(count == 1)", `[{"count":1.0}]`, workflow.OnFailFixLoop, false},
		{"any(flag == true)", `[{"flag":true}]`, workflow.OnFailFixLoop, false},
		// T1's null rule, which is the one that surprises.
		{"any(status == met)", `[{"status":null}]`, RoutingPass, false},
		{"any(status != met)", `[{"status":null}]`, RoutingPass, false},
		{"all(status == met)", `[{"other":"x"}]`, RoutingPass, false},
		// T3: an ordered comparison over a schema-less field parks.
		{"any(severity >= high)", `[{"severity":"low"}]`,
			workflow.OnFailWaitingHuman, true},
		{"any(count >= 5)", `[{"count":10}]`, workflow.OnFailWaitingHuman, true},
		// T4, evaluated BEFORE T3 — the clause that keeps an empty result set
		// runnable.
		{"any(severity >= high)", ``, RoutingPass, false},
		{"all(severity >= high)", ``, workflow.OnFailFixLoop, false},
		{"count>=0(severity >= high)", ``, workflow.OnFailFixLoop, false},
		{"count>=1(severity >= high)", ``, RoutingPass, false},
	}

	for _, tc := range cases {
		got := evalOne(t, tc.predicate, tc.payloads)
		if got.Routing != tc.want || got.Parked != tc.parked {
			t.Errorf("%s over %q: routing = %q parked = %v, want %q/%v — an S3 "+
				"semantic changed for a schema-less field",
				tc.predicate, tc.payloads, got.Routing, got.Parked,
				tc.want, tc.parked)
		}
	}

	// The reason's S3 wording survives MINUS its removed tail, asserted by
	// prefix exactly as §5 specifies.
	parked := evalOne(t, "any(severity >= high)", `[{"severity":"low"}]`)
	const prefix = `threshold "fix-loop" on step step@0 requires an ordered ` +
		`comparison (severity >= high); ordered comparisons need a registered ` +
		`ordered_enum schema (engine-spec §11.2)`
	if !strings.HasPrefix(parked.Reason, prefix) {
		t.Errorf("the S3 reason wording did not survive:\n got %q\nwant prefix %q",
			parked.Reason, prefix)
	}
}

// TestT1IsSchemaTypedForDeclaredFields is T1's upgraded half: for a DECLARED
// field the comparison follows the declared type.
//
// It changes the verdict only where the two readings genuinely differ — a
// numeric field where `1` and `1.0` are one value an author means — and leaves
// every string and enum case exactly as string identity already had it.
func TestT1IsSchemaTypedForDeclaredFields(t *testing.T) {
	typed := fakeOrder{
		order: map[string][]string{},
		types: map[string]string{
			"count": "integer", "score": "number", "flag": "boolean",
			"status": "string",
		},
	}

	cases := []struct {
		name      string
		predicate string
		payloads  string
		want      string
	}{
		{"an integer literal matches a JSON number", "any(count == 2)",
			`[{"count":2}]`, workflow.OnFailFixLoop},
		{"a number compares numerically, not textually", "any(score == 1.50)",
			`[{"score":1.5}]`, workflow.OnFailFixLoop},
		{"a boolean compares as a boolean", "any(flag == True)",
			`[{"flag":true}]`, workflow.OnFailFixLoop},
		{"a declared string still compares by identity", "any(status == met)",
			`[{"status":"met"}]`, workflow.OnFailFixLoop},
		{"a mismatch is still a mismatch", "any(count == 3)",
			`[{"count":2}]`, RoutingPass},
		{"the null rule is untouched by a declared type", "any(count == 2)",
			`[{"count":null}]`, RoutingPass},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalWith(t, typed, tc.predicate, tc.payloads)
			if got.Parked {
				t.Fatalf("equality parked; it never needs an order:\n%s", got.Reason)
			}
			if got.Routing != tc.want {
				t.Errorf("routing = %q, want %q", got.Routing, tc.want)
			}
		})
	}

	// And the schema-less reading of the same case is the S3 one: `1.50` and
	// `1.5` are different STRINGS, which is what T1 said before a type existed.
	if got := evalOne(t, "any(score == 1.50)", `[{"score":1.5}]`); got.Routing != RoutingPass {
		t.Errorf("the schema-less reading changed: routed %q, want %q",
			got.Routing, RoutingPass)
	}
}
