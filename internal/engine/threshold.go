package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Threshold evaluation — engine-spine §6.14 as payloads-thresholds §5 upgrades
// it, clause by clause against that section's own table and no other.
//
// §11.2 gives the grammar and one hard restriction: ordered comparisons are
// defined ONLY for fields whose registered schema declares `ordered_enum`.
// Schemas register HERE, at S5, so the four clauses read:
//
//	T1  equality (== / !=) evaluates. UNCHANGED for schema-less fields — as
//	    strings after JSON scalar normalization. For a DECLARED field the
//	    comparison is schema-typed: a string enum by value identity, a
//	    number/integer numerically, a boolean as a boolean
//	T2  ordered operators EVALUATE, over Position (§4.3 I3), for fields whose
//	    pinned schema declares `ordered_enum`. `a >= b` is
//	    `Position(field, a) >= Position(field, b)`, and there is no other
//	    definition
//	T3  SURVIVES, NARROWED. An ordered comparison that cannot be decided routes
//	    `waiting-human` with a recorded reason naming WHICH of three cases it
//	    was: (i) the step declares no `payload`, (ii) the declared field carries
//	    no `ordered_enum`, (iii) a value is absent from the declared order (I4).
//	    The engine NEVER guesses an order
//	T4  aggregation over an EMPTY payload set is the ordinary mathematical
//	    convention, and is evaluated BEFORE T3. UNCHANGED, verbatim
//
// T4-before-T3 is the clause that makes an empty result set runnable, and it is
// derived rather than invented: `any(P)` asserts some element satisfies P, and
// with no elements there is no witness, so it is false WHATEVER P IS — the
// predicate is never consulted. Short-circuiting an aggregation over an empty
// set before touching its predicate is what every implementation of any/all
// does, and it is why an action step with no held clusters and an empty result
// set still routes `pass` without consulting an order.
//
// THE ORDER IS THE USER'S, AND IT ARRIVES AS A PARAMETER. The resolver is
// threaded through rather than read from a package-level variable — the same
// purity discipline §4.9.2 applies to `Validate` — which is what lets the table
// tests run without a database, and what makes "no schema" a value a caller
// passes rather than a state the package remembers.

// RoutingPass is §11.2's default: no threshold matches ⇒ pass.
const RoutingPass = "pass"

// OrderResolver reports what the step's PINNED payload schema declares about a
// field (§5, §4.3).
//
// It is deliberately narrow. Position is the only ordering API — there is no
// Less(a, b) a caller could invoke on a field with no declared order — so the
// type makes "compare two values of a field core has no order for"
// unrepresentable rather than merely discouraged. Ordered and FieldType exist
// for the two things the evaluator must SAY rather than guess: which of T3's
// three cases it hit, and whether a literal is compared as a string, a number,
// or a boolean.
//
// A nil OrderResolver is the step-declares-no-`payload` case, and it is the
// exact S3 behavior — which is what makes the survival suite a re-run of the S3
// table with nothing passed in.
type OrderResolver interface {
	// Position reports a value's index in the field's declared order, and
	// whether the field is ordered AND the value is in that order (§4.3 I4).
	Position(field, value string) (int, bool)
	// Ordered reports whether the field declares an order at all.
	Ordered(field string) bool
	// FieldType is the field's declared JSON `type`, or "" when the field
	// declares none or is not declared at all. Both fall back to S3's string
	// comparison, which is correct: a schema that constrains nothing has said
	// the value may be anything.
	FieldType(field string) string
	// Conservative is the field's declared `conservative_end` — which END of
	// the declared order the author considers the cautious one — or "" when
	// none was declared.
	//
	// It reports a DIRECTION, never a rank and never a value, so it cannot
	// become the second ordering API Position exists to be the only one of. Its
	// single reader is the aggregate's even-count median tie-break (DKT-267):
	// core still does not know which end of an order is bad, it asks the order.
	Conservative(field string) string
}

// ThresholdResult is one evaluation's outcome.
type ThresholdResult struct {
	// Routing is the matched routing key, or RoutingPass when none matched.
	Routing string
	// Reason is set when T3 parked the step: it names the step, the routing
	// key, the predicate verbatim, and the cause, so an operator resolving it
	// can see exactly what could not be decided and why.
	Reason string
	// Parked reports a T3 park, distinguishing "routed to waiting-human because
	// a predicate SAID SO" from "parked because a comparison was undecidable".
	// Both end at `waiting-human`; only one is a gap S5 closes.
	Parked bool
}

