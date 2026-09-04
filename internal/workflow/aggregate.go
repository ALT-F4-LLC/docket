package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
)

// The `aggregate` builtin's register-time rules — V27's action clause and V28
// (docs/tdd/payloads-thresholds.md §7.1).
//
// The vocabulary lives HERE rather than in internal/engine because both need it
// and the engine imports this package: a register-time rule and a run-time
// dispatch that spelled the builtin's name twice would disagree on the first
// rename, and the disagreement would read as "my trusted `aggregate` command
// never ran".

// ActionAggregate is the one builtin engine-spec §2 names: "One is builtin and
// generic: action = "aggregate"".
const ActionAggregate = "aggregate"

// BuiltinActions are the action names core COMPUTES ITSELF. There is exactly
// one, and every other action name is a user-trusted command (§6.2).
var BuiltinActions = []string{ActionAggregate}

// ReservedActions are the action names core RESERVES — the names that resolve
// builtin-first and therefore never consult the trust store (§6.1 B1).
//
// It is a separate list from BuiltinActions on purpose. The two are equal today
// and TestReservedActionsAreAllImplemented asserts it; the moment they are not,
// V27 refuses a step naming a reserved-but-unimplemented action rather than
// letting it fall through to a trust lookup that would silently run an
// operator's command under a name core intends to take.
var ReservedActions = []string{ActionAggregate}

// AggregateParamKeys are the five keys §7.1's table declares, in its own order.
// V28's "no other keys" is checked against exactly this list.
var AggregateParamKeys = []string{"field", "method", "hold_spread", "output", "route_at"}

// AggregateMethods is the closed reduction vocabulary, in §2's order.
var AggregateMethods = []string{"median", "max", "min"}

// validateAction is V27's action clause and V28, over one step.
//
// It runs for every `action` step, builtin or not: V27 applies to the name and
// V28 only to `aggregate`, because core reads no key of a non-builtin action's
// params (§6.2) and validating a bag it never opens would be inventing a
// contract for someone else's command.
func validateAction(step *Step) error {
	if step.StepClass() != ClassAction {
		return nil
	}

	// V27: a reserved action name core does not implement. Unreachable while
	// the two lists agree, and it exists so that adding a name to the reserved
	// set ahead of its implementation is a register-time refusal rather than a
	// trust lookup nobody expected.
	if slices.Contains(ReservedActions, step.Action) && !slices.Contains(BuiltinActions, step.Action) {
		return &Error{
			Rule: "V27", Step: step.Name, Field: "action",
			Message: fmt.Sprintf(
				"step %q: `action` names %q, which this build reserves as a builtin "+
					"but does not compute; it would neither run as a builtin nor be "+
					"looked up in the trust store",
				step.Name, step.Action),
		}
	}

	if step.Action != ActionAggregate {
		return nil
	}
	if err := validateAggregateInputs(step); err != nil {
		return err
	}
	return validateAggregateParams(step)
}

// validateAggregateInputs is V31: `aggregate` requires a non-empty `inputs`.
//
// The builtin's input IS its declared `inputs` artifacts (engine-spec §2, as
// amended), so an aggregate that declares none can never compute —
// it would reduce over an empty set, produce an empty payload, hold nothing,
// and pass its threshold vacuously. That is V29's spirit applied to the DATA
// side: a step whose declaration makes its own computation impossible is a
// register-time refusal, not a run that quietly succeeds at nothing.
//
// It is checked BEFORE the params, so an author who declared no input hears
// about the missing input rather than about a `method` typo they can only fix
// second. V11 has already required `inputs` to be well-FORMED where present;
// this is the rule that requires it to be there at all.
func validateAggregateInputs(step *Step) error {
	if len(step.Inputs) > 0 {
		return nil
	}
	return &Error{
		Rule: "V31", Step: step.Name, Field: "inputs",
		Message: fmt.Sprintf(
			"step %q: `aggregate` requires a non-empty `inputs` — the builtin "+
				"reduces the payloads of the artifacts it names, so a step "+
				"declaring none has nothing to compute over",
			step.Name),
	}
}

// validateAggregateParams is V28: `aggregate` requires `field`, `method`, and
// `output`; `hold_spread` is an integer >= 0 when present; `route_at` is a
// non-empty string when present; and NO OTHER KEYS.
//
// The discipline is every V-rule's. A typo'd `method = "medain"` is otherwise
// discovered hours into a run, on a step whose inputs are already spent — and an
// unread key is a declaration the author believes is doing something.
func validateAggregateParams(step *Step) error {
	fail := func(field, format string, args ...any) error {
		return &Error{
			Rule: "V28", Step: step.Name, Field: field,
			Message: fmt.Sprintf("step %q: ", step.Name) + fmt.Sprintf(format, args...),
		}
	}

	// Deterministic iteration: a map's range order varies per run, and an error
	// message that varies is one nobody can test against.
	keys := make([]string, 0, len(step.Params))
	for key := range step.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !slices.Contains(AggregateParamKeys, key) {
			return fail("params",
				"`params.%s` is not a parameter of `aggregate`, which takes exactly %s",
				key, quotedList(AggregateParamKeys))
		}
	}

	field, _ := step.Params["field"].(string)
	if field == "" {
		return fail("params",
			"`aggregate` requires `params.field` — the payload property to reduce")
	}

	method, _ := step.Params["method"].(string)
	if !slices.Contains(AggregateMethods, method) {
		return fail("params",
			"`params.method` must be one of %s, got %q",
			quotedList(AggregateMethods), method)
	}

	// `output` is already V11's — an action step must declare it — and is
	// restated here so an `aggregate` step's params are checked as one
	// table rather than two halves an author has to assemble.
	if out, _ := step.Params["output"].(string); out == "" {
		return fail("params",
			"`params.output` is required: it is the artifact kind this step produces")
	}

	if raw, present := step.Params["hold_spread"]; present {
		hold, ok := AsInteger(raw)
		if !ok || hold < 0 {
			return fail("params",
				"`params.hold_spread` must be an integer >= 0, got %v", raw)
		}
	}

	// `route_at` is optional; when present it must be a non-empty string. WHICH
	// strings it may name is the declared order's business, and the order lives
	// in the step's registered schema — so the membership check is V28a's, in
	// ValidateSchemas, beside V29's order check. This clause is pure bytes and
	// stays here with V27, V28, and V31.
	if raw, present := step.Params["route_at"]; present {
		if s, ok := raw.(string); !ok || s == "" {
			return fail("params",
				"`params.route_at` must be a non-empty string naming a value of "+
					"`params.field`'s declared order, got %v", raw)
		}
	}
	return nil
}

// AsInteger reads an integer out of the opaque bag, tolerating every shape a
// TOML decode or a JSON round-trip produces. `parsed` stores a definition as
// JSON, so the same declaration arrives as int64 from a file and float64 from
// the database, and both are the same number.
//
// Exported because internal/engine's `aggregate` builtin (aggregate.go)
// coerces `params.hold_spread` through the identical switch at run time — the
// same tolerance V28 already applies at register time — and a run-time copy
// of this switch is exactly the kind of two-places-that-can-drift this
// package's own doc comment warns about.
func AsInteger(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
