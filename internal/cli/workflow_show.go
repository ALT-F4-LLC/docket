package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

var workflowShowCmd = &cobra.Command{
	Use:   "show <name>[@<version>]",
	Short: "Show a registered workflow definition",
	Long: `Show a registered workflow.

Omitting @version selects the highest registered version that is NOT
deprecated — the same version a new run would bind. Deprecated versions are
skipped; name them explicitly (name@version) to show one. A name whose every
version is deprecated is reported as not found. --source emits the stored TOML
verbatim, which is the exact bytes that were registered and hashed.

The summary reports a SOURCE STATUS: whether the file at the recorded
source_path still hashes to source_sha256 (matches), holds different bytes
(drifted), cannot be read at all (unreadable), or names nothing this invocation
can resolve — no path recorded, or a relative one (unchecked). Drift is reported,
never repaired: a registered name@version is frozen, and registering an edited
file is the install path's job.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowShow(cmd, args, getWriter(cmd))
	},
}

func runWorkflowShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	name, version, err := model.ParseWorkflowRef(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	wf, err := db.GetWorkflow(conn, getProjectID(cmd), name, version)
	if err != nil {
		return workflowErr(describeMissingWorkflow(err, args[0]))
	}

	source, _ := cmd.Flags().GetBool("source")
	if source {
		// The stored TOML, verbatim. In JSON mode it rides in the envelope as
		// a string so the output stays a single parseable document; in human
		// mode it is printed as-is, which is what makes
		// `docket workflow show X --source > X.toml` round-trip.
		if w.JSONMode {
			w.Success(map[string]string{"name": wf.Name, "source": wf.Body}, "")
			return nil
		}
		w.Success(nil, strings.TrimRight(wf.Body, "\n"))
		return nil
	}

	// DKT-590: the two provenance columns, checked against each other instead
	// of merely printed. `source_path` and `source_sha256` were reported side
	// by side while the file at that path had been a different definition for
	// four versions, and nothing in this output said so. The verdict rides on
	// the row itself (populated nowhere else), so it reaches the JSON envelope
	// and the human summary from one place.
	wf.SourceStatus = engine.CheckWorkflowSource(wf.SourcePath, wf.SourceSHA256)

	var message string
	if !w.JSONMode {
		message = renderWorkflowShow(wf)
	}
	w.Success(wf, message)
	return nil
}

// describeMissingWorkflow turns the storage sentinel into an operator-facing
// message naming what was asked for.
func describeMissingWorkflow(err error, ref string) error {
	if err == db.ErrWorkflowNotFound {
		return fmt.Errorf("%w: %s is not registered", db.ErrWorkflowNotFound, ref)
	}
	return err
}

func renderWorkflowShow(wf *model.Workflow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", wf.Ref())
	if wf.Description != "" {
		fmt.Fprintf(&b, "%s\n", wf.Description)
	}
	fmt.Fprintf(&b, "sha256: %s\n", wf.SourceSHA256)
	if wf.SourcePath != "" {
		fmt.Fprintf(&b, "source: %s\n", wf.SourcePath)
	}
	// The verdict prints on its own line, ALWAYS when it was computed — a
	// clean match included. "Nothing was said" and "the bytes still match" are
	// different facts, and the whole defect this closes is a reader who could
	// not tell them apart (DKT-590).
	if wf.SourceStatus != nil {
		fmt.Fprintf(&b, "source status: %s\n", engine.DescribeWorkflowSource(wf.SourceStatus))
	}
	// A retired version renders as retired. Without this the summary of a
	// version that can no longer bind is indistinguishable from one that can.
	if wf.Deprecated() {
		fmt.Fprintf(&b, "status: DEPRECATED — retired from binding, still "+
			"readable and still resolved by runs that pinned it\n")
	}

	// The parsed form is what activation reads, so the human view renders it
	// rather than re-reading the TOML — what is shown is what would run.
	def, err := workflow.FromCanonical([]byte(wf.Parsed))
	if err != nil {
		return strings.TrimRight(b.String(), "\n")
	}

	// The `[match]` clause — the binding rule. It was absent from this summary
	// while the AC asking to "see what an old version actually froze" was
	// nominally satisfied by --source alone; a reader comparing two versions
	// cares most about what each one BINDS.
	if m := renderMatch(def.Match); m != "" {
		fmt.Fprintf(&b, "\nmatch:\n%s", m)
	}

	fmt.Fprintf(&b, "\nsteps (%d):\n", len(def.Steps))
	var total float64
	for _, step := range def.Steps {
		total += stepExpectedCost(step)
		fmt.Fprintf(&b, "  %-20s %s", step.Name, describeStepClass(step))
		if len(step.After) > 0 {
			fmt.Fprintf(&b, "  after=[%s]", strings.Join(step.After, ", "))
		}
		if step.Loop {
			b.WriteString("  loop")
		}
		fmt.Fprintf(&b, "  %s\n", describeStepCost(step))
		// Gates decide whether a step's work is accepted, so a definition
		// summary that omitted them would hide the checks it imposes.
		for _, gate := range step.Gates {
			fmt.Fprintf(&b, "      gate %s", gate.Name)
			if gate.Source != "" {
				fmt.Fprintf(&b, "  source=%s", gate.Source)
			}
			b.WriteString("\n")
		}
	}
	// The floor a plan's budget arithmetic is compared against. It is stated
	// here because it existed NOWHERE a reader could reach: the per-step values
	// lived only in the registered TOML, so budgeting a run meant re-summing a
	// file by hand (DKT-528).
	fmt.Fprintf(&b, "\nexpected_cost total: %.2f  "+
		"(fanout expanded; `when`-gated and `loop` steps included)\n", total)
	return strings.TrimRight(b.String(), "\n")
}

// stepSiblings is how many expanded rows one [[step]] produces — one per
// `fanout` hint, or one when it does not fan out (internal/workflow/expand.go).
func stepSiblings(step *workflow.Step) int {
	if len(step.Fanout) == 0 {
		return 1
	}
	return len(step.Fanout)
}

// stepExpectedCost is what one [[step]] contributes to the workflow's
// expected-cost floor, WITH FANOUT EXPANDED.
//
// `expected_cost` is PER EXPANDED SIBLING: expansion copies the declared value
// onto every sibling row, and each sibling claims separately, so the budget
// floor accrues it once per sibling (internal/engine/budget.go §4.3 sums the
// per-claim events). A four-hint step declaring 0.60 therefore contributes 2.40.
func stepExpectedCost(step *workflow.Step) float64 {
	if step.ExpectedCost == nil {
		return 0
	}
	return *step.ExpectedCost * float64(stepSiblings(step))
}

// describeStepCost renders a step's declared cost. Every step carries the
// column — a step that declares nothing renders `cost=-` rather than being
// silently omitted, so a reader can tell "declares no cost" from "the summary
// does not show costs".
//
// A declared `expected_cost = 0` renders as `cost=-` too: registration
// materializes the §11.1 default into the parsed form (applyDefaults), so an
// absent field and an explicit zero are the same stored value by the time this
// reads it, and both contribute nothing to the floor.
func describeStepCost(step *workflow.Step) string {
	declared := 0.0
	if step.ExpectedCost != nil {
		declared = *step.ExpectedCost
	}
	if declared == 0 {
		return "cost=-"
	}

	out := fmt.Sprintf("cost=%.2f", declared)
	if n := stepSiblings(step); n > 1 {
		out = fmt.Sprintf("cost=%.2f x%d = %.2f", declared, n, declared*float64(n))
	}
	// A `loop` step is not expanded at ordinal 0 at all: it instantiates once
	// per loop entry (§11.3 (3)). The total counts one entry, which is the
	// floor — every additional fix round costs this much again.
	if step.Loop {
		out += " per loop entry"
	}
	return out
}

// renderMatch renders a `[match]` clause's four terms, omitting the absent
// ones. An entirely absent clause matches every issue, and says so.
func renderMatch(m *workflow.Match) string {
	if m == nil {
		return "  (absent — matches every issue)\n"
	}
	var b strings.Builder
	for _, term := range []struct {
		name   string
		values []string
	}{
		{"kind", m.Kind},
		{"labels_any", m.LabelsAny},
		{"labels_all", m.LabelsAll},
		{"unless_labels", m.UnlessLabels},
	} {
		if len(term.values) > 0 {
			fmt.Fprintf(&b, "  %-14s [%s]\n", term.name, strings.Join(term.values, ", "))
		}
	}
	if b.Len() == 0 {
		return "  (absent — matches every issue)\n"
	}
	return b.String()
}

// describeStepClass renders which of the four §11.1 alternatives a step
// declares, with its opaque hint. The hint is echoed, never interpreted.
func describeStepClass(step *workflow.Step) string {
	switch step.StepClass() {
	case workflow.ClassExecutor:
		return "executor=" + step.Executor
	case workflow.ClassAction:
		return "action=" + step.Action
	case workflow.ClassType:
		return "type=" + step.Type
	case workflow.ClassFanout:
		return fmt.Sprintf("fanout=[%s]", strings.Join(step.Fanout, ", "))
	default:
		return ""
	}
}

func init() {
	workflowShowCmd.Flags().Bool("source", false, "Emit the stored TOML verbatim")
	workflowCmd.AddCommand(workflowShowCmd)
}