// EvaluateThreshold applies a step's `threshold` table to a payload set.
//
// Routings are evaluated TOP TO BOTTOM, FIRST MATCH ROUTES (§11.2). "Top to
// bottom" over a Go map needs a defined order, and the order is the DECLARED
// one — recovered from the definition's routing sequence — because a map's
// range order varies per run and a first-match rule over a varying order is a
// routing decision that changes between two identical runs.
func EvaluateThreshold(
	instance string, threshold map[string]string, order []string,
	payloads []map[string]any, schema OrderResolver,
) (ThresholdResult, error) {
	for _, routing := range order {
		src, ok := threshold[routing]
		if !ok {
			continue
		}

		pred, err := workflow.ParsePredicate(src)
		if err != nil {
			return ThresholdResult{}, validationErr(
				"step %s: %v", instance, err)
		}

		matched, why, err := evaluate(pred, payloads, schema)
		if err != nil {
			return ThresholdResult{}, err
		}

		if why != "" {
			// T3: the comparison was actually attempted and cannot be decided.
			// Park, with the reason recorded — naming which of the three cases
			// it was, because the three need three different remedies.
			return ThresholdResult{
				Routing: workflow.OnFailWaitingHuman,
				Parked:  true,
				Reason: fmt.Sprintf(
					"threshold %q on step %s requires an ordered comparison (%s); %s",
					routing, instance, predicateBody(pred), why),
			}, nil
		}

		if matched {
			return ThresholdResult{Routing: routing}, nil
		}
	}

	// §11.2: no match ⇒ pass.
	return ThresholdResult{Routing: RoutingPass}, nil
}

// T3's three sub-cases, as the reason an operator reads.
//
// The S3 wording is kept for (i) MINUS its "which stage 5 supplies" tail, and
// the tail is removed in the commit that supplies it: a message promising a
// future stage after that stage has shipped is worse than no message. What
// replaces it is the remedy that is now actually available.
const (
	whyNoPayloadSchema = "ordered comparisons need a registered ordered_enum " +
		"schema (engine-spec §11.2), and this step declares no `payload`"
	whyFieldNotOrdered = "ordered comparisons need a registered ordered_enum " +
		"schema (engine-spec §11.2), and the step's declared schema carries no " +
		"`ordered_enum` on this field"
)

// whyValueUnordered is T3 case (iii) and §4.3 I4: a value present in the
// payload but ABSENT from the declared order.
//
// It is not sorted to an end and not treated as smallest. Payload validation at
// `complete` (§4.8) makes this unreachable for a validated payload; it is
// reachable for a payload recorded before the schema was declared, which is
// precisely when guessing would be worst.
func whyValueUnordered(field, value string) string {
	return fmt.Sprintf(
		"the value %q is not in the order the step's schema declares for %q, so "+
			"it has no position; core does not sort an unknown value to either end",
		value, field)
}

// evaluate applies one predicate to a payload set, reporting whether it matched
// and, when it could not be decided, WHY.
//
// The two results are separate because "did not match" and "could not be
// decided" are different facts with different consequences: the first advances
// to the next routing, the second parks the step. Collapsing them into a single
// bool is how an undecidable ordered comparison would silently read as "no
// match ⇒ pass" — the exact silent misroute T3 exists to prevent. The reason is
// carried rather than reconstructed by the caller, because only this function
// knows which of T3's three cases it hit.
func evaluate(
	p workflow.Predicate, payloads []map[string]any, schema OrderResolver,
) (matched bool, why string, err error) {
	// ---- T4, BEFORE T3. ----------------------------------------------------
	//
	// Over zero payloads the aggregation short-circuits without ever consulting
	// the predicate, so an ordered comparison over an empty set is decided
	// (false for `any`) rather than parked. This is what lets the fixture's
	// `reconcile` step — whose stub payload is empty at S3 (§6.13) — flow
	// through `any(severity >= high)` without the engine guessing anything
	// about severities.
	if len(payloads) == 0 {
		switch p.Agg {
		case workflow.AggAny:
			// No elements, so no witness: false, whatever P is.
			return false, "", nil
		case workflow.AggAll:
			// No elements, so no violation: true, whatever P is.
			return true, "", nil
		case workflow.AggCount:
			// count>=n over zero elements holds iff n == 0.
			return p.Count == 0, "", nil
		}
	}

	// ---- T2/T3: an ordered comparison over a non-empty set. ----------------
	//
	// The engine never guesses an order — not lexicographic, not
	// enum-declaration order, not "high > medium because someone will assume
	// so". A guessed order is a silent misroute, which is strictly worse than a
	// park an operator can see and resolve with `step resolve`. What changed at
	// this stage is only where the order comes FROM: a user's registered schema
	// rather than nowhere.
	if p.Ordered() {
		return evaluateOrdered(p, payloads, schema)
	}

	// ---- T1: equality. -----------------------------------------------------
	count := 0
	for _, payload := range payloads {
		ok, err := compareEquality(p, payload[p.Field], payload, schema)
		if err != nil {
			return false, "", err
		}
		if ok {
			count++
		}
	}
	return aggregate(p, count, len(payloads)), "", nil
}

