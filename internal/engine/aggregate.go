package engine

import (
	"fmt"
	"sort"
	"sync"

	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The `aggregate` builtin — docs/tdd/payloads-thresholds.md §7.2–§7.6.
//
// engine-spec §2, verbatim, is the specification:
//
//	One is builtin and generic: `action = "aggregate"` with
//	`params = { field, method = median|max|min, hold_spread, output, route_at }`
//	computes over any ordered-enum payload field — median, spread-hold, and a
//	recorded demotion trail work for severities, priorities, or tiers alike.
//
// EVERY VALUE HERE IS COMPARED BY ITS POSITION IN THE USER'S DECLARED ORDER AND
// BY NOTHING ELSE. `field` is a key to look up; the values under it are opaque
// tokens whose only property core knows is where the author put them. There is
// no table of known fields, no default order, and no fallback: a value with no
// position is a step failure naming it (G4), never a guess.
//
// This file is PURE. It takes a payload set, a resolver, and params; it returns
// a payload set or an error. No database, no clock, no process table — which is
// what makes §9 item 5's determinism a property of the function rather than of
// the environment it ran in.

// Aggregate params, as §7.1's table names them. Core reads exactly these five
// keys of the opaque bag and V28 refuses any other, so "the engine ignored my
// param" is a register-time sentence rather than a run-time mystery.
const (
	ParamField      = "field"
	ParamMethod     = "method"
	ParamHoldSpread = "hold_spread"
	ParamOutput     = "output"
	// ParamRouteAt is the routing floor (DKT-593): a value of the field's
	// declared order. Clusters whose reduced value sits at or above its
	// position are emitted to the step's output payload — the wire the
	// threshold and the loop read — and the rest go to the record. Like every
	// other value here it is an OPAQUE TOKEN compared only by position: core
	// does not know it is a severity, only where the author put it.
	ParamRouteAt = "route_at"
)

// The three reductions of §7.3. There are exactly three because §2 names
// exactly three; each addition would be core acquiring an opinion about what is
// worth computing (§13).
const (
	MethodMedian = "median"
	MethodMax    = "max"
	MethodMin    = "min"
)

// AggregateMethods is the closed vocabulary, in §2's own order, for V28's error
// message and for SKILL.md's table.
var AggregateMethods = []string{MethodMedian, MethodMax, MethodMin}

// Output keys of the aggregate's own payload (§7.6). They are STATISTICS AND
// PACKAGING vocabulary: `members` is a cluster's inputs, `held` is whether the
// spread reached the bound, `demoted_from` is a position in the declared order
// that was not the result, and `operator_resolved` is whether a person accepted
// it. None names a domain.
const (
	KeyMembers          = "members"
	KeyHeld             = "held"
	KeyDemotedFrom      = "demoted_from"
	KeyOperatorResolved = "operator_resolved"
	// KeyOperatorNote carries the operator's decision text into the resolved
	// payload (DKT-84). Before it, `operator_resolved` was the whole message:
	// a fixer downstream was told a decision existed and given nothing to act
	// on — the flag transited, the content did not.
	KeyOperatorNote = "operator_note"
	// KeyOperatorSetFrom records the computed value an operator's correction
	// replaced (DKT-42), mirroring `demoted_from`'s trail discipline: the
	// value that was not taken stays readable beside the one that was.
	KeyOperatorSetFrom = "operator_set_from"
)

// aggregateSchema compiles the shipped `aggregate@1` document, once.
//
// It is compiled from the EMBEDDED bytes rather than read from the registry.
// The registry's row is what a run PINS and what `schema show` prints (§7.6 E3),
// and U7 deliberately leaves a divergent pre-existing row alone so a database
// never becomes unopenable because a binary changed — so the registry is not a
// safe source for the document this binary validates against. The two agreeing
// is TestEmbeddedAggregateSchemaMatchesItsGolden's business, at build time.
var aggregateSchema = sync.OnceValues(schema.Aggregate)

// AggregateParams is the validated form of §7.1's table.
type AggregateParams struct {
	Field      string
	Method     string
	HoldSpread int
	Output     string
	// RouteAt is the routing floor, or "" when the step declares none — the
	// absent case, in which Aggregate emits every cluster exactly as it always
	// has.
	RouteAt string
}

// AggregateOutcome is one aggregation's result.
type AggregateOutcome struct {
	// Payload is the output payload, one element per emitted cluster (§7.6).
	// Without `route_at` that is one element per input element; with it, only
	// the clusters whose reduced value's position reached the floor.
	Payload []map[string]any
	// Held indexes the PAYLOAD elements whose spread tripped `hold_spread`. It
	// is a list rather than a count because §7.7's materialization has to be
	// able to say WHICH clusters are open, and a count cannot. The indices are
	// positions in Payload — the payload the artifact records and held
	// resolution re-reads (H2a) — which coincide with input positions whenever
	// `route_at` is absent.
	Held []int
	// Recorded holds the clusters `route_at` routed below the floor (DKT-593):
	// fully reduced — value, members, demotion trail — but destined for the
	// record rather than the loop output. The caller writes them into the
	// action's own audit row; they never enter the artifact payload, so the
	// threshold and every downstream `inputs` reader see only the emitted set.
	// Nil whenever `route_at` is absent, which is what keeps the absent case
	// byte-identical to the pre-`route_at` builtin.
	Recorded []map[string]any
}

// ParseAggregateParams reads §7.1's five keys out of the opaque bag.
//
// It is the ONE place core reads inside `params`, and it reads exactly the keys
// §2 names. The same rules run at register time (V28) so a typo'd
// `method = "medain"` is refused before a run spends the inputs it would have
// aggregated; this exists as well because a definition can reach the engine
// through a database restored from elsewhere.
func ParseAggregateParams(params map[string]any) (AggregateParams, error) {
	var out AggregateParams

	field, err := stringParam(params, ParamField)
	if err != nil {
		return out, err
	}
	if field == "" {
		return out, fmt.Errorf("`params.%s` is required: it names the payload "+
			"property to aggregate over", ParamField)
	}

	method, err := stringParam(params, ParamMethod)
	if err != nil {
		return out, err
	}
	if !contains(AggregateMethods, method) {
		return out, fmt.Errorf("`params.%s` must be one of %v, got %q",
			ParamMethod, AggregateMethods, method)
	}

	output, err := stringParam(params, ParamOutput)
	if err != nil {
		return out, err
	}
	if output == "" {
		return out, fmt.Errorf("`params.%s` is required: it is the kind this "+
			"step produces", ParamOutput)
	}

	// `hold_spread` tolerates the same int/int64/float64/json.Number shapes
	// V28's register-time check does (workflow.AsInteger — one coercion switch
	// for both the register-time and run-time reads of this param, rather than
	// two that can drift).
	hold := 0
	if raw, present := params[ParamHoldSpread]; present {
		var ok bool
		if hold, ok = workflow.AsInteger(raw); !ok {
			return out, fmt.Errorf("`params.%s` must be an integer, got %v", ParamHoldSpread, raw)
		}
	}
	if hold < 0 {
		return out, fmt.Errorf("`params.%s` must be an integer >= 0, got %d",
			ParamHoldSpread, hold)
	}

	// `route_at` is optional; present, it must be a non-empty string. WHETHER
	// the value has a position is the declared order's question and is asked in
	// Aggregate, where the resolver is — the same split V28/V28a make at
	// register time.
	routeAt, err := stringParam(params, ParamRouteAt)
	if err != nil {
		return out, err
	}
	if _, present := params[ParamRouteAt]; present && routeAt == "" {
		return out, fmt.Errorf("`params.%s` must be a non-empty string naming "+
			"a value of `params.%s`'s declared order", ParamRouteAt, ParamField)
	}

	// V28's "no other keys". An unread key is a declaration the author believes
	// is doing something; saying so at register time is the whole discipline.
	for key := range params {
		switch key {
		case ParamField, ParamMethod, ParamHoldSpread, ParamOutput, ParamRouteAt:
			continue
		}
		return out, fmt.Errorf("`params.%s` is not a parameter of `aggregate`; "+
			"it takes exactly %v", key,
			[]string{ParamField, ParamMethod, ParamHoldSpread, ParamOutput, ParamRouteAt})
	}

	out = AggregateParams{
		Field: field, Method: method, HoldSpread: hold, Output: output,
		RouteAt: routeAt,
	}
	return out, nil
}

// Aggregate reduces each input element's cluster to a single value of the same
// field (§7.2–§7.5).
//
// EACH ELEMENT OF THE INPUT PAYLOAD IS ONE CLUSTER. `params` carries no grouping
// key because clustering is the JUDGED half and reconciliation is the COMPUTED
// half (engine-core §6) — so the grouping arrives in the payload, as an array
// under the field, and G2 makes a scalar there a one-member cluster.
//
// G2 is the property that makes the machinery safe to adopt: over a flat,
// unclustered payload `aggregate` is the IDENTITY. Every value passes through,
// nothing is held, nothing is demoted, and the threshold sees exactly what it
// would have seen without the action — so an operator can introduce clustering
// later without a behavior cliff.
func Aggregate(
	payloads []map[string]any, params AggregateParams, order OrderResolver,
) (*AggregateOutcome, error) {
	if order == nil || !order.Ordered(params.Field) {
		// V29 makes this unreachable through `workflow register`. It is
		// reachable from a restored database, and it is a refusal rather than a
		// guess: median, max, and min are defined ONLY over an order.
		return nil, fmt.Errorf(
			"field %q declares no `ordered_enum` in this step's pinned payload "+
				"schema; median, max, and min are defined only over a declared order",
			params.Field)
	}

	// `route_at` resolves to a POSITION, once, before any cluster is read. A
	// value the declared order does not name is a step failure naming it —
	// V28a makes this unreachable through `workflow register`, and it is
	// reachable from a restored database, where it follows G4's discipline: no
	// position, no guess. -1 means no floor; every cluster is emitted.
	routeFloor := -1
	if params.RouteAt != "" {
		pos, ok := order.Position(params.Field, params.RouteAt)
		if !ok {
			return nil, fmt.Errorf(
				"`params.%s`: the value %q is not in the order this step's "+
					"pinned schema declares for %q, so it has no position; core "+
					"does not guess a routing floor for an unknown value",
				ParamRouteAt, params.RouteAt, params.Field)
		}
		routeFloor = pos
	}

	out := &AggregateOutcome{Payload: make([]map[string]any, 0, len(payloads))}

	for i, element := range payloads {
		members, err := clusterMembers(element, params.Field, i)
		if err != nil {
			return nil, err
		}

		positions, err := positionsOf(members, params.Field, i, order)
		if err != nil {
			return nil, err
		}

		taken := positions[reductionIndex(
			params.Method, len(positions), order.Conservative(params.Field))]
		top := positions[len(positions)-1]
		reduced := members[taken.at]

		// §7.4: spread is DISTANCE IN THE DECLARED ORDER, never a count of
		// distinct values and never a value difference — so {low, high} and
		// {low, medium, high} are both spread 2. The comparison is `>=`, and
		// `hold_spread = 0` (the default, and the absent case) NEVER holds, so a
		// step that does not opt in behaves exactly as it did before this clause
		// existed.
		spread := top.rank - positions[0].rank
		held := params.HoldSpread > 0 && spread >= params.HoldSpread

		// G3: every OTHER key of the element is carried through verbatim. Core
		// does not read them, and dropping them would strip an instance's own
		// identifiers from the very payload a downstream step consumes.
		result := make(map[string]any, len(element)+4)
		for key, value := range element {
			if key == params.Field {
				continue
			}
			result[key] = value
		}
		result[params.Field] = reduced
		result[KeyMembers] = members
		result[KeyHeld] = held
		result[KeyOperatorResolved] = false

		// D1/D2: demoted when the computed value's position is STRICTLY BELOW
		// its maximum member's, recording the value that was not taken. Absent —
		// omitted from the object, never an empty string — when nothing was.
		// D3 falls out of this rather than being written separately: `max` takes
		// the top position and so never demotes, `min` demotes whenever the
		// members differ, and `median` demotes on any cluster whose upper half
		// is non-trivial.
		if taken.rank < top.rank {
			result[KeyDemotedFrom] = members[top.at]
		}

		// `route_at` (DKT-593): a cluster whose reduced value's position is
		// below the floor goes to the RECORD, not the loop output. The routing
		// is CORE's, over its own computed positions — the same comparison the
		// threshold would make — which is what keeps a downstream fixer's "the
		// reconciled set is the work" contract intact: the set it receives has
		// already been routed, so it has nothing to filter.
		//
		// A HELD cluster is never routed below the floor. Its computed value is
		// exactly the value the hold refuses to trust — the spread says the
		// members disagree, and an operator resolving the hold may accept a
		// different value entirely — so routing it out by that value would
		// spend the decision the hold exists to ask. It is emitted, it gates
		// the step as every held cluster does, and the floor's opinion waits
		// for the operator's.
		if routeFloor >= 0 && !held && taken.rank < routeFloor {
			out.Recorded = append(out.Recorded, result)
			continue
		}

		out.Payload = append(out.Payload, result)
		if held {
			// The index is the element's position in the EMITTED payload — the
			// payload the artifact records and H2a's `<step>-held@k#i` suffix
			// addresses — which is `i` exactly whenever no cluster was routed
			// below the floor.
			out.Held = append(out.Held, len(out.Payload)-1)
		}
	}

	return out, nil
}

// reductionIndex is §7.3's table, over member positions sorted ascending.
//
//	min     m[0]
//	max     m[len-1]
//	median  m[(len-1)/2]  — integer division: the exact middle for odd counts,
//	                        the LOWER of the two central values for even ones,
//	                        unless the ORDER ITSELF declares its upper end the
//	                        conservative one, in which case m[len/2]
//
// THE DEFAULT EVEN-COUNT TIE RULE IS STILL THE LOWER MEDIAN, and the argument
// for it is unchanged and still correct. Core does not know which end of a
// declared order is worse: a rule justified by "take the more severe of the
// two" would require core to believe the top of every order is the bad end,
// which is an opinion about severities baked into a statistics function — wrong
// and invisible for a ripeness or a confidence enum. `(len-1)/2` is the
// standard lower median for ordinal data where no arithmetic mean exists, and
// it is order-independent, which §9 item 5 requires.
//
// WHAT CHANGED (DKT-267) IS WHO IS ASKED, NOT WHAT CORE BELIEVES. An order can
// know its own bad end even when core cannot, so a schema may declare
// `conservative_end` on the field and this function reads that declaration. A
// severity order that declares `"upper"` breaks its ties upward; a confidence or
// ripeness order declares nothing, `conservative` arrives empty, and the
// expression is the one that was here before. Both branches remain
// order-independent and total — the tie-break is a function of the declaration
// and the count, never of arrival sequence.
//
// The parameter is a DIRECTION, not a comparison: it can move the median by at
// most one position and it is read for no other method, since `min` and `max`
// already name an end explicitly and a declared direction cannot have an
// opinion about the end an author asked for by name.
func reductionIndex(method string, n int, conservative string) int {
	switch method {
	case MethodMin:
		return 0
	case MethodMax:
		return n - 1
	default:
		// Only an EVEN count has two central values to choose between; for an
		// odd count both expressions name the same element, so the guard is
		// documentation rather than arithmetic.
		if conservative == schema.ConservativeUpper && n%2 == 0 {
			return n / 2
		}
		return (n - 1) / 2
	}
}

// ranked pairs a member's position in the declared order with its index in the
// members slice, so a reduction can name the VALUE the author wrote rather than
// a value reconstructed from a rank.
type ranked struct {
	rank int
	at   int
}

// positionsOf ranks a cluster's members, ascending.
//
// Sorting is by position and then by arrival index, which makes the result
// TOTAL and therefore order-independent: two members at the same position are
// the same value, so which one the reduction names cannot be observed.
func positionsOf(
	members []string, field string, element int, order OrderResolver,
) ([]ranked, error) {
	out := make([]ranked, 0, len(members))
	for i, member := range members {
		rank, ok := order.Position(field, member)
		if !ok {
			// G4: a member value not present in the declared order is a step
			// failure naming the value and the field. NOT sorted to an end and
			// NOT ignored.
			return nil, fmt.Errorf(
				"payload[%d].%s: the value %q is not in the order this step's "+
					"pinned schema declares, so it has no position; core does not "+
					"sort an unknown value to either end", element, field, member)
		}
		out = append(out, ranked{rank: rank, at: i})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		return out[i].at < out[j].at
	})
	return out, nil
}

