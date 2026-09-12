package engine

import (
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The `pass_floor` exit bar (DKT-870): a step whose routing resolved to `pass`
// while its own recorded payload still holds work at or above a declared
// position does not exit — it parks, and an operator decides.
//
// It exists because "converged" in the ledger sometimes meant "dispositioned":
// a threshold reads whatever field its author chose, and a routing step's
// self-reported disposition can contradict the evidence it recorded beside it.
// RUN-58's reconcile@1 routed `pass` and the loop exited with all 16 clusters
// open, SIX at the order's high position, none held and none operator-resolved
// — including the fail-open siblings of the class round 0 had closed. Nothing
// engine-visible stood between that pass and issue completion.
//
// THE FLOOR IS THE AUTHOR'S, AND EVERY VALUE IS COMPARED BY POSITION. `field`
// and `at` are opaque tokens positioned in the step's PINNED schema order —
// `route_at`'s exact discipline (aggregate.go) — so core acquires no opinion
// about severities: a workflow whose order ranks ripeness or confidence gets
// the identical check. `held` and `operator_resolved` are the engine's own
// packaging vocabulary (§7.6), and elements carrying either are exempt because
// both already route through a decision channel: a held cluster gates the step
// for an operator, and a resolved one records the operator's acceptance — the
// approved-hold resume must not re-park on the decision it just received.
type passFloorResult struct {
	// Standing counts the payload elements at or above the floor with neither
	// exemption — the number the park's reason names.
	Standing int
}

// passFloorStanding counts the elements a `pass` would leave standing above
// the declared floor, or 0 when the floor is absent or cannot measure.
//
// IT FAILS TOWARD THE PASS, the direction every completion-side guard here
// fails (roundMovedNothing, DKT-588's hand-back): a nil resolver (V37 makes
// this unreachable through `workflow register`; reachable from a restored
// database), a floor value outside the declared order, an element without the
// field, and an element whose value has no position are all absent
// measurements, never breaches — the guard must not act on evidence it cannot
// read, and overriding a routing on a guess would be a silent misroute wearing
// a park's clothes.
func passFloorStanding(
	floor *workflow.PassFloor, payloads []map[string]any, order OrderResolver,
) passFloorResult {
	var out passFloorResult
	if floor == nil || order == nil {
		return out
	}
	want, ok := order.Position(floor.Field, floor.At)
	if !ok {
		return out
	}
	for _, element := range payloads {
		if flagged(element, KeyHeld) || flagged(element, KeyOperatorResolved) {
			continue
		}
		raw, present := element[floor.Field]
		if !present || raw == nil {
			continue
		}
		value, err := normalizeScalar(raw)
		if err != nil {
			continue // A composite value has no position to compare.
		}
		got, ok := order.Position(floor.Field, value)
		if !ok {
			continue
		}
		if got >= want {
			out.Standing++
		}
	}
	return out
}

// flagged reads one of the aggregate's boolean packaging keys off an element.
// Only an explicit JSON `true` counts: the keys are the engine's own output
// vocabulary (§7.6), absent on payloads no aggregate produced, and absence of
// a decision is not a decision.
func flagged(element map[string]any, key string) bool {
	v, _ := element[key].(bool)
	return v
}
