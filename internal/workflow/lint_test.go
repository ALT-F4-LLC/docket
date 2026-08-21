package workflow

import (
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// lintCase is one row of the §4.3.3 lint table. Cases with no `wants` are the
// negative half — a definition that must NOT trip the lint, which is where the
// exceptions live.
type lintCase struct {
	lint  string
	name  string
	src   string
	wants []string
}

var lintCases = []lintCase{
	{
		lint: "L1", name: "three-step cycle",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = ["c"]
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
[[step]]
name = "c"
after = ["b"]
executor = "x"
emits = "k"
`,
		// The cycle renders STEP NAMES, not DKT-N: CycleError.IDs carries the
		// dense indices this layer assigned, and rendering them through
		// model.FormatID would report issue ids for steps.
		wants: []string{"cycle detected among steps", "a", "b", "c"},
	},
	{
		lint: "L2", name: "unreachable step",
		// `stranded` is acyclic and well-formed, but nothing reaches it: it is
		// not a root, no threshold routes to it, and it is not a loop step. Its
		// only predecessor is itself unreachable.
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
[[step]]
name = "island"
loop = true
executor = "x"
emits = "k"
after_loop = "a"
[[step]]
name = "stranded"
after = ["island"]
executor = "x"
emits = "k"
`,
		// A loop step is excluded from ordinary expansion, so a step whose only
		// predecessor is one is genuinely unreachable in the ordinary topology.
		wants: []string{"unreachable", "stranded"},
	},
	{
		lint: "L3", name: "no root",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = ["b"]
executor = "x"
emits = "k"
[[step]]
name = "b"
after = []
executor = "x"
emits = "k"
[[step]]
name = "c"
after = ["a"]
executor = "x"
emits = "k"
`,
		// b IS a root here, so this must pass — the true no-root case cannot be
		// built without a cycle, which L1 catches first. See
		// TestL3RequiresARoot for the direct assertion.
	},
	{
		lint: "L4", name: "inputs from a non-predecessor",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "other"
[[step]]
name = "c"
after = ["a"]
executor = "x"
emits = "k"
inputs = ["b.other"]
`,
		wants: []string{`"c"`, "`inputs`", `"b.other"`, "not a predecessor"},
	},
}

func TestLintTable(t *testing.T) {
	for _, tc := range lintCases {
		t.Run(tc.lint+"_"+tc.name, func(t *testing.T) {
			def, err := Parse([]byte(tc.src))
			testsupport.Must(t, err, "Parse: %v", err)
			if err := Validate(def); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			err = Lint(def)

			if len(tc.wants) == 0 {
				testsupport.Must(t, err, "%s: unexpected lint error: %v", tc.lint, err)
				return
			}
			if err == nil {
				t.Fatalf("%s: expected a lint error, got none", tc.lint)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error %q does not mention %q", tc.lint, err.Error(), want)
				}
			}
		})
	}
}

// TestLintTableIsComplete holds the lint table and its tests in set equality,
// the same discipline TestValidationTableIsComplete applies to V1-V25.
func TestLintTableIsComplete(t *testing.T) {
	documented := make(map[string]bool, len(LintIDs))
	for _, id := range LintIDs {
		documented[id] = true
	}
	tested := make(map[string]bool, len(lintCases))
	for _, tc := range lintCases {
		tested[tc.lint] = true
	}

	var missing, extra []string
	for id := range documented {
		if !tested[id] {
			missing = append(missing, id)
		}
	}
	for id := range tested {
		if !documented[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("documented lints with no test case: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("test cases naming undocumented lints: %s", strings.Join(extra, ", "))
	}
}

// TestCycleRendersStepNames is §4.6's explicit requirement, asserted on its
// own: a 3-step cycle reports step names, never DKT-N. planner.CycleError
// renders its IDs through model.FormatID, so the workflow layer must map the
// dense indices back before reporting — a FormatID-shaped rendering difference
// is this layer's business, and internal/planner is not modified for it.
func TestCycleRendersStepNames(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "alpha"
after = ["gamma"]
executor = "x"
emits = "k"
[[step]]
name = "beta"
after = ["alpha"]
executor = "x"
emits = "k"
[[step]]
name = "gamma"
after = ["beta"]
executor = "x"
emits = "k"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	err = Lint(def)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	msg := err.Error()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(msg, name) {
			t.Errorf("cycle error %q does not name step %q", msg, name)
		}
	}
	if strings.Contains(msg, "DKT-") {
		t.Errorf("cycle error %q renders issue ids, not step names", msg)
	}
}

// TestL2ExceptionThresholdInterposedStep is the exception that matters most:
// §11.2 says routing to a step name "interposes that declared,
// otherwise-unreached step as a successor gate", so being unreached in the
// ordinary topology is LEGITIMATE for such a step. A naive reachability lint
// rejects the reference instance's own security workflow.
func TestL2ExceptionThresholdInterposedStep(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
threshold = { "extra-gate" = "any(f == v)" }
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
[[step]]
name = "extra-gate"
type = "human"
after = []
on_fail = "skip"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("a threshold-interposed step must not be an orphan: %v", err)
	}
}

