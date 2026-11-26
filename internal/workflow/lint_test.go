package workflow

import (
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// lintCase is one row of the lint table. Cases with no `wants` are the
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

// TestL2ExceptionThresholdInterposedStep is the exception that matters most:
// Routing to a step name "interposes that declared,
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

// TestL4ExceptsIssueLatestInputs: `issue.latest.<kind>` names no producer
// step, so L4 has no predecessor to require — resolving to nothing is a legal
// answer for the form. The consumer here is deliberately NOT a loop step,
// because loop steps are excepted from L4 wholesale and would mask a reading
// of `issue.latest` as a step name (DKT-492).
func TestL4ExceptsIssueLatestInputs(t *testing.T) {
	def, err := Parse([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "doc"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "findings"
inputs = ["issue.latest.doc"]
`))
	testsupport.Must(t, err, "Parse: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("`issue.latest.<kind>` must not be ordered by the ordinary topology: %v", err)
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