// clusterMembers reads one element's members (§7.2 G1/G2/G5).
func clusterMembers(element map[string]any, field string, index int) ([]string, error) {
	raw, present := element[field]
	if !present || raw == nil {
		return nil, fmt.Errorf(
			"payload[%d] has no %q; an `aggregate` step reduces that field and "+
				"cannot reduce a field that is not there", index, field)
	}

	switch v := raw.(type) {
	case []any:
		// G5: an empty members array is a step failure naming the element index.
		// An empty cluster has no median, and inventing one — null? the lowest? —
		// is exactly the kind of guess this stage exists to refuse.
		if len(v) == 0 {
			return nil, fmt.Errorf(
				"payload[%d].%s is an empty array; an empty cluster has no median, "+
					"and core will not invent one", index, field)
		}
		members := make([]string, 0, len(v))
		for i, item := range v {
			s, err := memberValue(item)
			if err != nil {
				return nil, fmt.Errorf("payload[%d].%s[%d]: %w", index, field, i, err)
			}
			members = append(members, s)
		}
		return members, nil
	default:
		// G2: a scalar is a ONE-MEMBER cluster. The median/max/min of one value
		// is that value, the spread is 0, nothing is held, nothing is demoted —
		// the identity property.
		s, err := memberValue(raw)
		if err != nil {
			return nil, fmt.Errorf("payload[%d].%s: %w", index, field, err)
		}
		return []string{s}, nil
	}
}