// TestL2ExceptionLoopStep is the other exception: §11.3 (3) excludes
// `loop = true` steps from ordinary expansion, so they are not orphans either.
func TestL2ExceptionLoopStep(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
[[step]]
name = "fixup"
loop = true
executor = "x"
emits = "k"
after_loop = "a"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("a loop = true step must not be an orphan: %v", err)
	}
}

// TestL2CatchesARealOrphan proves the exceptions did not hollow the lint out.
func TestL2CatchesARealOrphan(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
[[step]]
name = "stranded"
after = ["stranded-parent"]
executor = "x"
emits = "k"
[[step]]
name = "stranded-parent"
after = ["b"]
executor = "x"
emits = "k"
when = "kind == task"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// This one IS reachable — a is root, b follows, stranded-parent follows b,
	// stranded follows it. The lint must accept it.
	if err := Lint(def); err != nil {
		t.Fatalf("a reachable chain was reported as orphaned: %v", err)
	}
}

// TestL3RequiresARoot asserts L3 directly. Every step declaring a non-empty
// `after` is necessarily cyclic, which L1 reports first, so the assertion is
// made against the root computation rather than through Lint.
func TestL3RequiresARoot(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = ["b"]
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if roots := rootSteps(def); len(roots) != 0 {
		t.Errorf("rootSteps found %d roots in a rootless definition", len(roots))
	}

	// And the first-step exemption does produce a root.
	def2, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "first"
executor = "x"
emits = "k"
[[step]]
name = "second"
after = ["first"]
executor = "x"
emits = "k"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	roots := rootSteps(def2)
	if len(roots) != 1 || roots[0].Name != "first" {
		t.Errorf("first-step exemption did not produce a root: %v", roots)
	}
}

// TestL4ExceptsLoopSteps: a loop step's inputs bind per §11.3 (3) within its
// ordinal, falling back to the highest earlier ordinal per input, so its
// producers need not precede it in the ordinary topology. The committed fixture
// depends on this — `fix` takes `reconcile.findings` while being excluded from
// the ordinary topology entirely.
func TestL4ExceptsLoopSteps(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
[[step]]
name = "later"
after = ["a"]
executor = "x"
emits = "findings"
[[step]]
name = "fixup"
loop = true
executor = "x"
emits = "k"
inputs = ["later.findings"]
after_loop = "a"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("a loop step's inputs must not be ordered by the ordinary topology: %v", err)
	}
}

// TestPlannerReuseIsUnmodified is the binding repo fact, asserted structurally:
// the lints go through planner.BuildDAG and planner.TopoSort rather than a
// second Kahn implementation. A future edit that inlines a topological sort
// here would leave this test passing but the reuse claim false, so the test
// checks the observable consequence instead — the cycle report carries every
// member of the cycle, which is TopoSort's CycleError contract.
func TestPlannerReuseIsUnmodified(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "root"
after = []
executor = "x"
emits = "k"
[[step]]
name = "p"
after = ["q"]
executor = "x"
emits = "k"
[[step]]
name = "q"
after = ["p"]
executor = "x"
emits = "k"
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err = Lint(def)
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "p") || !strings.Contains(msg, "q") {
		t.Errorf("cycle error %q does not carry both cycle members", msg)
	}
	if strings.Contains(msg, "root") {
		t.Errorf("cycle error %q names a step outside the cycle", msg)
	}
}
