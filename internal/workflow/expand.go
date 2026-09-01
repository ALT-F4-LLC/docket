package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// Step statuses expansion can assign. The full ten-status machine is phase 3's
// (TDD §6.2); expansion writes only these three, and `ready` is never among
// them because `ready` is COMPUTED at read time, never stored as intent.
const (
	StatusPending = "pending"
	StatusSkipped = "skipped"
)

// StepInstance is one expanded step row, before it is written. It carries the
// decomposed identity (name/ordinal/sibling) AND the rendered `instance`
// string, because §11.3 makes the rendered form the step's public identity —
// it appears in wire shapes, events, and error strings — and a stored rendering
// cannot drift from a formatting helper someone edits later.
type StepInstance struct {
	Name         string
	Ordinal      int
	SiblingIndex *int
	Instance     string
	Kind         string
	Executor     string
	Class        string
	Status       string
	MaxAttempts  *int
	ExpectedCost float64
	Metadata     map[string]any
	// Packet is the step's declared packet files with `{executor}` already
	// substituted FOR THIS SIBLING (§1.1.1).
	//
	// Substitution happens at expansion rather than at render because a fanout
	// step is one step whose siblings carry different hints: resolving here is
	// what lets one declaration produce per-sibling paths, and it puts the
	// resolved list on the row that the closure-size arithmetic already reads.
	Packet []string
}

// RenderInstance renders the §11.3 step identity `name@k#i`. The `#i` suffix is
// absent when the step is not fanned out.
func RenderInstance(name string, ordinal int, sibling *int) string {
	if sibling == nil {
		return fmt.Sprintf("%s@%d", name, ordinal)
	}
	return fmt.Sprintf("%s@%d#%d", name, ordinal, *sibling)
}

// Expand is the pure function of engine-core §2: "Expansion is a pure function
// of (issue kind, labels, pipeline definitions @ pinned version). Same issue,
// same pipeline version => identical steps, every time."
//
// It produces, deterministically (TDD §5.3.1):
//
//   - one row per non-`loop` [[step]], at ordinal 0, named `name@0`;
//   - for a `fanout` step, one row per hint — `name@0#0 .. name@0#n-1`, in
//     DECLARED HINT ORDER, the index being the position in the `fanout` array
//     and never a map iteration;
//   - steps whose `when` is false created with status `skipped`, NOT omitted —
//     §11.1 says "step is skipped when false", and a CREATED skipped step is
//     what makes a downstream `after` resolvable and the topology identical
//     regardless of the predicate's value;
//   - `loop = true` steps NOT created (§11.3 (3): "excluded from ordinary
//     expansion");
//   - steps named as a `threshold` step-name routing target CREATED, in
//     status `pending` — §11.2's interposed gate. The engine's readiness
//     latch (R3's interposition clause) is what keeps one unready until a
//     routing predecessor's recorded routing names it, and a routing that
//     resolves anywhere else terminalizes it `skipped`.
//
// Determinism is the property this function exists to have, and the one a
// careless edit breaks: it walks def.Steps and step.Fanout, both slices in
// declaration order, and touches no map. TestExpansionIsDeterministic expands
// the fixture 100 times and requires byte-identical rows, so a walk over a map
// fails loudly rather than flapping in production — Go randomizes map iteration
// precisely so this class of bug surfaces early.
func Expand(def *Definition, s Subject, ordinal int) []StepInstance {
	out := make([]StepInstance, 0, len(def.Steps))

	for _, step := range def.Steps {
		// (3) of §11.3: loop bodies instantiate at loop entry, not here.
		if step.Loop {
			continue
		}
		out = append(out, expandStep(step, s, ordinal)...)
	}

	return out
}

