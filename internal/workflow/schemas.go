package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/schema"
)

// Registered is a compiled, registered payload schema.
//
// It is an ALIAS rather than a second type: the schema package owns what a
// registered document is, and a copy here would be a second answer to the same
// question. The alias lets the rules below read as §4.9.2 wrote them.
type Registered = schema.Registered

// SchemaResolver reports what is registered.
//
// V21a-V21d and V25a are the only rules that ask a question about the
// ENVIRONMENT; they take it as a parameter, so `Validate()` stays a pure
// function of bytes and every other rule with it. V26 (gates-trust §8.2)
// established this pattern for exactly the same reason, and this follows it
// rather than inventing a second one.
//
// It is what keeps TestFixtureRegistersClean honest: that test exercises
// Validate over the committed fixture with NO database, and continues to.
type SchemaResolver interface {
	// Schema returns the registered document at name@version, or an error
	// wrapping schema.ErrNotRegistered when there is none.
	Schema(name string, version int) (*Registered, error)
}

// ErrNotRegistered is what a SchemaResolver returns for an absent name@version.
// It is declared here, beside the interface, so an implementation has one
// sentinel to return rather than a message this package has to pattern-match.
var ErrNotRegistered = errors.New("schema not registered")

// ValidateSchemas is §4.9.1's cross-validation table: the rules that compare a
// definition against the schemas it names.
//
// §11.2, verbatim: "Fields and literals are validated against the registered
// schema at `workflow register` time." That sentence cannot be satisfied when
// there is no registered schema, so V25a makes an unregistered reference a HARD
// refusal here. The alternatives all amount to not validating at register time
// while claiming to: deferring to activation produces a *registered* workflow
// that can never activate, and deferring to first evaluation surfaces a typo
// hours into a run, on a step whose work is already done.
//
// It is called AFTER Validate and ValidateVoteRules, so an author sees grammar
// errors before environment errors.
func ValidateSchemas(def *Definition, resolver SchemaResolver) error {
	if def == nil || resolver == nil {
		return nil
	}

	for _, step := range def.Steps {
		// V29's first half, checked BEFORE the V21d skip: an `aggregate` step
		// MUST declare a `payload`. Median, max, and min are defined only over
		// an order, so an aggregate without a declared one is a step that can
		// never compute — and §11.2's own restriction is the same restriction.
		//
		// This is a deviation from §11.1, which makes `payload` optional, and it
		// is recorded as an amendment rather than slipped in. The alternative
		// considered and rejected: inferring the order from the schema of the
		// step the aggregate reads its input from. That is magic — nothing in
		// the grammar says an action reads its predecessor's payload — it is
		// unstable under `inputs` edits, and it hides the one declaration an
		// author most needs to see.
		if step.Action == ActionAggregate && step.Payload == "" {
			return withWorkflow(&Error{
				Rule: "V29", Step: step.Name, Field: "payload",
				Message: fmt.Sprintf(
					"step %q: an `action = %q` step must declare `payload = "+
						"\"name@version\"`, and that schema must declare "+
						"`params.field` (%q) as an `ordered_enum`. Median, max, and "+
						"min are defined only over a declared order",
					step.Name, ActionAggregate, aggregateField(step)),
			}, def.Pipeline.Name)
		}

		// V21d: a step with a `threshold` and NO declared `payload` keeps S3
		// behavior — grammar-only validation, no field check, and T3 at runtime.
		// NOT an error.
		//
		// This is the clause that keeps the design honest. Making `payload`
		// mandatory wherever a `threshold` appears would be a tidier rule and
		// would break every S3-era definition, including the committed fixture's
		// `verify`, whose `any(status == unmet)` needs no schema to be correct
		// (T1). Equality has never needed an order and still does not.
		if step.Payload == "" {
			continue
		}

		registered, err := resolveStepSchema(step, resolver)
		if err != nil {
			return withWorkflow(err, def.Pipeline.Name)
		}
		if err := validateStepThresholds(step, registered); err != nil {
			return withWorkflow(err, def.Pipeline.Name)
		}
		if err := validateAggregateSchema(step, registered); err != nil {
			return withWorkflow(err, def.Pipeline.Name)
		}
		if err := validatePassFloorSchema(step, registered); err != nil {
			return withWorkflow(err, def.Pipeline.Name)
		}
	}
	return nil
}