// evaluateOrdered is T2: `a op b` is `Position(field, a) op Position(field, b)`,
// and there is NO OTHER DEFINITION.
//
// The literal is positioned first and once. It is a property of the predicate,
// not of the payload, so a literal outside the declared order is one refusal
// rather than one per element — and it is refused rather than approximated,
// which is the same rule I4 applies to a payload value.
func evaluateOrdered(
	p workflow.Predicate, payloads []map[string]any, schema OrderResolver,
) (bool, string, error) {
	// T3 (i): the step declares no `payload`, so there is no order to consult.
	if schema == nil {
		return false, whyNoPayloadSchema, nil
	}
	// T3 (ii): the field is declared but carries no `ordered_enum`. V21c makes
	// this unreachable through `workflow register`; it is reachable from a
	// database restored from elsewhere, which is the same reason
	// ParsePredicate keeps its own error.
	if !schema.Ordered(p.Field) {
		return false, whyFieldNotOrdered, nil
	}
	// T3 (iii) for the literal.
	want, ok := schema.Position(p.Field, p.Literal)
	if !ok {
		return false, whyValueUnordered(p.Field, p.Literal), nil
	}

	count := 0
	for _, payload := range payloads {
		raw, present := payload[p.Field]
		if !present || raw == nil {
			// The null rule is T1's and applies here for the same reason: there
			// is no value, so there is nothing to rank. It is a non-match, not a
			// park — a missing field is a fact about the element, where an
			// undeclared order is a fact about the schema.
			continue
		}
		value, err := normalizeScalar(raw)
		if err != nil {
			return false, "", fmt.Errorf("threshold field %q: %w", p.Field, err)
		}
		got, ok := schema.Position(p.Field, value)
		if !ok {
			// T3 (iii): I4's value-not-in-the-declared-order. NOT sorted to an
			// end and NOT treated as smallest.
			return false, whyValueUnordered(p.Field, value), nil
		}
		if comparePositions(p.Op, got, want) {
			count++
		}
	}
	return aggregate(p, count, len(payloads)), "", nil
}

// comparePositions applies an ordered operator to two POSITIONS.
//
// This is the only place two enum values are ever compared, and they are
// compared as integers that came out of Position. Core knows that `enum[i]`
// precedes `enum[i+1]` because a user's document said so; it does not know
// which end is worse, higher, or better, and nothing here needs it to.
func comparePositions(op string, got, want int) bool {
	switch op {
	case workflow.OpGE:
		return got >= want
	case workflow.OpGT:
		return got > want
	case workflow.OpLE:
		return got <= want
	case workflow.OpLT:
		return got < want
	}
	return false
}

// aggregate applies §11.2's aggregation to a match count.
//
// It is one function for the ordered and the equality paths so the two cannot
// disagree about what `all` means over a set with absent values.
func aggregate(p workflow.Predicate, count, total int) bool {
	switch p.Agg {
	case workflow.AggAny:
		return count > 0
	case workflow.AggAll:
		return count == total
	case workflow.AggCount:
		return count >= p.Count
	}
	return false
}

