package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The `aggregate` table — docs/tdd/payloads-thresholds.md §9.2.
//
// Every case runs against `severityOrder`, the five-value order declared by
// threshold_test.go's fake resolver. That order is INSTANCE DATA: core computes
// a median over it because a document said `info` precedes `low`, and the same
// computation would run over ripeness grades or T-shirt sizes unchanged.

// aggregateOver runs one aggregation over a JSON payload literal.
func aggregateOver(
	t *testing.T, raw string, params AggregateParams, order OrderResolver,
) (*AggregateOutcome, error) {
	t.Helper()
	return Aggregate(payloadSet(t, raw), params, order)
}

// medianParams is the fixture's own shape, with `hold_spread` off by default so
// the reduction cases are not entangled with the hold.
func medianParams(method string, hold int) AggregateParams {
	return AggregateParams{
		Field: "severity", Method: method, HoldSpread: hold, Output: "findings",
	}
}

// reducedValue reads one output element's reduced value.
func reducedValue(t *testing.T, out *AggregateOutcome, i int) string {
	t.Helper()
	v, ok := out.Payload[i]["severity"].(string)
	if !ok {
		t.Fatalf("output element %d has no `severity`: %#v", i, out.Payload[i])
	}
	return v
}

// TestAggregateMedian is §7.3's median, including the even-count rule an
// implementer would most likely get wrong.
//
// `m[(len-1)/2]` is the LOWER of the two central values, and the argument is not
// conservatism: core does not know which end of a declared order is worse, so a
// rule justified by "take the more severe of the two" would bake an opinion
// about severities into a statistics function — wrong and invisible for a
// ripeness or a confidence enum.
func TestAggregateMedian(t *testing.T) {
	cases := []struct {
		name    string
		members string
		want    string
		why     string
	}{
		{"one member is itself", `["medium"]`, "medium", ""},
		{"odd count takes the exact middle",
			`["low","medium","high"]`, "medium", ""},
		{"odd count, five members",
			`["info","low","medium","high","blocker"]`, "medium", ""},
		{"EVEN count takes the LOWER middle",
			`["low","blocker"]`, "low",
			"the two-member case: positions 1 and 4, (2-1)/2 = 0, so `low`"},
		{"even count, four members",
			`["info","low","high","blocker"]`, "low",
			"positions 0,1,3,4; (4-1)/2 = 1"},
		{"duplicates count as members",
			`["high","high","low"]`, "high", "sorted: low, high, high; index 1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := aggregateOver(t,
				`[{"severity":`+tc.members+`}]`, medianParams(MethodMedian, 0), severityOrder)
			testsupport.Must(t, err, "Aggregate: %v", err)
			if got := reducedValue(t, out, 0); got != tc.want {
				t.Errorf("median of %s = %q, want %q. %s", tc.members, got, tc.want, tc.why)
			}
		})
	}
}

// TestAggregateMaxAndMin covers the other two reductions, including D3's "max
// never demotes".
func TestAggregateMaxAndMin(t *testing.T) {
	const members = `[{"severity":["low","medium","high"]}]`

	max, err := aggregateOver(t, members, medianParams(MethodMax, 0), severityOrder)
	testsupport.Must(t, err, "max: %v", err)
	if got := reducedValue(t, max, 0); got != "high" {
		t.Errorf("max = %q, want high", got)
	}
	if _, present := max.Payload[0][KeyDemotedFrom]; present {
		t.Error("`max` recorded a demotion; it takes the top position and so " +
			"never demotes (D3)")
	}

	min, err := aggregateOver(t, members, medianParams(MethodMin, 0), severityOrder)
	testsupport.Must(t, err, "min: %v", err)
	if got := reducedValue(t, min, 0); got != "low" {
		t.Errorf("min = %q, want low", got)
	}
	if min.Payload[0][KeyDemotedFrom] != "high" {
		t.Errorf("min demoted_from = %v, want high — `min` demotes whenever the "+
			"members differ (D3)", min.Payload[0][KeyDemotedFrom])
	}
}

