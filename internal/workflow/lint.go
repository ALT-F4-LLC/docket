package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/planner"
)

// Lint applies the §4.3.3 DAG lints L1-L4 to a validated definition.
//
// planner.BuildDAG and planner.TopoSort are REUSED, not duplicated: a
// workflow's `after` edges form a DAG over step names exactly as `depends_on`
// edges form one over issue IDs. The adapter is the whole of the difference —
// step names get dense integer indices, `after` becomes reverse edges, and
// TopoSort's CycleError is caught and re-rendered with step names instead of
// DKT-N. No change to internal/planner is needed or made: a FormatID-shaped
// rendering difference is this layer's business.
func Lint(def *Definition) error {
	if err := lintTopology(def); err != nil {
		return withWorkflow(err, def.Pipeline.Name)
	}
	return nil
}

// stepGraph is the adapter: step names to dense indices and back.
type stepGraph struct {
	def     *Definition
	index   map[string]int
	names   []string
	dag     *planner.DAG
	ordered []*Step
}

// newStepGraph assigns dense indices in DECLARATION order, so every rendering
// derived from the graph — cycle members, orphan lists — comes out in the order
// the author wrote, not in map order.
func newStepGraph(def *Definition) *stepGraph {
	g := &stepGraph{
		def:   def,
		index: make(map[string]int, len(def.Steps)),
		names: make([]string, 0, len(def.Steps)),
	}

	// planner.BuildDAG keys nodes by issue ID and ignores relations naming
	// issues outside the input, so indices start at 1 — a 0 ID would still
	// work, but 1-based keeps the mapping legible against model.FormatID's
	// 1-based ids when debugging.
	issues := make([]*model.Issue, 0, len(def.Steps))
	for i, step := range def.Steps {
		id := i + 1
		g.index[step.Name] = id
		g.names = append(g.names, step.Name)
		g.ordered = append(g.ordered, step)
		issues = append(issues, &model.Issue{ID: id, Title: step.Name})
	}

	// `after` names predecessors, which is exactly `depends_on`: the step
	// DEPENDS ON each of its `after` entries.
	var relations []model.Relation
	for _, step := range def.Steps {
		for _, pred := range step.After {
			predID, ok := g.index[pred]
			if !ok {
				// V9 already rejected this; skip rather than fabricate an edge.
				continue
			}
			relations = append(relations, model.Relation{
				SourceIssueID: g.index[step.Name],
				TargetIssueID: predID,
				RelationType:  model.RelationDependsOn,
			})
		}
	}

	g.dag = planner.BuildDAG(issues, relations)
	return g
}

// nameOf maps a dense index back to its step name. This is the whole of the
// CycleError rendering difference: CycleError.IDs carries indices and renders
// them as DKT-N, so the workflow layer maps them back before reporting.
func (g *stepGraph) nameOf(id int) string {
	if id >= 1 && id <= len(g.names) {
		return g.names[id-1]
	}
	return fmt.Sprintf("#%d", id)
}

func lintTopology(def *Definition) error {
	g := newStepGraph(def)

	// L1: the `after` graph is acyclic.
	if _, err := planner.TopoSort(g.dag); err != nil {
		var cycle *planner.CycleError
		if errors.As(err, &cycle) {
			names := make([]string, 0, len(cycle.IDs))
			for _, id := range cycle.IDs {
				names = append(names, g.nameOf(id))
			}
			return &Error{
				Rule: "L1", Field: "after",
				Message: fmt.Sprintf(
					"cycle detected among steps: %s", strings.Join(names, ", ")),
			}
		}
		return &Error{Rule: "L1", Field: "after", Message: err.Error()}
	}

	// L3: at least one root exists. A root is a step with no `after` — either
	// an explicit `after = []` or the first-step exemption. `loop = true`
	// steps are excluded from ordinary expansion (§11.3 (3)), so one cannot
	// serve as the root of the ordinary topology.
	roots := rootSteps(def)
	if len(roots) == 0 {
		return &Error{
			Rule: "L3", Field: "after",
			Message: "no root step: every step declares `after`, so nothing can start " +
				"(a root declares `after = []`)",
		}
	}

	// L2: every non-root step is reachable from some root.
	if err := lintReachability(def, g, roots); err != nil {
		return err
	}

	// L4: inputs reference only topological predecessors.
	return lintInputOrdering(def, g)
}

// rootSteps returns the steps that start the ordinary topology.
func rootSteps(def *Definition) []*Step {
	var roots []*Step
	for i, step := range def.Steps {
		if step.Loop {
			continue
		}
		if len(step.After) == 0 && (step.HasAfter() || i == 0) {
			roots = append(roots, step)
		}
	}
	return roots
}

