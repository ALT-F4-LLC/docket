package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// renderRows renders expanded rows to a stable string — the comparison unit
// for the determinism proof and for the topology assertions below.
func renderRows(rows []StepInstance) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%.2f\n",
			r.Instance, r.Kind, r.Status, r.Executor, r.Class, r.ExpectedCost)
	}
	return b.String()
}

// TestExpansionIsDeterministic is §5.3.1's central obligation, asserted
// directly: expansion is a pure function of (issue kind, labels, pipeline
// definition @ pinned version), so the fixture expands to byte-identical rows
// every time.
//
// 100 iterations, because the only realistic way this property breaks is an
// expansion that walks a Go map without sorting — and Go randomizes map
// iteration order per run precisely so that bug surfaces rather than lurking.
// A single-iteration test would pass on a map walk roughly whenever it felt
// like it.
func TestExpansionIsDeterministic(t *testing.T) {
	def := mustParseFixture(t)
	subject := Subject{Kind: "task", Labels: []string{"backend"}}

	want := renderRows(Expand(def, subject, 0))
	if want == "" {
		t.Fatal("the fixture expanded to zero rows")
	}

	for i := 1; i < 100; i++ {
		if got := renderRows(Expand(def, subject, 0)); got != want {
			t.Fatalf("expansion %d differs from expansion 0:\n got:\n%s\nwant:\n%s", i, got, want)
		}
	}
}

// TestExpandFanoutIsInDeclaredHintOrder is the §5.3.1 fanout rule: one row per
// hint, `#0..#n-1`, in the order the `fanout` array declares — the index is the
// POSITION in that array, never a map iteration.
func TestExpandFanoutIsInDeclaredHintOrder(t *testing.T) {
	def := mustParseFixture(t)
	rows := Expand(def, Subject{Kind: "task"}, 0)

	review := StepByName(def, "review")
	if review == nil || len(review.Fanout) == 0 {
		t.Fatal("the fixture's `review` step is no longer a fanout step")
	}

	var got []StepInstance
	for _, r := range rows {
		if r.Name == "review" {
			got = append(got, r)
		}
	}

	if len(got) != len(review.Fanout) {
		t.Fatalf("expanded %d `review` siblings, want %d (one per hint)",
			len(got), len(review.Fanout))
	}
	for i, hint := range review.Fanout {
		wantInstance := fmt.Sprintf("review@0#%d", i)
		if got[i].Instance != wantInstance {
			t.Errorf("sibling %d instance = %q, want %q", i, got[i].Instance, wantInstance)
		}
		if got[i].SiblingIndex == nil || *got[i].SiblingIndex != i {
			t.Errorf("sibling %d sibling_index = %v, want %d", i, got[i].SiblingIndex, i)
		}
		// The hint is the sibling's executor: `fanout` is "[executor hints]"
		// and siblings differ only in which hint they carry.
		if got[i].Executor != hint {
			t.Errorf("sibling %d executor = %q, want the declared hint %q",
				i, got[i].Executor, hint)
		}
	}
}

// TestExpandWhenFalseCreatesSkipped is §11.1's "step is skipped when false",
// and the reason it is stated as a CREATED row rather than an omission: a
// downstream `after` must stay resolvable and the topology must be identical
// regardless of the predicate's value.
func TestExpandWhenFalseCreatesSkipped(t *testing.T) {
	def := &Definition{
		Pipeline: Pipeline{Name: "conditional", Version: 1},
		Steps: []*Step{
			{Name: "first", Executor: "one", Emits: "a", After: []string{}},
			{Name: "maybe", Executor: "two", Emits: "b", After: []string{"first"},
				When: "kind == bug"},
			{Name: "last", Executor: "three", Emits: "c", After: []string{"maybe"}},
		},
	}

	held := Expand(def, Subject{Kind: "bug"}, 0)
	notHeld := Expand(def, Subject{Kind: "task"}, 0)

	// The topology is IDENTICAL either way: same rows, same order, same
	// instances. Only the status of the conditional step differs.
	if len(held) != len(notHeld) {
		t.Fatalf("when-true expanded %d rows, when-false %d; topology must be identical",
			len(held), len(notHeld))
	}
	for i := range held {
		if held[i].Instance != notHeld[i].Instance {
			t.Errorf("row %d instance differs: %q vs %q",
				i, held[i].Instance, notHeld[i].Instance)
		}
	}

	statusOf := func(rows []StepInstance, name string) string {
		for _, r := range rows {
			if r.Name == name {
				return r.Status
			}
		}
		return "<absent>"
	}

	if got := statusOf(held, "maybe"); got != StatusPending {
		t.Errorf("when-true `maybe` status = %q, want %q", got, StatusPending)
	}
	if got := statusOf(notHeld, "maybe"); got != StatusSkipped {
		t.Errorf("when-false `maybe` status = %q, want %q (created, not omitted)",
			got, StatusSkipped)
	}
	// Stated as its own assertion, because "omitted" is the bug this rule
	// exists to prevent and a status check alone would not catch it.
	if statusOf(notHeld, "maybe") == "<absent>" {
		t.Error("a when-false step was OMITTED; §11.1 requires it created `skipped`")
	}
}