// validatePassFloorSchema is V37a (DKT-870): a declared `pass_floor` must be
// answerable by the step's pinned schema — the field declared and ordered, and
// `at` a value of that order. It is V28a's discipline applied to the exit bar:
// a floor is a POSITION in the declared order, a value with no position has no
// floor to name, and the question is asked at register time rather than at the
// end of a run, on the routing whose `pass` the floor exists to gate. V37 has
// already required `field` and `at` to be non-empty and `payload` to be
// declared; this is the half only the schema can answer.
func validatePassFloorSchema(step *Step, registered *Registered) error {
	if step.PassFloor == nil {
		return nil
	}
	declared, ok := registered.Field(step.PassFloor.Field)
	if !ok {
		return &Error{
			Rule: "V37a", Step: step.Name, Field: "pass_floor",
			Message: fmt.Sprintf(
				"step %q: `pass_floor.field` names %q, which %s does not "+
					"declare. Declared fields: %s",
				step.Name, step.PassFloor.Field, registered.Ref(),
				describeFields(registered)),
		}
	}
	if !declared.Ordered {
		return &Error{
			Rule: "V37a", Step: step.Name, Field: "pass_floor",
			Message: fmt.Sprintf(
				"step %q: `pass_floor.field` names %q, but %s declares no "+
					"`ordered_enum` on it; an exit bar is a position in a "+
					"declared order, and core does not invent one",
				step.Name, step.PassFloor.Field, registered.Ref()),
		}
	}
	if !slices.Contains(declared.Enum, step.PassFloor.At) {
		return &Error{
			Rule: "V37a", Step: step.Name, Field: "pass_floor",
			Message: fmt.Sprintf(
				"step %q: `pass_floor.at` names %q, which is not in the order "+
					"%s declares for %q (%s); an exit bar is a position in the "+
					"declared order, and core does not guess one for an unknown "+
					"value",
				step.Name, step.PassFloor.At, registered.Ref(),
				step.PassFloor.Field, quotedList(declared.Enum)),
		}
	}
	return nil
}

// aggregateField renders `params.field` for a message, or a placeholder when
// the step declares none — V28 has already refused that case, so the
// placeholder only ever appears if the two rules are reached out of order.
func aggregateField(step *Step) string {
	if field, ok := step.Params["field"].(string); ok && field != "" {
		return field
	}
	return "<params.field>"
}

// validateAggregateSchema is V29's second half and V30, over one `aggregate`
// step whose `payload` resolved.
func validateAggregateSchema(step *Step, registered *Registered) error {
	if step.Action != ActionAggregate {
		return nil
	}
	field := aggregateField(step)

	// V29: the declared schema must declare `params.field` as `ordered_enum`.
	declared, ok := registered.Field(field)
	if !ok {
		return &Error{
			Rule: "V29", Step: step.Name, Field: "params",
			Message: fmt.Sprintf(
				"step %q: `params.field` names %q, which %s does not declare. "+
					"Declared fields: %s",
				step.Name, field, registered.Ref(), describeFields(registered)),
		}
	}
	if !declared.Ordered {
		return &Error{
			Rule: "V29", Step: step.Name, Field: "params",
			Message: fmt.Sprintf(
				"step %q: `params.field` names %q, but %s declares no "+
					"`ordered_enum` on it. Median, max, and min are defined ONLY "+
					"over a declared order, so this step could never compute",
				step.Name, field, registered.Ref()),
		}
	}

	// V28a: `route_at`, when declared, must name a value of the field's
	// declared order — the routing floor is a POSITION in that order, and a
	// value with no position has no floor to name (G4's discipline, asked at
	// register time rather than hours into a run). V28 has already required it
	// to be a non-empty string; this is the half only the schema can answer,
	// which is why it lives here beside V29 rather than in the pure-bytes
	// param check.
	if routeAt, ok := step.Params["route_at"].(string); ok && routeAt != "" {
		if !slices.Contains(declared.Enum, routeAt) {
			return &Error{
				Rule: "V28a", Step: step.Name, Field: "params",
				Message: fmt.Sprintf(
					"step %q: `params.route_at` names %q, which is not in the "+
						"order %s declares for %q (%s); a routing floor is a "+
						"position in the declared order, and core does not guess "+
						"one for an unknown value",
					step.Name, routeAt, registered.Ref(), field,
					quotedList(declared.Enum)),
			}
		}
	}

	// V30: the conjunction must be SATISFIABLE.
	return validateAggregateProbe(step, registered, declared)
}