// ExpandOrdinal is expansion at a LOOP ORDINAL — §11.3 clauses (3) and (4),
// the counterpart of Expand's ordinary expansion at ordinal 0.
//
// Two sets of steps instantiate at ordinal k, and only these two:
//
//   - clause (3): the SERVING `loop = true` steps, supplied as `bodies` —
//     the triggering step's cluster (LoopBodiesFor), which is every body when
//     no `serves` is declared anywhere;
//   - clause (4): the cluster's `after_loop` roots AND THEIR DOWNSTREAM CHAIN
//     ("`after_loop` and its downstream chain re-instantiate at ordinal k"),
//     supplied as `downstream` because the chain is a property of the
//     definition's shape that the caller already computed for the sweep.
//
// Both sets are the CALLER's, computed for the entry's triggering step, so
// the instantiation and the supersede sweep read one answer to "what does
// this cluster re-run" rather than two that can drift.
//
// EVERYTHING ELSE IS LEFT AT ITS EXISTING ORDINAL. `implement` is upstream of
// `after_loop`: it does not re-run, its artifact is not reproduced, and §7.4's
// per-input fallback exists precisely so a step at ordinal k can still bind it.
// A body OUTSIDE the triggering cluster is likewise left alone — its rounds are
// its own triggers' to mint (§11.3 cluster scoping, DKT-544).
//
// Sharing Expand's per-step body is the point. The status/class/fanout/metadata
// rules are applied by one function for both ordinals, so ordinal 1's topology
// cannot drift from ordinal 0's — a second implementation of "one row per hint,
// in declared order" is exactly how it would.
func ExpandOrdinal(
	def *Definition, s Subject, ordinal int, bodies, downstream map[string]bool,
) []StepInstance {
	out := make([]StepInstance, 0, len(def.Steps))

	for _, step := range def.Steps {
		// Clause (3) or clause (4); a step in neither set does not re-instantiate.
		if !(step.Loop && bodies[step.Name]) && !downstream[step.Name] {
			continue
		}
		out = append(out, expandStep(step, s, ordinal)...)
	}

	return out
}

// ServesTrigger reports whether a step's loop-cluster declaration covers a
// triggering step name. An empty `serves` covers EVERY trigger — the
// backward-compatible reading under which a workflow with no `serves`
// anywhere has one cluster spanning every body and root (§11.3).
func ServesTrigger(step *Step, trigger string) bool {
	return len(step.Serves) == 0 || slices.Contains(step.Serves, trigger)
}

// LoopBodiesFor is the set of `loop = true` step names serving one trigger —
// §11.3 clause (3) scoped to the step whose routing entered the loop.
func LoopBodiesFor(def *Definition, trigger string) map[string]bool {
	out := make(map[string]bool)
	for _, step := range def.Steps {
		if step.Loop && ServesTrigger(step, trigger) {
			out[step.Name] = true
		}
	}
	return out
}

// ExpandStepAt renders ONE named step's instance rows at an ordinal — the same
// rows ExpandOrdinal would have produced for it, and nothing else.
//
// It exists for §11.3 (2)'s repair case: a step superseded at ordinal k-1 whose
// successor the ordinal-k expansion did not produce has to be created before
// the transaction closes, and creating it means expanding exactly one step. The
// caller cannot reach for ExpandOrdinal there, because ExpandOrdinal always
// also emits the `loop` bodies — which that caller has already written, and
// which a second pass would duplicate.
//
// An unknown name yields no rows rather than an error: the one caller derives
// the name from a stored `instance`, and a MATERIALIZED name (`<step>-held`) is
// deliberately not in the definition. Its successor is its routing step's new
// instance, not a held row expansion never wrote.
func ExpandStepAt(def *Definition, s Subject, ordinal int, name string) []StepInstance {
	step := StepByName(def, name)
	if step == nil {
		return nil
	}
	return expandStep(step, s, ordinal)
}