// TestExpandOmitsLoopSteps is §11.3 (3): `loop = true` steps are "excluded from
// ordinary expansion" and instantiate at loop entry, which is phase 4's.
func TestExpandOmitsLoopSteps(t *testing.T) {
	def := mustParseFixture(t)
	rows := Expand(def, Subject{Kind: "task"}, 0)

	var loopSteps []string
	for _, step := range def.Steps {
		if step.Loop {
			loopSteps = append(loopSteps, step.Name)
		}
	}
	if len(loopSteps) == 0 {
		t.Fatal("the fixture no longer declares a `loop = true` step")
	}

	for _, r := range rows {
		if slices.Contains(loopSteps, r.Name) {
			t.Errorf("expansion created %q at ordinal 0; `loop = true` steps are "+
				"excluded from ordinary expansion (§11.3 (3))", r.Instance)
		}
	}

	// Every OTHER step is present exactly once at ordinal 0 — the loop
	// exclusion must not take anything else with it.
	for _, step := range def.Steps {
		if step.Loop {
			continue
		}
		want := 1
		if len(step.Fanout) > 0 {
			want = len(step.Fanout)
		}
		var n int
		for _, r := range rows {
			if r.Name == step.Name {
				n++
			}
		}
		if n != want {
			t.Errorf("step %q expanded to %d rows, want %d", step.Name, n, want)
		}
	}
}

// TestExpandThresholdInterposedStepIsPending is §11.2's interposed gate: a step
// reachable only by a threshold step-name routing IS created, in status
// `pending`, and simply never becomes ready unless routed to.
// TestInterposedStepsClassifiesByTargetNotReachability is DKT-38's
// classification half: V8 forces every non-first step to declare `after`, so a
// real-world interposed gate declares `after = [routing-step]` — the canonical
// shape — and an `after`-reachability test would classify every such gate as
// ordinary. The test is BEING NAMED AS A ROUTING TARGET, nothing else.
func TestInterposedStepsClassifiesByTargetNotReachability(t *testing.T) {
	def := &Definition{
		Pipeline: Pipeline{Name: "interposed", Version: 1},
		Steps: []*Step{
			{Name: "verify", Executor: "verify", Emits: "report", After: []string{},
				Threshold: map[string]string{"tribunal": "any(status == blocked)"}},
			// The corpus shape: reached by `after`, AND a threshold target.
			{Name: "tribunal", Type: TypeVote, OnFail: OnFailWaitingHuman,
				After: []string{"verify"}},
			{Name: "finish", Executor: "finish", Emits: "record",
				After: []string{"tribunal"}},
		},
	}

	if got := InterposedSteps(def); len(got) != 1 || got[0] != "tribunal" {
		t.Errorf("InterposedSteps() = %v, want [tribunal] — an after-declaring "+
			"target is still an interposed gate", got)
	}
	if got := RoutingPredecessors(def, "tribunal"); len(got) != 1 || got[0] != "verify" {
		t.Errorf("RoutingPredecessors(tribunal) = %v, want [verify]", got)
	}
	if got := RoutingPredecessors(def, "finish"); got != nil {
		t.Errorf("RoutingPredecessors(finish) = %v, want none — ordinary steps "+
			"are invisible to the latch", got)
	}
	targets := ThresholdTargets(map[string]string{
		"fix-loop": "any(a == b)", "pass": "any(a == b)",
		"tribunal": "any(a == b)", "audit": "any(a == b)",
	})
	if len(targets) != 2 || targets[0] != "audit" || targets[1] != "tribunal" {
		t.Errorf("ThresholdTargets() = %v, want [audit tribunal] — the closed "+
			"vocabulary excluded, the rest sorted", targets)
	}
}