// TestAggregateIsOrderIndependent is §7.3 clause 4 and §9 item 5: the same
// members in any input order give the same result.
func TestAggregateIsOrderIndependent(t *testing.T) {
	permutations := []string{
		`[{"severity":["info","low","high","blocker"]}]`,
		`[{"severity":["blocker","high","low","info"]}]`,
		`[{"severity":["high","info","blocker","low"]}]`,
	}

	var first string
	for i, raw := range permutations {
		out, err := aggregateOver(t, raw, medianParams(MethodMedian, 0), severityOrder)
		testsupport.Must(t, err, "Aggregate(%s): %v", raw, err)
		got := reducedValue(t, out, 0)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("%s reduced to %q, but the first permutation gave %q; the "+
				"result must not depend on input order", raw, got, first)
		}
	}
}

// TestAggregateScalarFieldIsTheIdentity is G2, the property that makes the
// machinery safe to adopt.
//
// Over a flat, unclustered payload — every element with a scalar field —
// `aggregate` is the IDENTITY. Every value passes through, `held` is false
// everywhere, nothing is demoted, and the threshold sees exactly what it would
// have seen without the action. An operator can therefore introduce clustering
// later without a behavior cliff.
func TestAggregateScalarFieldIsTheIdentity(t *testing.T) {
	const raw = `[{"severity":"low"},{"severity":"blocker"},{"severity":"medium"}]`

	// `hold_spread` deliberately ON: a scalar cluster has spread 0, so even an
	// opted-in step holds nothing.
	out, err := aggregateOver(t, raw, medianParams(MethodMedian, 1), severityOrder)
	testsupport.Must(t, err, "Aggregate: %v", err)
	if len(out.Payload) != 3 {
		t.Fatalf("produced %d elements, want one per input element", len(out.Payload))
	}
	if len(out.Held) != 0 {
		t.Errorf("held %v; a one-member cluster has spread 0 and cannot trip", out.Held)
	}

	for i, want := range []string{"low", "blocker", "medium"} {
		if got := reducedValue(t, out, i); got != want {
			t.Errorf("element %d reduced to %q, want the input value %q", i, got, want)
		}
		if _, present := out.Payload[i][KeyDemotedFrom]; present {
			t.Errorf("element %d recorded a demotion over a single member", i)
		}
		members, _ := out.Payload[i][KeyMembers].([]string)
		if len(members) != 1 || members[0] != want {
			t.Errorf("element %d members = %v, want the one value", i, members)
		}
	}
}

// TestAggregateSpreadIsDistanceInTheDeclaredOrder is §7.4: spread is the
// distance between the extreme members' POSITIONS, never a count of distinct
// values and never a value difference.
func TestAggregateSpreadIsDistanceInTheDeclaredOrder(t *testing.T) {
	// Both clusters span positions 1..3, so both have spread 2 — one with two
	// members, one with three. A count-of-distinct-values implementation would
	// give 2 and 3 and hold the wrong one.
	const raw = `[{"severity":["low","high"]},{"severity":["low","medium","high"]}]`

	out, err := aggregateOver(t, raw, medianParams(MethodMedian, 2), severityOrder)
	testsupport.Must(t, err, "Aggregate: %v", err)
	if len(out.Held) != 2 {
		t.Fatalf("held %v, want both clusters — {low,high} and {low,medium,high} "+
			"are both spread 2", out.Held)
	}
	for i := range out.Payload {
		if out.Payload[i][KeyHeld] != true {
			t.Errorf("element %d is not marked held", i)
		}
	}
}