// expandStep renders one [[step]] into its instance rows at an ordinal: one row,
// or one per `fanout` hint in declared order.
//
// It is shared by Expand, ExpandOrdinal, and ExpandStepAt so all three agree by
// construction (see ExpandOrdinal's comment).
func expandStep(step *Step, s Subject, ordinal int) []StepInstance {
	status := StatusPending
	if !WhenHolds(step.When, s) {
		status = StatusSkipped
	}

	class := step.Class
	if class == "" {
		// §11.1 `class`: "default = executor value". V23 records the rule; the
		// default is applied HERE rather than at parse so `parsed` stores what
		// the author wrote and the engine derives the rest — a defaulted value
		// baked into the pinned form would make a later change to the defaulting
		// rule silently re-interpret it.
		class = step.Executor
	}

	base := StepInstance{
		Name:         step.Name,
		Ordinal:      ordinal,
		Kind:         stepKind(step),
		Executor:     step.Executor,
		Class:        class,
		Status:       status,
		MaxAttempts:  step.MaxAttempts,
		Metadata:     step.Metadata,
		ExpectedCost: expectedCost(step),
	}

	if len(step.Fanout) == 0 {
		base.Instance = RenderInstance(step.Name, ordinal, nil)
		base.Packet = substitutePacket(step.Packet, step.Executor)
		return []StepInstance{base}
	}

	// One sibling per hint, in declared order. The hint is the sibling's
	// executor: `fanout` is "[executor hints]" (§11.1) and each sibling is an
	// instance of one step differing only in which hint it carries.
	out := make([]StepInstance, 0, len(step.Fanout))
	for i, hint := range step.Fanout {
		sibling := i
		row := base
		row.SiblingIndex = &sibling
		row.Instance = RenderInstance(step.Name, ordinal, &sibling)
		row.Executor = hint
		if step.Class == "" {
			row.Class = hint
		}
		// PER SIBLING, against THIS sibling's hint — the whole point of the
		// token. A literal entry substitutes to itself, so a hint family
		// sharing one file needs no special case.
		row.Packet = substitutePacket(step.Packet, hint)
		out = append(out, row)
	}
	return out
}

// stepKind maps a step's §11.1 "exactly one of" alternative to the `kind`
// column's vocabulary: 'executor' | 'action' | 'human' | 'vote'. A fanout step
// is an executor step expanded per hint, so its siblings are `executor` rows —
// `fanout` describes how many, not what kind.
func stepKind(step *Step) string {
	switch step.StepClass() {
	case ClassType:
		return step.Type
	case ClassAction:
		return ClassAction
	default:
		// ClassExecutor and ClassFanout both produce executor rows. V5
		// guaranteed exactly one alternative is declared, so no other value
		// reaches here from a registered definition.
		return ClassExecutor
	}
}

// expectedCost is §11.1's `expected_cost`, default 0. It is stored on the row
// and emitted on `next` rows at this stage but ENFORCES NOTHING — S6 owns the
// budget floor. Storing it now keeps the wire shape whole: a later stage adding
// a field to `next` rows would be a compat event for dispatchers already
// consuming them.
func expectedCost(step *Step) float64 {
	if step.ExpectedCost == nil {
		return 0
	}
	return *step.ExpectedCost
}

// InterposedSteps returns the step names some `threshold` routes to by name —
// §11.2's interposed gates.
//
// The test is BEING NAMED AS A ROUTING TARGET, never `after`-reachability.
// V8 forces every non-first step to declare `after`, so a real-world
// interposed gate declares `after = [routing-step]` (the canonical shape: it
// anchors the gate in the topology and gives the staged closure its stage) —
// an unreachability test would classify every such gate as ordinary and hand
// it R3's plain readiness, which is exactly DKT-38's every-run panel seating.
// The set is computed from the definition rather than stored, so it cannot
// drift from the thresholds that define it.
//
// These are the steps L2's reachability lint exempts, and they are expanded in
// status `pending` like any other: readiness latches them until a routing
// predecessor's recorded routing names them, and a routing that resolves
// anywhere else terminalizes them `skipped` (engine reconcile).
func InterposedSteps(def *Definition) []string {
	routings := make(map[string]struct{})
	for _, step := range def.Steps {
		for _, target := range ThresholdTargets(step.Threshold) {
			routings[target] = struct{}{}
		}
	}
	if len(routings) == 0 {
		return nil
	}

	// Walk def.Steps rather than the map, so the result is in declaration
	// order and two runs agree.
	var out []string
	for _, step := range def.Steps {
		if _, ok := routings[step.Name]; ok {
			out = append(out, step.Name)
		}
	}
	return out
}

// ThresholdTargets returns the step-name routings of one threshold table — the
// keys outside the closed {fix-loop, waiting-human, pass} vocabulary — sorted
// for determinism (TOML tables decode into Go maps, which lose declaration
// order; ThresholdOrder makes the same recovery for evaluation).
func ThresholdTargets(threshold map[string]string) []string {
	var out []string
	for routing := range threshold {
		if slices.Contains(thresholdRoutings, routing) {
			continue
		}
		out = append(out, routing)
	}
	slices.Sort(out)
	return out
}