// memberValue normalizes one member to the string form a declared order names
// its values in.
//
// A composite member is a step failure rather than a rendering: `["a","b"]` as
// a member of a cluster of clusters is a shape §7.2 does not define, and
// flattening it would be core deciding what the author meant.
func memberValue(raw any) (string, error) {
	switch raw.(type) {
	case map[string]any, []any:
		return "", fmt.Errorf(
			"a cluster member must be a scalar value the declared order names, " +
				"not an object or an array")
	}
	value, err := normalizeScalar(raw)
	if err != nil {
		return "", err
	}
	return value, nil
}

func stringParam(params map[string]any, key string) (string, error) {
	raw, present := params[key]
	if !present {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("`params.%s` must be a string", key)
	}
	return s, nil
}

// AggregateBody renders the human half of the artifact an aggregate produces.
//
// It names counts and nothing else: how many clusters were reduced, how many
// are open, and how many the routing floor sent to the record. A body that
// summarized the VALUES would be core narrating an instance's meaning back to
// its operator.
func aggregateBody(step string, clusters, held, recorded int) string {
	body := fmt.Sprintf("aggregate on %s: %d cluster(s) reduced", step, clusters)
	if held > 0 {
		body += fmt.Sprintf(", %d held for an operator decision", held)
	}
	if recorded > 0 {
		body += fmt.Sprintf(
			", %d below the `route_at` floor, recorded and not routed", recorded)
	}
	return body
}