// TestAggregateHoldBoundary is §7.4's `>=` boundary and its off switch.
func TestAggregateHoldBoundary(t *testing.T) {
	// positions 1 and 3: spread 2.
	const raw = `[{"severity":["low","high"]}]`

	cases := []struct {
		hold int
		want bool
		why  string
	}{
		{0, false, "hold_spread = 0 (the default and the absent case) NEVER holds"},
		{1, true, "1 < spread"},
		{2, true, "the >= boundary: hold_spread == spread holds"},
		{3, false, "hold_spread > spread does not hold"},
	}

	for _, tc := range cases {
		out, err := aggregateOver(t, raw, medianParams(MethodMedian, tc.hold), severityOrder)
		testsupport.Must(t, err, "hold_spread = %d: %v", tc.hold, err)
		got := len(out.Held) > 0
		if got != tc.want {
			t.Errorf("hold_spread = %d held = %v, want %v — %s",
				tc.hold, got, tc.want, tc.why)
		}
		if out.Payload[0][KeyHeld] != tc.want {
			t.Errorf("hold_spread = %d: the payload's `held` disagrees with the "+
				"outcome's Held list", tc.hold)
		}
	}
}

// TestAggregateDemotionTrail is §7.5 D1/D2, with the absence asserted as KEY
// ABSENCE rather than as an empty string.
//
// The distinction matters on the wire: `demoted_from: ""` claims a demotion to
// nothing, where an omitted key says there was none. `aggregate@1` makes the
// key optional precisely so the second is expressible.
func TestAggregateDemotionTrail(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		members string
		want    any // nil ⇒ the key must be ABSENT
	}{
		{"median below the max demotes", MethodMedian,
			`["low","medium","high"]`, "high"},
		{"median at the max does not", MethodMedian, `["high","high"]`, nil},
		{"a single member never demotes", MethodMedian, `["medium"]`, nil},
		{"min demotes to the bottom", MethodMin, `["low","blocker"]`, "blocker"},
		{"min over identical members does not", MethodMin, `["low","low"]`, nil},
		{"max never demotes", MethodMax, `["info","blocker"]`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := aggregateOver(t, `[{"severity":`+tc.members+`}]`,
				medianParams(tc.method, 0), severityOrder)
			testsupport.Must(t, err, "Aggregate: %v", err)
			got, present := out.Payload[0][KeyDemotedFrom]
			if tc.want == nil {
				if present {
					t.Errorf("demoted_from = %v, want the key ABSENT", got)
				}
				return
			}
			if !present {
				t.Fatalf("demoted_from is absent, want %v", tc.want)
			}
			if got != tc.want {
				t.Errorf("demoted_from = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAggregateCarriesEveryOtherKeyThrough is G3: every other key of the input
// element survives verbatim.
//
// Core does not read them — that is the genericity rule — and dropping them
// would strip an instance's own identifiers from the very payload a downstream
// step consumes.
func TestAggregateCarriesEveryOtherKeyThrough(t *testing.T) {
	const raw = `[{"severity":["low","high"],"id":"C-1","note":"two reviewers ` +
		`disagreed","tags":["a","b"],"count":3}]`

	out, err := aggregateOver(t, raw, medianParams(MethodMedian, 0), severityOrder)
	testsupport.Must(t, err, "Aggregate: %v", err)
	element := out.Payload[0]
	if element["id"] != "C-1" || element["note"] != "two reviewers disagreed" {
		t.Errorf("scalar keys were not carried through: %#v", element)
	}
	if element["count"] != float64(3) {
		t.Errorf("a numeric key was not carried through verbatim: %#v", element["count"])
	}
	tags, ok := element["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("a composite key was not carried through: %#v", element["tags"])
	}
}

// TestAggregateFailures is §7.2's G4/G5 and B3: each is a step failure with a
// reason, never a silent guess and never an engine error that wedges the run.
func TestAggregateFailures(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "G4: a member outside the declared order",
			raw:  `[{"severity":["low","urgent"]}]`,
			want: []string{"urgent", "severity", "no position", "either end"},
		},
		{
			name: "G5: an empty members array",
			raw:  `[{"severity":[]}]`,
			want: []string{"payload[0]", "empty", "median"},
		},
		{
			name: "a composite member",
			raw:  `[{"severity":[{"nested":true}]}]`,
			want: []string{"payload[0]", "scalar"},
		},
		{
			name: "an object where the field should be",
			raw:  `[{"severity":{"value":"low"}}]`,
			want: []string{"payload[0]", "scalar"},
		},
		{
			name: "the field is absent",
			raw:  `[{"other":"thing"}]`,
			want: []string{"payload[0]", "severity"},
		},
		{
			name: "the field is null",
			raw:  `[{"severity":null}]`,
			want: []string{"payload[0]", "severity"},
		},
		{
			name: "the second element fails, naming its index",
			raw:  `[{"severity":"low"},{"severity":"urgent"}]`,
			want: []string{"payload[1]", "urgent"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := aggregateOver(t, tc.raw, medianParams(MethodMedian, 0), severityOrder)
			if err == nil {
				t.Fatal("no error; core guessed rather than refusing")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the reason does not name %q:\n%v", want, err)
				}
			}
		})
	}
}

// TestAggregateRefusesAnUnorderedField is the runtime half of V29: an aggregate
// over a field with no declared order refuses rather than reducing.
//
// V29 makes this unreachable through `workflow register`. It is reachable from
// a database restored from elsewhere, and it is a refusal because median, max,
// and min are defined ONLY over an order.
func TestAggregateRefusesAnUnorderedField(t *testing.T) {
	for name, order := range map[string]OrderResolver{
		"no schema at all": nil,
		"an unordered field": fakeOrder{
			order: map[string][]string{"other": {"a", "b"}},
		},
	} {
		_, err := aggregateOver(t, `[{"severity":"low"}]`,
			medianParams(MethodMedian, 0), order)
		if err == nil {
			t.Errorf("%s: aggregated without a declared order", name)
			continue
		}
		if !strings.Contains(err.Error(), "ordered_enum") {
			t.Errorf("%s: the reason does not name the missing declaration:\n%v",
				name, err)
		}
	}
}

// TestAggregateOutputSatisfiesTheShippedSchema is §7.6 E1/E5's run-time half:
// what the builtin produces validates against `aggregate@1`.
//
// The conjunction with the step's declared schema is V30's register-time
// question; this is the other half — that the document the engine actually
// writes is the document the shipped schema describes.
func TestAggregateOutputSatisfiesTheShippedSchema(t *testing.T) {
	const raw = `[{"severity":["low","medium","high"],"id":"C-1"},` +
		`{"severity":"info"}]`

	out, err := aggregateOver(t, raw, medianParams(MethodMedian, 2), severityOrder)
	testsupport.Must(t, err, "Aggregate: %v", err)
	encoded, err := json.Marshal(out.Payload)
	testsupport.Must(t, err, "encoding the output: %v", err)

	builtin, err := aggregateSchema()
	testsupport.Must(t, err, "compiling the embedded document: %v", err)
	err = builtin.ValidatePayload(encoded)
	testsupport.Must(t, err, "the aggregate's own output does not satisfy its shipped "+
		"schema:\n%v\n%s", err, encoded)

	// And the shape is the one §7.6 documents, key by key.
	var elements []map[string]any
	err = json.Unmarshal(encoded, &elements)
	testsupport.Must(t, err, "decoding: %v", err)
	for i, element := range elements {
		for _, key := range []string{"severity", KeyMembers, KeyHeld, KeyOperatorResolved} {
			if _, ok := element[key]; !ok {
				t.Errorf("element %d has no %q: %#v", i, key, element)
			}
		}
		if element[KeyOperatorResolved] != false {
			t.Errorf("element %d starts operator_resolved = %v; nobody has "+
				"resolved anything yet", i, element[KeyOperatorResolved])
		}
	}
	if elements[0][KeyHeld] != true || elements[1][KeyHeld] != false {
		t.Errorf("held flags = %v/%v, want the spread-2 cluster held and the "+
			"scalar one not", elements[0][KeyHeld], elements[1][KeyHeld])
	}
}

// TestParseAggregateParams is §7.1's table at run time — the same rules V28
// applies at register, because a definition can reach the engine through a
// database restored from elsewhere.
func TestParseAggregateParams(t *testing.T) {
	good := map[string]any{
		"field": "severity", "method": "median",
		"hold_spread": int64(2), "output": "findings",
	}
	params, err := ParseAggregateParams(good)
	testsupport.Must(t, err, "ParseAggregateParams: %v", err)
	if params.Field != "severity" || params.Method != MethodMedian ||
		params.HoldSpread != 2 || params.Output != "findings" {
		t.Errorf("params = %+v", params)
	}

	// `hold_spread` absent defaults to 0, which never holds.
	params, err = ParseAggregateParams(map[string]any{
		"field": "severity", "method": "max", "output": "findings",
	})
	testsupport.Must(t, err, "ParseAggregateParams without hold_spread: %v", err)
	if params.HoldSpread != 0 {
		t.Errorf("hold_spread defaulted to %d, want 0", params.HoldSpread)
	}

	bad := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"no field", map[string]any{"method": "median", "output": "k"}, "field"},
		{"unknown method",
			map[string]any{"field": "s", "method": "medain", "output": "k"}, "method"},
		{"no output", map[string]any{"field": "s", "method": "median"}, "output"},
		{"negative hold_spread",
			map[string]any{"field": "s", "method": "median", "output": "k",
				"hold_spread": int64(-1)}, "hold_spread"},
		{"a key aggregate does not take",
			map[string]any{"field": "s", "method": "median", "output": "k",
				"grouping": "cluster"}, "grouping"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAggregateParams(tc.params); err == nil {
				t.Fatal("accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %q:\n%v", tc.want, err)
			}
		})
	}
}