// validateAggregateProbe is V30: the declared schema must ACCEPT an
// aggregate-shaped document.
//
// The aggregate's output is validated at run time against BOTH `aggregate@1`
// and the step's declared schema (§7.6 E5). An instance schema with
// `"additionalProperties": false` makes that conjunction unsatisfiable — and the
// failure would otherwise land at the end of a review fan-out, hours in, on a
// step whose inputs are already spent. Asking the question at register time
// costs one validation.
//
// THE PROBE INVENTS NOTHING. Every value in it comes from the schema being
// checked: the reduced value and the members are the field's own declared enum
// values, and the extra key is drawn from the schema's own declared properties
// when it has another one. It carries the aggregate output keys and one
// carried-through G3 key, so the conjunction it tests is the REAL one (review
// F3) rather than a stripped-down shape that would pass where the real output
// fails.
func validateAggregateProbe(step *Step, registered *Registered, declared schema.Field) error {
	probe, err := json.Marshal([]any{aggregateProbeElement(registered, declared)})
	if err != nil {
		return fmt.Errorf("building the aggregate probe for step %q: %w", step.Name, err)
	}

	if err := registered.ValidatePayload(probe); err != nil {
		return &Error{
			Rule: "V30", Step: step.Name, Field: "payload",
			Message: fmt.Sprintf(
				"step %q: %s cannot accept an `aggregate` output document, so this "+
					"step could never record its result. The output carries the "+
					"reduced value under %q plus %q, %q, %q and the input element's "+
					"own keys, and it must satisfy both %s and %s. The probe was "+
					"refused: %v\nProbe: %s",
				step.Name, registered.Ref(), declared.Name,
				"members", "held", "operator_resolved",
				registered.Ref(), schema.AggregateRef(), err, probe),
		}
	}

	// And against the shipped document, which is the other half of the
	// conjunction. A failure here is a BUILD defect — the probe is constructed
	// to satisfy `aggregate@1` — so it is reported as one rather than blamed on
	// the author.
	builtin, err := schema.Aggregate()
	if err != nil {
		return fmt.Errorf("compiling the embedded %s document: %w",
			schema.AggregateRef(), err)
	}
	if err := builtin.ValidatePayload(probe); err != nil {
		return fmt.Errorf(
			"the %s probe for step %q does not satisfy %s, which is a build "+
				"defect rather than a definition error: %w",
			ActionAggregate, step.Name, schema.AggregateRef(), err)
	}
	return nil
}

// aggregateProbeElement builds one synthetic output element from the schema's
// own declarations.
func aggregateProbeElement(registered *Registered, declared schema.Field) map[string]any {
	// The lowest declared value, deterministically: the probe must be the same
	// document on every run, or V30's verdict would depend on map iteration.
	value := declared.Enum[0]

	element := map[string]any{
		declared.Name: value,
		"members":     []any{value},
		"held":        false,
		// `demoted_from` is deliberately ABSENT: it is optional in aggregate@1
		// and omitted whenever nothing was demoted (D2), so a probe that always
		// carried it would test a shape the common case never produces.
		"operator_resolved": false,
	}

	// One carried-through G3 key, drawn from the schema's own other declared
	// properties so a `required` sibling does not fail the probe for a reason
	// that has nothing to do with the conjunction under test. A schema with no
	// other property gets a synthetic key, which is the case
	// `additionalProperties: false` actually refuses.
	key := probeCarriedKey(registered, declared.Name)
	element[key] = probeCarriedValue(registered, key)
	return element
}

// probeCarriedKey picks the carried-through key: another declared property when
// there is one, else a name no schema would declare.
func probeCarriedKey(registered *Registered, exclude string) string {
	for _, name := range registered.FieldNames() {
		if name != exclude {
			return name
		}
	}
	return "carried_through_key"
}