func TestExpandThresholdInterposedStepIsPending(t *testing.T) {
	def := &Definition{
		Pipeline: Pipeline{Name: "interposed", Version: 1},
		Steps: []*Step{
			{Name: "measure", Executor: "measure", Emits: "report", After: []string{},
				Threshold: map[string]string{"escalate": "any(status == bad)"}},
			// Declared, never named by any `after`: reached ONLY by the
			// routing above. L2 exempts it from the orphan lint for exactly
			// this reason.
			{Name: "escalate", Type: TypeHuman, OnFail: OnFailSkip},
			{Name: "finish", Executor: "finish", Emits: "done", After: []string{"measure"}},
		},
	}

	interposed := InterposedSteps(def)
	if len(interposed) != 1 || interposed[0] != "escalate" {
		t.Fatalf("InterposedSteps() = %v, want [escalate]", interposed)
	}

	rows := Expand(def, Subject{Kind: "task"}, 0)

	var found bool
	for _, r := range rows {
		if r.Name != "escalate" {
			continue
		}
		found = true
		if r.Status != StatusPending {
			t.Errorf("interposed step status = %q, want %q", r.Status, StatusPending)
		}
		if r.Kind != TypeHuman {
			t.Errorf("interposed step kind = %q, want %q", r.Kind, TypeHuman)
		}
	}
	if !found {
		t.Error("the threshold-interposed step was not created; §11.2 requires it " +
			"created `pending` so a routing has something to interpose")
	}
}

// TestExpandStepKinds pins the `kind` column's vocabulary against the four
// §11.1 alternatives. A fanout step's siblings are `executor` rows: `fanout`
// says how many, not what kind.
func TestExpandStepKinds(t *testing.T) {
	def := &Definition{
		Pipeline: Pipeline{Name: "kinds", Version: 1},
		Steps: []*Step{
			{Name: "work", Executor: "worker", Emits: "a", After: []string{}},
			{Name: "compute", Action: "aggregate", After: []string{"work"},
				Params: map[string]any{"output": "b"}},
			{Name: "gate", Type: TypeHuman, OnFail: OnFailSkip, After: []string{"compute"}},
			{Name: "poll", Type: TypeVote, After: []string{"gate"},
				Voters: []string{"a"}, VoteRule: "simple"},
			{Name: "spread", Fanout: []string{"x", "y"}, Emits: "c", After: []string{"poll"}},
		},
	}

	want := map[string]string{
		"work": ClassExecutor, "compute": ClassAction,
		"gate": TypeHuman, "poll": TypeVote, "spread": ClassExecutor,
	}

	for _, r := range Expand(def, Subject{Kind: "task"}, 0) {
		if got := r.Kind; got != want[r.Name] {
			t.Errorf("step %q kind = %q, want %q", r.Name, got, want[r.Name])
		}
	}
}