// conservativeSeverityOrder is `severityOrder` with the one thing added that
// DKT-267 is about: the order says which of its ends is the cautious one.
//
// It is a SEPARATE fixture rather than a field set on the shared one so that
// every other case in this file keeps running against an order that declares no
// direction — which is the evidence that the default is untouched.
var conservativeSeverityOrder = fakeOrder{
	order:        severityOrder.order,
	types:        severityOrder.types,
	conservative: map[string]string{"severity": "upper"},
}

// TestAggregateMedianTieBreaksTowardTheDeclaredConservativeEnd is DKT-267.
//
// The lower median is right when core is the only one who could choose, and
// wrong when the ORDER ITSELF has said which end is worse. Both clusters
// observed in the retro epoch (RUN-20 CL-4 and CL-6) were operator-corrected
// upward; this is that correction moved into the declaration, where it applies
// to severity orders and to nothing else.
func TestAggregateMedianTieBreaksTowardTheDeclaredConservativeEnd(t *testing.T) {
	cases := []struct {
		name    string
		members string
		want    string
		why     string
	}{
		{"the two-member tie resolves UPWARD",
			`["low","blocker"]`, "blocker",
			"positions 1 and 4; the declared conservative end is upper, so 4/2 = 1 -> blocker"},
		{"four members take the upper middle",
			`["info","low","high","blocker"]`, "high",
			"positions 0,1,3,4; 4/2 = 2 -> high"},
		{"an ODD count is untouched — it has no tie to break",
			`["low","medium","high"]`, "medium",
			"both expressions name the same element when the count is odd"},
		{"one member is still itself",
			`["medium"]`, "medium", ""},
		{"adjacent values tie upward too",
			`["low","medium"]`, "medium", "positions 1,2; 2/2 = 1 -> medium"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := aggregateOver(t, `[{"severity":`+tc.members+`}]`,
				medianParams(MethodMedian, 0), conservativeSeverityOrder)
			testsupport.Must(t, err, "Aggregate: %v", err)
			if got := reducedValue(t, out, 0); got != tc.want {
				t.Errorf("median of %s under conservative_end=upper = %q, want %q. %s",
					tc.members, got, tc.want, tc.why)
			}
		})
	}
}