// compareEquality is T1: `==` and `!=` compare the payload field's value to the
// literal. No schema is required to know whether two values are the same value,
// so this path never parks and never consults an order.
//
// UNCHANGED FOR SCHEMA-LESS FIELDS: string comparison after JSON scalar
// normalization, exactly as S3 did it. For a field whose pinned schema declares
// a `type`, the comparison is schema-typed — which changes the verdict in
// exactly the cases where the two readings genuinely differ, and nowhere else:
// `count == 1.0` against `{"count": 1}` is a numeric equality an author means
// and string normalization misses.
//
// The `null` rule is the one that surprises: null is NEVER equal to anything,
// INCLUDING null. That is deliberate and is stated in T1 — a missing field and
// a field explicitly set to null are both "no value here", and treating two
// absences as equal would make `all(field == x)` true for a payload set that
// never mentioned the field.
func compareEquality(
	p workflow.Predicate, value any, payload map[string]any, schema OrderResolver,
) (bool, error) {
	_, present := payload[p.Field]

	// An absent field and an explicit null are the same fact, and neither
	// compares equal — so `!=` against a null is FALSE too, not true. "Not
	// equal" asserts two values differ, and there is no value here to differ.
	if !present || value == nil {
		return false, nil
	}

	same, err := sameValue(p, value, schema)
	if err != nil {
		return false, err
	}
	switch p.Op {
	case workflow.OpEQ:
		return same, nil
	case workflow.OpNE:
		return !same, nil
	}
	return false, nil
}

// sameValue decides identity for T1, under the field's declared type when there
// is one.
//
// A type the schema declares but whose literal does not parse falls back to the
// string comparison rather than erroring: V21b refuses such a literal at
// register time, so reaching here means a definition arrived some other way,
// and S3's answer is the conservative one.
func sameValue(p workflow.Predicate, value any, schema OrderResolver) (bool, error) {
	normalized, err := normalizeScalar(value)
	if err != nil {
		return false, fmt.Errorf("threshold field %q: %w", p.Field, err)
	}
	if schema == nil {
		return normalized == p.Literal, nil
	}

	switch schema.FieldType(p.Field) {
	case "number", "integer":
		got, gotErr := strconv.ParseFloat(normalized, 64)
		want, wantErr := strconv.ParseFloat(p.Literal, 64)
		if gotErr == nil && wantErr == nil {
			return got == want, nil
		}
	case "boolean":
		got, gotErr := strconv.ParseBool(normalized)
		want, wantErr := strconv.ParseBool(p.Literal)
		if gotErr == nil && wantErr == nil {
			return got == want, nil
		}
	}
	// "string", an enum, or no declared type: value identity, which is what
	// string comparison over normalized scalars already is.
	return normalized == p.Literal, nil
}

// normalizeScalar renders a JSON scalar canonically for string comparison
// (T1): numbers canonicalized, booleans `true`/`false`, strings verbatim.
//
// Numbers go through strconv rather than fmt so 1.0 and 1 compare equal to the
// literal `1` — encoding/json decodes every number into float64, so a payload
// written `{"count": 1}` would otherwise normalize to "1e+00" and match
// nothing an author would write.
func normalizeScalar(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case json.Number:
		return v.String(), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	// A composite value (object or array) has no scalar rendering, and
	// inventing one would make `field == [1,2]` mean something undefined.
	return "", fmt.Errorf("value %v is not a JSON scalar and cannot be compared", value)
}

// predicateBody renders a predicate's inner `field op literal` for the T3
// reason string, matching the §6.14 example's `(severity >= high)` spelling.
func predicateBody(p workflow.Predicate) string {
	return fmt.Sprintf("%s %s %s", p.Field, p.Op, p.Literal)
}

// ThresholdOrder returns a step's threshold routings in DECLARED order.
//
// TOML tables decode into Go maps, which lose declaration order, so the order
// is reconstructed deterministically: the §11.2 non-step routings first in
// their specified sequence, then step-name routings sorted. That is not the
// author's literal file order — TOML cannot give it back — but it IS a total,
// reproducible order, which is what first-match-routes actually requires. Two
// identical runs must route identically; a map range would not guarantee that.
func ThresholdOrder(threshold map[string]string) []string {
	if len(threshold) == 0 {
		return nil
	}

	var out []string
	// The closed vocabulary first, in §11.2's own sequence.
	for _, routing := range []string{
		workflow.OnFailFixLoop, workflow.OnFailWaitingHuman, RoutingPass,
	} {
		if _, ok := threshold[routing]; ok {
			out = append(out, routing)
		}
	}

	// Then interposed step-name routings, sorted for determinism.
	var steps []string
	for routing := range threshold {
		switch routing {
		case workflow.OnFailFixLoop, workflow.OnFailWaitingHuman, RoutingPass:
			continue
		}
		steps = append(steps, routing)
	}
	sort.Strings(steps)
	return append(out, steps...)
}