// lintReachability is L2. Its exception list is the load-bearing detail:
// §11.2 says routing to a step name "interposes that declared,
// otherwise-unreached step as a successor gate", so "unreached" is a
// LEGITIMATE state for such a step — a naive reachability lint would reject
// the reference instance's own security workflow. `loop = true` steps are
// excluded from ordinary expansion by §11.3 (3) and are likewise not orphans.
func lintReachability(def *Definition, g *stepGraph, roots []*Step) error {
	reached := make(map[string]bool, len(def.Steps))
	var walk func(name string)
	walk = func(name string) {
		if reached[name] {
			return
		}
		reached[name] = true
		id := g.index[name]
		node, ok := g.dag.Nodes[id]
		if !ok {
			return
		}
		// Forward edges go blocker -> blocked, i.e. predecessor -> successor.
		successors := make([]int, 0, len(node.Forward))
		for succ := range node.Forward {
			successors = append(successors, succ)
		}
		sort.Ints(successors)
		for _, succ := range successors {
			walk(g.nameOf(succ))
		}
	}
	for _, root := range roots {
		walk(root.Name)
	}

	interposed := thresholdInterposedSteps(def)

	var orphans []string
	for _, step := range def.Steps {
		if reached[step.Name] {
			continue
		}
		if step.Loop || interposed[step.Name] {
			continue
		}
		orphans = append(orphans, step.Name)
	}

	if len(orphans) > 0 {
		return &Error{
			Rule: "L2", Step: orphans[0], Field: "after",
			Message: fmt.Sprintf(
				"step(s) %s are unreachable from any root step",
				strings.Join(quotedValues(orphans), ", ")),
		}
	}
	return nil
}

// thresholdInterposedSteps collects the steps some threshold routes to by
// name. §11.2: such a step "becomes ready next, and on its pass execution
// resumes at the routing step's ordinary downstream" — it is reached by
// routing, so being unreached in the `after` topology is legitimate for it.
// One classification, shared with expansion and the engine's readiness latch:
// a second reading of "interposed" is how the lint and the latch would drift
// (DKT-38's original defect was exactly such a split).
func thresholdInterposedSteps(def *Definition) map[string]bool {
	interposed := make(map[string]bool)
	for _, name := range InterposedSteps(def) {
		interposed[name] = true
	}
	return interposed
}

// lintInputOrdering is L4: `inputs` reference only steps that are topological
// predecessors (transitively `after`), so an input can never resolve to an
// artifact that does not exist yet.
//
// `loop = true` steps are excepted: their inputs bind per §11.3 (3) within
// their ordinal, falling back to the highest earlier ordinal per input, so
// their producers are not required to precede them in the ordinary topology.
func lintInputOrdering(def *Definition, g *stepGraph) error {
	for _, step := range def.Steps {
		if step.Loop {
			continue
		}
		ancestors := ancestorsOf(g, step.Name)

		for _, input := range step.Inputs {
			if input == "issue.body" || input == "issue.diff" {
				continue
			}
			// `issue.latest.<kind>` names no producer step: it resolves over
			// whatever the issue has recorded by the consumer's ordinal,
			// falling back per §7.4 — resolving to nothing is a legal answer,
			// so there is no "artifact that does not exist yet" for L4 to
			// guard against. Without this skip the shape below would read
			// `issue.latest` as a step name and refuse every non-loop
			// consumer of the form.
			if _, ok := LatestKind(input); ok {
				continue
			}
			// `issue.linked.<relation>.<kind>` (DKT-547) likewise names no
			// producer step: its artifact was recorded under ANOTHER issue and
			// pinned at activation, so it exists before any step of this
			// workflow runs — there is no ordering for L4 to enforce.
			if _, _, ok := LinkedInput(input); ok {
				continue
			}
			m := inputShape.FindStringSubmatch(input)
			if m == nil {
				continue // V11 already rejected it.
			}
			producer := m[1]
			if producer == step.Name || ancestors[producer] {
				continue
			}
			return &Error{
				Rule: "L4", Step: step.Name, Field: "inputs",
				Message: fmt.Sprintf(
					"step %q: `inputs` entry %q names step %q, which is not a predecessor of %q — "+
						"its artifact would not exist yet",
					step.Name, input, producer, step.Name),
			}
		}
	}
	return nil
}

// ancestorsOf walks reverse edges (blocked -> blockers) to collect every
// transitive predecessor of a step.
func ancestorsOf(g *stepGraph, name string) map[string]bool {
	ancestors := make(map[string]bool)
	var walk func(n string)
	walk = func(n string) {
		node, ok := g.dag.Nodes[g.index[n]]
		if !ok {
			return
		}
		preds := make([]int, 0, len(node.Reverse))
		for pred := range node.Reverse {
			preds = append(preds, pred)
		}
		sort.Ints(preds)
		for _, pred := range preds {
			pname := g.nameOf(pred)
			if ancestors[pname] {
				continue
			}
			ancestors[pname] = true
			walk(pname)
		}
	}
	walk(name)
	return ancestors
}