// TestAggregateConservativeEndLeavesTheOtherReductionsAlone is the containment
// half: a declared direction moves the MEDIAN TIE and nothing else.
//
// `min` and `max` already name an end explicitly, so a direction that also
// nudged them would be overruling the author with the author's own annotation.
// The demotion trail must keep describing what actually happened — under an
// upward tie-break the taken value can now BE the top, which means no demotion
// is recorded where the lower median would have recorded one.
func TestAggregateConservativeEndLeavesTheOtherReductionsAlone(t *testing.T) {
	const members = `[{"severity":["low","blocker"]}]`

	max, err := aggregateOver(t, members, medianParams(MethodMax, 0), conservativeSeverityOrder)
	testsupport.Must(t, err, "max: %v", err)
	if got := reducedValue(t, max, 0); got != "blocker" {
		t.Errorf("max = %q, want blocker; a declared direction must not move a "+
			"reduction that names its end explicitly", got)
	}

	min, err := aggregateOver(t, members, medianParams(MethodMin, 0), conservativeSeverityOrder)
	testsupport.Must(t, err, "min: %v", err)
	if got := reducedValue(t, min, 0); got != "low" {
		t.Errorf("min = %q, want low; a declared direction must not move a "+
			"reduction that names its end explicitly", got)
	}

	median, err := aggregateOver(t, members, medianParams(MethodMedian, 0), conservativeSeverityOrder)
	testsupport.Must(t, err, "median: %v", err)
	if _, present := median.Payload[0][KeyDemotedFrom]; present {
		t.Errorf("median recorded demoted_from = %v; the upward tie-break TOOK "+
			"the top position, so there is no value that was not taken (D1/D2)",
			median.Payload[0][KeyDemotedFrom])
	}
}