// probeCarriedValue produces a value the carried key's own declaration accepts,
// so the probe fails only for the reason V30 is asking about. `key` is
// probeCarriedKey's result, passed in rather than recomputed: the caller has
// already resolved it to set the map entry, and a second call would answer
// the same question from the same inputs.
func probeCarriedValue(registered *Registered, key string) any {
	field, ok := registered.Field(key)
	if !ok {
		return "carried-through"
	}
	if len(field.Enum) > 0 {
		return field.Enum[0]
	}
	switch field.Type {
	case "number":
		return 0
	case "integer":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return "carried-through"
}

// resolveStepSchema is V25a: `payload` names a REGISTERED name@version.
func resolveStepSchema(step *Step, resolver SchemaResolver) (*Registered, error) {
	name, version, err := ParsePayloadRef(step.Payload)
	if err != nil {
		// V25 already refused a malformed shape; reaching here means the two
		// grammars disagreed, which is what sharing PayloadShape prevents.
		return nil, &Error{
			Rule: "V25a", Step: step.Name, Field: "payload",
			Message: fmt.Sprintf("step %q: %v", step.Name, err),
		}
	}

	registered, err := resolver.Schema(name, version)
	if errors.Is(err, ErrNotRegistered) {
		return nil, &Error{
			Rule: "V25a", Step: step.Name, Field: "payload",
			Message: fmt.Sprintf(
				"step %q: `payload` names %q, which is not registered. "+
					"Register it first: `docket schema register %s <file.json>`",
				step.Name, step.Payload, step.Payload),
		}
	}
	if err != nil {
		return nil, err
	}
	return registered, nil
}

// validateStepThresholds is V21a-V21c over one step's predicates.
func validateStepThresholds(step *Step, registered *Registered) error {
	// Deterministic iteration: a map's range order varies per run, and an error
	// message that varies is one nobody can test against.
	routings := make([]string, 0, len(step.Threshold))
	for routing := range step.Threshold {
		routings = append(routings, routing)
	}
	sort.Strings(routings)

	for _, routing := range routings {
		predicate, err := ParsePredicate(step.Threshold[routing])
		if err != nil {
			// V21 already refused the shape; this cannot fire through the CLI,
			// and returning the error is cheaper than proving it cannot.
			return &Error{
				Rule: "V21", Step: step.Name, Field: "threshold",
				Message: fmt.Sprintf("step %q: %v", step.Name, err),
			}
		}

		field, declared := registered.Field(predicate.Field)
		// V21a: the field exists as a top-level property of the item schema.
		if !declared {
			return &Error{
				Rule: "V21a", Step: step.Name, Field: "threshold",
				Message: fmt.Sprintf(
					"step %q: `threshold` predicate %q names field %q, which %s does "+
						"not declare. Declared fields: %s",
					step.Name, predicate.Source, predicate.Field, registered.Ref(),
					describeFields(registered)),
			}
		}

		// V21c: an ORDERED operator requires a declared order. It is checked
		// before V21b because "this field has no order" is the more useful
		// sentence for `any(risk >= medium)` on an unordered field than "medium
		// is not a valid value" would be.
		if predicate.Ordered() && !field.Ordered {
			return &Error{
				Rule: "V21c", Step: step.Name, Field: "threshold",
				Message: fmt.Sprintf(
					"step %q: `threshold` predicate %q uses the ordered operator %q, "+
						"but %s declares no `ordered_enum` on field %q. Ordered "+
						"comparisons are defined ONLY for fields whose registered "+
						"schema declares one — core knows a value's position because "+
						"a schema said so, and will not guess",
					step.Name, predicate.Source, predicate.Op, registered.Ref(),
					predicate.Field),
			}
		}

		// V21b: the literal is valid for the field.
		if err := validateLiteral(step, registered, field, predicate); err != nil {
			return err
		}
	}
	return nil
}

// validateLiteral is V21b: a member of `enum` when the field declares one;
// parseable as the declared `type` otherwise.
//
// A field that declares NEITHER an enum nor a type constrains nothing, so any
// literal is legal for it. That is not a gap: the schema author said the value
// may be anything, and inventing a constraint here would be core deciding what
// their field means.
func validateLiteral(step *Step, registered *Registered, field schema.Field, predicate Predicate) error {
	if len(field.Enum) > 0 {
		for _, value := range field.Enum {
			if value == predicate.Literal {
				return nil
			}
		}
		return &Error{
			Rule: "V21b", Step: step.Name, Field: "threshold",
			Message: fmt.Sprintf(
				"step %q: `threshold` predicate %q compares field %q against %q, "+
					"which is not one of the values %s declares: %s",
				step.Name, predicate.Source, predicate.Field, predicate.Literal,
				registered.Ref(), strings.Join(quotedValues(field.Enum), ", ")),
		}
	}

	var err error
	switch field.Type {
	case "number":
		_, err = strconv.ParseFloat(predicate.Literal, 64)
	case "integer":
		_, err = strconv.Atoi(predicate.Literal)
	case "boolean":
		_, err = strconv.ParseBool(predicate.Literal)
	default:
		// "string", or no declared type: nothing to parse.
		return nil
	}
	if err != nil {
		return &Error{
			Rule: "V21b", Step: step.Name, Field: "threshold",
			Message: fmt.Sprintf(
				"step %q: `threshold` predicate %q compares field %q against %q, "+
					"which %s declares as `%s`",
				step.Name, predicate.Source, predicate.Field, predicate.Literal,
				registered.Ref(), field.Type),
		}
	}
	return nil
}

// describeFields lists a schema's declared fields for V21a's message, so an
// author reading a typo is told what they could have written.
func describeFields(registered *Registered) string {
	names := registered.FieldNames()
	if len(names) == 0 {
		return "none — this schema declares no top-level properties on its item schema"
	}
	return strings.Join(quotedValues(names), ", ")
}

func quotedValues(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconv.Quote(v)
	}
	return out
}