// TestExpandClassDefaultsToExecutor is V23's rule applied at expansion: `class`
// defaults to the `executor` value. The default is applied HERE rather than at
// parse, so `parsed` stores what the author wrote and a later change to the
// defaulting rule cannot silently re-interpret a pinned definition.
func TestExpandClassDefaultsToExecutor(t *testing.T) {
	def := &Definition{
		Pipeline: Pipeline{Name: "classes", Version: 1},
		Steps: []*Step{
			{Name: "defaulted", Executor: "writer", Emits: "a", After: []string{}},
			{Name: "explicit", Executor: "writer", Class: "write", Emits: "b",
				After: []string{"defaulted"}},
			{Name: "fanned", Fanout: []string{"p", "q"}, Emits: "c",
				After: []string{"explicit"}},
			{Name: "fannedExplicit", Fanout: []string{"p", "q"}, Class: "read",
				Emits: "d", After: []string{"fanned"}},
		},
	}

	classes := make(map[string][]string)
	for _, r := range Expand(def, Subject{Kind: "task"}, 0) {
		classes[r.Name] = append(classes[r.Name], r.Class)
	}

	if got := classes["defaulted"]; len(got) != 1 || got[0] != "writer" {
		t.Errorf("defaulted class = %v, want [writer]", got)
	}
	if got := classes["explicit"]; len(got) != 1 || got[0] != "write" {
		t.Errorf("explicit class = %v, want [write]", got)
	}
	// A fanout sibling with no declared class accounts against its own hint:
	// two siblings running different hints are not one concurrency bucket.
	if got := classes["fanned"]; len(got) != 2 || got[0] != "p" || got[1] != "q" {
		t.Errorf("fanned classes = %v, want [p q]", got)
	}
	// An explicit class applies to every sibling, which is how a fanout is
	// held to one bucket on purpose.
	if got := classes["fannedExplicit"]; len(got) != 2 || got[0] != "read" || got[1] != "read" {
		t.Errorf("fannedExplicit classes = %v, want [read read]", got)
	}
}

// TestRenderAndParseInstanceRoundTrip pins §11.3's identity format in both
// directions. The rendered form is the step's PUBLIC identity — it appears in
// wire shapes, events, and error strings — so a reader that re-derived the
// format by hand is how the two drift.
func TestRenderAndParseInstanceRoundTrip(t *testing.T) {
	two := 2
	cases := []struct {
		name     string
		ordinal  int
		sibling  *int
		rendered string
	}{
		{"implement", 0, nil, "implement@0"},
		{"review", 0, &two, "review@0#2"},
		{"fix", 3, nil, "fix@3"},
		{"review", 12, &two, "review@12#2"},
	}

	for _, tc := range cases {
		t.Run(tc.rendered, func(t *testing.T) {
			if got := RenderInstance(tc.name, tc.ordinal, tc.sibling); got != tc.rendered {
				t.Fatalf("RenderInstance = %q, want %q", got, tc.rendered)
			}

			name, ordinal, sibling, err := ParseInstance(tc.rendered)
			testsupport.Must(t, err, "ParseInstance(%q): %v", tc.rendered, err)
			if name != tc.name || ordinal != tc.ordinal {
				t.Errorf("ParseInstance = (%q, %d), want (%q, %d)",
					name, ordinal, tc.name, tc.ordinal)
			}
			switch {
			case tc.sibling == nil && sibling != nil:
				t.Errorf("ParseInstance returned sibling %d, want none", *sibling)
			case tc.sibling != nil && sibling == nil:
				t.Errorf("ParseInstance returned no sibling, want %d", *tc.sibling)
			case tc.sibling != nil && *sibling != *tc.sibling:
				t.Errorf("ParseInstance sibling = %d, want %d", *sibling, *tc.sibling)
			}
		})
	}
}

// TestArtifactKindPerStepClass asserts TDD §4.3.1 row by row. Getting this
// wrong rejects the canonical fixture: `fix` declares
// `inputs = ["reconcile.findings", ...]` and `reconcile` is an ACTION step with
// no `emits` — it names its kind in `params.output`.
func TestArtifactKindPerStepClass(t *testing.T) {
	cases := []struct {
		name string
		step *Step
		want string
	}{
		{"executor emits", &Step{Executor: "e", Emits: "change-summary"}, "change-summary"},
		{"action params.output",
			&Step{Action: "aggregate", Params: map[string]any{"output": "findings"}},
			"findings"},
		{"action without params", &Step{Action: "aggregate"}, ""},
		{"fanout emits", &Step{Fanout: []string{"a"}, Emits: "findings"}, "findings"},
		{"human produces nothing", &Step{Type: TypeHuman}, ""},
		{"vote produces nothing", &Step{Type: TypeVote}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArtifactKind(tc.step); got != tc.want {
				t.Errorf("ArtifactKind = %q, want %q", got, tc.want)
			}
		})
	}

	// The fixture's own case, which is the one that found this rule.
	def := mustParseFixture(t)
	if got := ArtifactKind(StepByName(def, "reconcile")); got != "findings" {
		t.Errorf("fixture `reconcile` artifact kind = %q, want \"findings\" from params.output", got)
	}
}