// TestAggregateConservativeLowerIsTheDefaultSaidOutLoud pins the third state.
//
// "" (nothing declared) and "lower" (declared) must compute identically. If
// they ever diverge, one of them has become a third behavior nobody asked for.
func TestAggregateConservativeLowerIsTheDefaultSaidOutLoud(t *testing.T) {
	declaredLower := fakeOrder{
		order:        severityOrder.order,
		types:        severityOrder.types,
		conservative: map[string]string{"severity": "lower"},
	}
	const members = `[{"severity":["low","blocker"]}]`

	silent, err := aggregateOver(t, members, medianParams(MethodMedian, 0), severityOrder)
	testsupport.Must(t, err, "undeclared: %v", err)
	declared, err := aggregateOver(t, members, medianParams(MethodMedian, 0), declaredLower)
	testsupport.Must(t, err, "declared lower: %v", err)

	if got, want := reducedValue(t, declared, 0), reducedValue(t, silent, 0); got != want {
		t.Errorf("conservative_end=lower computed %q but declaring nothing "+
			"computed %q; `lower` is the default said out loud, not a third rule",
			got, want)
	}
	if got := reducedValue(t, silent, 0); got != "low" {
		t.Errorf("the undeclared default computed %q, want low — the lower "+
			"median is unchanged for every order that declares no direction", got)
	}
}

// TestAggregateConservativeEndIsPerFIELD guards the lookup key.
//
// The direction is asked for on the field being aggregated, not on the schema
// as a whole: one payload can carry a severity (directional) beside a
// confidence (not), and reading the wrong field's declaration would silently
// apply one field's opinion to the other.
func TestAggregateConservativeEndIsPerField(t *testing.T) {
	mixed := fakeOrder{
		order: map[string][]string{
			"severity":   {"info", "low", "medium", "high", "blocker"},
			"confidence": {"guess", "likely", "certain"},
		},
		types:        map[string]string{"severity": "string", "confidence": "string"},
		conservative: map[string]string{"severity": "upper"},
	}

	out, err := aggregateOver(t, `[{"confidence":["guess","certain"]}]`,
		AggregateParams{Field: "confidence", Method: MethodMedian, Output: "findings"}, mixed)
	testsupport.Must(t, err, "Aggregate: %v", err)
	got, ok := out.Payload[0]["confidence"].(string)
	if !ok {
		t.Fatalf("output has no `confidence`: %#v", out.Payload[0])
	}
	if got != "guess" {
		t.Errorf("confidence median = %q, want guess; `confidence` declares no "+
			"direction and must not inherit `severity`'s", got)
	}
}
