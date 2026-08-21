package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// newRunActivateCmd builds a FRESH `run activate` command, flags included.
// Both the package's registered runActivateCmd and the test suite build
// through this one factory (import.go's newImportCmd is the precedent):
// registering the flags in exactly one place means a test that builds its
// own instance still carries the SAME flag set a real invocation parses, so
// a flag that stopped being registered fails the test with "unknown flag"
// rather than a hand-maintained stand-in silently testing nothing.
func newRunActivateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "activate RUN-N",
		Short: "Bind, pin, snapshot, and expand a run",
		Long: `Activate a run: one transaction, all or nothing.

  1. Bind      each issue to EXACTLY ONE registered workflow, by its [match]
               clause. Zero matches or several is a refusal naming the issue
               and every candidate.
  2. Lint      the work graph for cycles.
  3. Pin       each bound workflow at its registered content hash, plus every
               --pin file at its own. Pinning is never partial: one unreadable
               path refuses the whole activation.
  4. Snapshot  each issue's body, title, kind, labels, and scope. Steps read
               the snapshot, never the live issue, so an edit mid-run cannot
               change what work was already scheduled against.
  5. Harvest   the fenced command blocks whose tag a bound workflow declares,
               verbatim and hashed. Blocks with an undeclared tag are ignored.
  6. Expand    the first phase's steps — those whose issues have no unsatisfied
               dependencies. Later phases expand as their predecessors finish.
  7. Promote   the issues from backlog to todo, and the run to active.

Nothing executes. No gate, no action, no command runs during activation; files
are read only to pin them by content hash.

Instance config is scanned from EVERY configured root, in order. With the shared
store that is ~/.docket/config/ first, then this checkout's .docket/config/ if it
has one; with DOCKET_PATH or a repo-local store it is that store's config/ alone.
A root that does not exist is skipped, so a repository needs no .docket/ at all.
A workflow, schema, or pinned file offered by two roots with DIFFERENT bytes
refuses the activation and names both paths.

Re-activating an active run expands newly-unblocked phases only and INHERITS
the original pin set — a workflow re-registered or a pinned file edited since
activation does not reach a run already under way.

The JSON envelope carries the run's expected cost as TWO distinct fields:
` + "`expected_cost_total`" + ` is every step the run holds, including ones a
prior activation already created; ` + "`expected_cost_added`" + ` is only
what THIS call just added — equal to the total on a first activation, and
the newly-expanded phase's cost alone on a re-activation. A skipped step
still counts toward both sums. The human summary line prints the added
figure before the total, for the same reason: reading the total as the
increment is how a budget cap once got raised against the wrong number.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunActivate(cmd, args, getWriter(cmd))
		},
	}
	cmd.Flags().Bool("dry-run", false,
		"compute the activation, print what it would bind and invoke, and write nothing")
	cmd.Flags().StringSlice("pin", nil,
		"File to pin by content hash (repeatable); core never reads its meaning")
	cmd.Flags().String("reason", "",
		"Optional reason recorded on the run-activated event")
	return cmd
}

var runActivateCmd = newRunActivateCmd()

// activateResult is the verb's wire shape. It reports what the transaction did
// rather than echoing the run alone, so an operator can tell a first
// activation from a re-activation that expanded nothing.
type activateResult struct {
	Run            *model.Run `json:"run"`
	Reactivation   bool       `json:"reactivation,omitempty"`
	IssuesBound    int        `json:"issues_bound"`
	IssuesExpanded int        `json:"issues_expanded"`
	StepsCreated   int        `json:"steps_created"`
	// ExpectedCostAdded is the increment `expected_cost_total` moved BY THIS
	// ACTIVATION — the steps it just created, and none of the run's prior
	// ones — named distinctly from the total so a conductor reading the
	// envelope cannot mistake the whole roster for what this call added
	// (DKT-517: a conductor read `expected_cost_total=35.1` as a 5-step
	// increment whose real cost, this field, was 2.1, and a budget cap raise
	// was approved against the wrong number). Equal to `expected_cost_total`
	// on a first activation, since the run held no prior steps. A skipped
	// step still counts toward this sum, same as the total.
	ExpectedCostAdded float64 `json:"expected_cost_added"`
	// ExpectedCostTotal is the run-wide expected-cost sum as this activation
	// leaves (or, on --dry-run, would leave) it — the WHOLE roster, every
	// step the run holds including ones a prior activation created, not this
	// call's increment (see ExpectedCostAdded for that) — cap-vs-cost for an
	// activation gate, beside the step count (DKT-54).
	ExpectedCostTotal float64                 `json:"expected_cost_total"`
	PinsRecorded      int                     `json:"pins_recorded"`
	FencesHarvested   int                     `json:"fences_harvested"`
	ContextWarnings   []engine.ContextWarning `json:"context_warnings,omitempty"`
	// ScopeWarnings is the unscoped-holder lint's array: issues that declared no
	// scope while binding a workflow that holds the tree. `omitempty`, so a run
	// where every issue declared its scope carries no key at all rather than an
	// empty one.
	ScopeWarnings []engine.ScopeWarning `json:"scope_warnings,omitempty"`
	// Fences is §7.7 S2's array: the same data the human report renders,
	// carrying the RAW stored command bytes — encoding/json escapes controls
	// by contract and the consumer is a program, so quoting on top would
	// double-escape the value a machine reads (§5.7 E4).
	Fences []engine.FenceReport `json:"fences,omitempty"`
	// GatePreflight is DKT-255's array: every DECLARED gate and whether a trust
	// entry resolves it here. `omitempty`, so a run whose workflows declare no
	// gates carries no key rather than an empty one — the same dormancy-visible
	// stance Registered takes.
	GatePreflight []engine.GatePreflight `json:"gate_preflight,omitempty"`
	// HoldPolicy is DKT-266's disclosure: who answers a hold this run mints.
	// NOT omitempty — `"panel": false` is the answer a reader needs, and an
	// absent key would make "one operator decides" indistinguishable from a
	// docket too old to report it, which is the exact ambiguity this closes.
	HoldPolicy engine.HoldPolicy `json:"hold_policy"`
	// Registered is F21's array, ALONGSIDE `fences`: what auto-registration
	// acted on, as {kind, name, version, path, sha256, outcome}.
	//
	// F23: it is `omitempty`, so an activation that registered nothing carries
	// no key rather than an empty array — which is F17's dormancy visible in the
	// output, and the same "a field that is not a fact does not appear" rule the
	// report's optional sections follow.
	Registered []engine.Registration `json:"registered,omitempty"`
	// PinsFromConfig counts what the scan pinned rather than registered (F4).
	PinsFromConfig int `json:"pins_from_config,omitempty"`
	// PromotedIssues names every issue this activation moved backlog -> todo,
	// by display id (DKT-102/DKT-94: counts alone don't tell an operator
	// approving a `--dry-run` WHICH issues would move). Carried on both a
	// dry run and a real activation — the same list, since promotion is
	// computed identically on both.
	PromotedIssues []string `json:"promoted_issues,omitempty"`
	// BoundIssues is DKT-94's roster: every issue this activation bound, paired
	// with the exact workflow@version, so a `--dry-run` (or a real activation)
	// answers WHAT was bound rather than leaving `issues_bound` as a bare count.
	BoundIssues []engine.BoundIssue `json:"bound_issues,omitempty"`
	// DryRun marks a computed-and-discarded activation, so a consumer cannot
	// mistake it for a real one.
	DryRun bool `json:"dry_run,omitempty"`
	// ProjectedStatus and ProjectedActivatedAtMS are set ONLY when DryRun is
	// true (DKT-96/DKT-100/DKT-109): what `run.status`/`run.activated_at_ms`
	// would become if this activation committed. `run` itself always renders
	// the row as it actually is on disk — never the rolled-back mutation — so
	// a dry run's JSON can never be mistaken, field-for-field, for a real
	// activation's.
	ProjectedStatus        string `json:"projected_status,omitempty"`
	ProjectedActivatedAtMS *int64 `json:"projected_activated_at_ms,omitempty"`
	// Reason is the operator-supplied `--reason`, echoed back so both the
	// JSON envelope and the human summary line let an operator confirm what
	// was recorded rather than trusting an unechoed flag value (matching
	// `run pause`/`run resume`/`run abandon`'s human-mode echo).
	Reason string `json:"reason,omitempty"`
}

func runRunActivate(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	pins, _ := cmd.Flags().GetStringSlice("pin")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	reason, _ := cmd.Flags().GetString("reason")

	result, err := engine.Activate(conn, runID, engine.ActivateOptions{
		FilePins: pins,
		NowMS:    model.NowMS(),
		DryRun:   dryRun,
		Reason:   reason,
	})
	if err != nil {
		return runErr(err)
	}

	// The WARN cap reports and proceeds; only the ERROR cap refuses. Warn
	// writes to stderr in human mode and is a no-op in JSON mode, where the
	// envelope on stdout is the sole channel — so the same warnings ride out
	// in `context_warnings` below rather than being lost.
	for _, warning := range result.ContextWarnings {
		w.Warn("step %s on %s carries a %d-byte context, over the %d-byte "+
			"warning threshold (context.warn_bytes)",
			warning.Instance, warning.IssueID,
			warning.Bytes, warning.Cap)
	}

	// The unscoped-holder lint, on the same channel and for the same reason. It
	// names the remedy as well as the fact: an operator reading "declares no
	// scope" needs to know that scope is set on the ISSUE, not in the workflow
	// that happens to be named beside it.
	for _, warning := range result.ScopeWarnings {
		w.Warn("%s is bound to %s and %s; declare one with "+
			"`docket issue edit %s --scope GLOB`",
			warning.IssueID, warning.Workflow, warning.Reason,
			warning.IssueID)
	}

	// §7.7 S1: every harvested fenced command, verbatim, with its trust status
	// — so an operator sees `unmatched` commands BEFORE the run rather than
	// after. It renders through the escaping renderer (T18): the bytes are
	// content an issue author wrote, and a command carrying terminal escapes
	// must show up as escapes rather than repaint the line being approved.
	// F20: the `Registered` block goes ABOVE the fences, because that is the
	// order an operator reads them in — what this activation adopted, then what
	// it will invoke. Both are on stderr in human mode for the same reason: they
	// are a report about the activation, not its result.
	if !w.JSONMode {
		engine.RenderRegistrationReport(os.Stderr, result.Registered, result.PinsFromConfig)
		engine.RenderFenceReport(os.Stderr, result.Fences)
		// After the fence report, because a missing ENTRY is the thing an
		// operator acts on and the last block printed is the one they read.
		engine.RenderGatePreflight(os.Stderr, result.GatePreflight)
		// Who answers a hold this run mints (DKT-266). Silent unless something
		// is configured — see RenderHoldPolicy for why the default earns no
		// line and a half-configured pair earns a warning.
		engine.RenderHoldPolicy(os.Stderr, result.HoldPolicy)
	}

	payload := activateResult{
		Run:               result.Run,
		Reactivation:      result.Reactivation,
		IssuesBound:       result.IssuesBound,
		IssuesExpanded:    result.IssuesExpanded,
		StepsCreated:      result.StepsCreated,
		ExpectedCostAdded: result.ExpectedCostAdded,
		ExpectedCostTotal: result.ExpectedCostTotal,
		PinsRecorded:      result.PinsRecorded,
		FencesHarvested:   result.FencesHarvested,
		ContextWarnings:   result.ContextWarnings,
		ScopeWarnings:     result.ScopeWarnings,
		Fences:            result.Fences,
		GatePreflight:     result.GatePreflight,
		HoldPolicy:        result.HoldPolicy,
		Registered:        result.Registered,
		PinsFromConfig:    result.PinsFromConfig,
		DryRun:            result.DryRun,
		PromotedIssues:    result.PromotedIssues,
		BoundIssues:       result.BoundIssues,
		Reason:            reason,
	}
	if result.DryRun {
		payload.ProjectedStatus = string(result.ProjectedStatus)
		payload.ProjectedActivatedAtMS = result.ProjectedActivatedAtMS
	}

	w.Success(payload, renderActivation(payload))
	return nil
}

func renderActivation(r activateResult) string {
	verb := "Activated"
	if r.Reactivation {
		verb = "Re-activated"
	}
	// A dry run says so. Reporting "Activated" for a computation that was
	// discarded is the one way this verb could actively mislead: an operator
	// who reads it as done would never run the real thing.
	if r.DryRun {
		verb = "Dry run of"
	}

	var parts []string
	if r.IssuesBound > 0 {
		part := fmt.Sprintf("%d issue(s) bound", r.IssuesBound)
		if len(r.BoundIssues) > 0 {
			roster := make([]string, 0, len(r.BoundIssues))
			for _, b := range r.BoundIssues {
				roster = append(roster, fmt.Sprintf("%s->%s", b.IssueID, b.Workflow))
			}
			part += fmt.Sprintf(" (%s)", strings.Join(roster, ", "))
		}
		parts = append(parts, part)
	}
	parts = append(parts,
		fmt.Sprintf("%d issue(s) expanded", r.IssuesExpanded),
		fmt.Sprintf("%d step(s)", r.StepsCreated),
		// The increment BEFORE the total (DKT-517): the total alone read as
		// "what this activation cost" when it was in fact the whole run's
		// roster, and that misreading is what got a budget cap raised
		// against the wrong number.
		fmt.Sprintf("%.2f expected cost added", r.ExpectedCostAdded),
		fmt.Sprintf("%.2f expected cost total", r.ExpectedCostTotal))
	if r.PinsRecorded > 0 {
		parts = append(parts, fmt.Sprintf("%d pin(s)", r.PinsRecorded))
	}
	if r.FencesHarvested > 0 {
		parts = append(parts, fmt.Sprintf("%d fenced command(s)", r.FencesHarvested))
	}
	if len(r.PromotedIssues) > 0 {
		parts = append(parts, fmt.Sprintf("promoted %s",
			strings.Join(r.PromotedIssues, ", ")))
	}

	msg := fmt.Sprintf("%s %s: %s", verb, r.Run.Ref(), strings.Join(parts, ", "))
	// A re-activation must SAY what it inherited (DKT-85): counts alone read
	// as fresh binding-and-pinning work, which is exactly how a conductor once
	// mistook a no-op against a stale picture for a real activation.
	if r.Reactivation && !r.DryRun {
		msg += " (re-activation: original pin set inherited, nothing re-registered)"
	}
	if r.Reason != "" {
		msg += fmt.Sprintf(" (reason: %s)", r.Reason)
	}
	// DKT-96/DKT-100/DKT-109: a dry run's `r.Run` renders the run as it
	// ACTUALLY is (status still `planning`, or `active` unchanged on a
	// re-activation) — say what committing would change, so the human
	// rendering carries the same signal the JSON's `projected_status` does.
	if r.DryRun {
		if r.ProjectedStatus != "" && r.ProjectedStatus != string(r.Run.Status) {
			msg += fmt.Sprintf(" (would move %s: %s -> %s)",
				r.Run.Ref(), r.Run.Status, r.ProjectedStatus)
		} else {
			msg += fmt.Sprintf(" (%s stays %s; nothing would change)",
				r.Run.Ref(), r.Run.Status)
		}
	}
	return msg
}

func init() {
	runCmd.AddCommand(runActivateCmd)
}