// RoutingPredecessors returns the names of the steps whose `threshold` routes
// to `name`, in declaration order — the steps whose recorded routing decides
// whether the interposed gate `name` ever becomes ready. Empty for a step no
// threshold names, which is what makes ordinary steps invisible to the
// readiness latch.
func RoutingPredecessors(def *Definition, name string) []string {
	var out []string
	for _, step := range def.Steps {
		if slices.Contains(ThresholdTargets(step.Threshold), name) {
			out = append(out, step.Name)
		}
	}
	return out
}

// ContextSize is the byte size of the closure an expanded step would carry —
// the engine-core §8 measurement the caps in §5.5 are compared against.
//
// At this stage the closure is the issue snapshot plus the step's own metadata:
// input artifacts do not exist until steps complete (phase 3), so an expansion-
// time measurement is necessarily of what activation itself froze. That is the
// right thing to measure here: engine-core §8 puts the check "at expansion
// time — the fix is a pipeline/contract change, visible before spend", and the
// body snapshot is the part an oversized issue makes oversized.
// packetBytes reports the size of one declared packet file, or 0 when the run
// pinned no such path. It is a function rather than a map so activation can
// answer from whatever it already holds without building an index per step.
func ContextSize(
	bodySnapshot, issueSnapshot string, inst StepInstance, packetBytes func(string) int,
) int {
	n := len(bodySnapshot) + len(issueSnapshot) + len(inst.Instance)
	for k, v := range inst.Metadata {
		n += len(k) + len(fmt.Sprint(v))
	}

	// The declared packet files count toward the closure (§1.5).
	//
	// THIS IS NOT OPTIONAL BOOKKEEPING. engine-core §8 records closure size on
	// the step as the honest cost figure, and §11.1's caps refuse an oversized
	// closure AT EXPANSION TIME — "the fix is a pipeline/contract change,
	// visible before spend, not a silent 107KB spawn". Inlining file bodies
	// without counting them would leave the recorded figure understating real
	// cost and the caps no longer binding, which is exactly the failure that
	// sentence exists to prevent.
	//
	// A file the run did not pin contributes 0 here rather than refusing: the
	// refusal is activation's (a declared entry must be pinned), and a size
	// helper that also enforced existence would put one rule in two places.
	if packetBytes != nil {
		for _, entry := range inst.Packet {
			n += len(entry) + packetBytes(entry)
		}
	}
	return n
}

// ArtifactKind reports the artifact kind a step produces, per TDD §4.3.1. It is
// exported because activation and phase 3's input resolution both need it and
// must agree — the kind comes from a DIFFERENT field per step class, and two
// readers deriving it independently is how they drift.
//
//	executor      -> `emits` (required, V7)
//	action        -> `params.output`
//	fanout        -> `emits`, applying to every sibling
//	human | vote  -> none; a gate records a decision, not an artifact
func ArtifactKind(step *Step) string {
	switch step.StepClass() {
	case ClassAction:
		out, _ := step.Params["output"].(string)
		return out
	case ClassType:
		return ""
	default:
		return step.Emits
	}
}

// StepByName finds a step in a definition by its §11.1 `name`, which V4 made
// unique within the workflow.
func StepByName(def *Definition, name string) *Step {
	for _, step := range def.Steps {
		if step.Name == name {
			return step
		}
	}
	return nil
}

// ParseInstance decomposes a rendered `name@k#i` identity back into its parts.
// It is the inverse of RenderInstance and exists so a caller reading the stored
// `instance` column never re-derives the format by hand.
func ParseInstance(instance string) (name string, ordinal int, sibling *int, err error) {
	at := strings.LastIndex(instance, "@")
	if at < 0 {
		return "", 0, nil, fmt.Errorf("invalid step instance %q: no @ordinal", instance)
	}
	name, rest := instance[:at], instance[at+1:]

	if hash := strings.Index(rest, "#"); hash >= 0 {
		var i int
		if _, err := fmt.Sscanf(rest[hash+1:], "%d", &i); err != nil {
			return "", 0, nil, fmt.Errorf("invalid step instance %q: bad sibling index", instance)
		}
		sibling = &i
		rest = rest[:hash]
	}
	if _, err := fmt.Sscanf(rest, "%d", &ordinal); err != nil {
		return "", 0, nil, fmt.Errorf("invalid step instance %q: bad ordinal", instance)
	}
	return name, ordinal, sibling, nil
}
