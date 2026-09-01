package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// `docket dispatch open|close|verify|abandon` — engine-spec §1's surface line,
// engine-spec §2's scheduling block (docs/tdd/runs-dispatch.md §5).
//
// A dispatch is a FROZEN COPY OF ONE `next` ANSWER. It is not a lock: the steps
// in it stay `pending` and any claimant may still claim them. What it buys is
// that the engine can refuse to offer a NEW batch while the previous one is
// unreconciled — which turns a relay that lost track of its own spawns from a
// silent double-executor into a stalled run with a reason.

var dispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Record and reconcile batch manifests for a run",
	Long: `Batch dispatchers open a manifest, spawn against it, and reconcile it.

A dispatch is a frozen copy of one ` + "`next`" + ` answer, recorded so the spawns a
relay actually made can be checked against what the engine actually offered.

It is NOT a lock and NOT a claim. The steps in a manifest are still pending, and
any claimant may still claim them — a dispatcher is the thing that STARTS
executors, not an executor, so claiming on its behalf would mint a token nobody
holds.

While a dispatch is open, ` + "`next --run`" + ` REFUSES rather than returning an empty
list: relay drift stalls loudly instead of proceeding around its own mess.

RECOVERY IS DESIGNED IN. Exactly one dispatch is open per run, enforced by the
database. A manifest carries a TTL that ` + "`next`" + ` auto-abandons once it lapses,
and ` + "`dispatch abandon`" + ` retires one unconditionally — so a relay that crashed
mid-batch can never wedge a run.`,
}

// ---- open ------------------------------------------------------------------

var dispatchOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Record a batch manifest of the run's ready steps",
	Long: `Compute the ready set exactly as ` + "`next --run`" + ` does and record it.

The manifest is the ordered rows, stored as their canonical JSON bytes plus a
sha256 — so ` + "`dispatch verify`" + ` compares byte for byte rather than by a
re-serialization that could differ in key order.

Opening while one is already open is a CONFLICT naming the open dispatch and its
expiry. That refusal comes from a unique index rather than a check-then-insert,
so two relays racing produce one manifest and one refusal, never two manifests.

--ack-reap acknowledges a write-class reap by the seq of its ` + "`lease-reaped`" + `
event. It is the entry point a NEW relay uses when taking over from a crashed
one: core knows the database lease lapsed but cannot check a process it did not
start, so it holds the class's headroom until somebody confirms the writer is
gone.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchOpen(cmd, getWriter(cmd))
	},
}

func runDispatchOpen(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}
	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}
	acks, err := ackSeqs(cmd)
	if err != nil {
		return err
	}

	manifest, err := engine.NewEngine().OpenDispatch(conn, runID, limit, acks, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	// DKT-193: stale targets warn on stderr for a human AND ride the JSON
	// envelope (`stale_targets`), the two channels every advisory here uses —
	// w.Warn is suppressed in JSON mode by design.
	warnStaleTargets(w, manifest.StaleTargets)

	// DKT-408: pin drift takes the same two channels, at the same moment, for
	// the same reader — the conductor deciding whether to spend a wave on
	// these rows. Every step reading a drifted ref will refuse at render;
	// warning here is what lets that be a decision instead of a discovery.
	warnPinDrift(w, manifest.PinDrift, manifest.Run)

	// DKT-242: a manifest short of the rows a reader can see are ready must
	// say WHY, or the shortfall reads as a graph that has run dry — the one
	// conclusion that makes a dispatcher stop asking. Both channels again: the
	// stderr line for a human, `budget_held` in the envelope for a relay.
	if manifest.BudgetHeld != "" {
		w.Warn("%s", manifest.BudgetHeld)
	}

	var message string
	if !w.JSONMode {
		message = renderManifest(manifest)
	}
	w.Success(manifest, message)
	return nil
}

// warnStaleTargets renders DKT-193's advisory, one line per affected row.
//
// It PRINTS THE ENGINE'S REASON rather than paraphrasing it (DKT-415). This line
// was a second wording of the same fact, so the claim-time semantics the reason
// now names — the half a conductor deciding whether to dispatch through the
// warning actually needs — would have reached the JSON reader and not the human
// one. One string, two channels.
func warnStaleTargets(w *output.Writer, stale []engine.StaleTarget) {
	for _, s := range stale {
		w.Warn("%s (%s): %s", s.Instance, s.Issue, s.Reason)
	}
}

// warnUnclaimedTargets renders DKT-993's advisory, one line per back-filled
// step no worker ever claimed.
//
// The rows ARE recorded — the engine's contract is warn-and-record, not refuse
// — so the line says so explicitly. A conductor that reads "recorded" and meant
// it can move on; one that did not now has the step id its journal mis-joined
// onto, which is the whole gap RUN-66 left: the bad rows landed in silence and
// surfaced only as unexplained spend in `run report`.
func warnUnclaimedTargets(w *output.Writer, unclaimed []engine.UnclaimedTarget) {
	for _, u := range unclaimed {
		w.Warn("%s (%s) is %s and was never claimed — no worker ever held it, "+
			"so it reported nothing to reconstruct. The row is recorded; check "+
			"the journal that produced it before trusting the spend",
			u.Step, exec.Render(u.Instance), u.Status)
	}
}

// warnPinDrift renders DKT-408's advisory, one line per unsound pin, plus the
// recovery verb — steps reading these refs will refuse at claim/render, and
// the conductor should learn that before spending a wave, not from it.
func warnPinDrift(w *output.Writer, drift []engine.PinVerdict, runRef string) {
	for _, v := range drift {
		switch v.Status {
		case engine.PinChanged:
			w.Warn("%s %s changed since %s pinned it (pinned %.12s, on disk %.12s) — "+
				"steps reading it will refuse at render",
				v.Kind, v.Ref, runRef, v.Pinned, v.Found)
		case engine.PinMissing:
			w.Warn("%s %s is pinned by %s but no longer resolves — steps reading it "+
				"will refuse", v.Kind, v.Ref, runRef)
		}
	}
	if len(drift) > 0 {
		w.Warn("restore the file(s), or `docket run repin %s --reason ...` to adopt "+
			"current disk bytes for the remaining steps", runRef)
	}
}

// renderManifest is the human view of §11.4's `dispatch` shape.
//
// It reuses render.RenderStepRows for the rows themselves, so a manifest and the
// `next` answer it froze LOOK the same as well as being byte-identical — an
// operator comparing the two by eye should not have to translate between two
// table layouts.
func renderManifest(m *engine.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s opened for %s at seq %d, expiring at %d\n\n",
		m.Dispatch, m.Run, m.OpenedSeq, m.ExpiresMS)
	b.WriteString(render.RenderStepRows(m.Rows))
	return strings.TrimRight(b.String(), "\n")
}

// ---- verify ----------------------------------------------------------------

var dispatchVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Compare the open manifest to the run's current ready set",
	Long: `Recompute the ready set and compare it to the stored manifest, byte for byte.

A row whose step has since reached a TERMINAL status — recorded, exhausted
its retries and failed, or was skipped or superseded — is skipped: the ready
set legitimately advancing past a step that finished is not a conflict. Every
other stored row is matched by the step it names, not by its position in the
manifest. Equal (or entirely explained by rows like that) is exit 0. Unequal
is a CONFLICT naming the manifest's own row position, with the stored row and
the recomputed row both rendered — so an operator can see whether a lease
lapsed, a priority changed, or the step vanished from the ready set outright,
rather than being told the two differ.

THIS VERB WRITES NOTHING, including no lease reap. It is the one
scheduling-shaped verb that must not reap: reaping would change the very ready
set it was asked to compare against, and a verify that mutated its own subject
could never fail.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchVerify(cmd, getWriter(cmd))
	},
}

func runDispatchVerify(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}

	result, mismatch, err := engine.NewEngine().VerifyDispatch(conn, runID, model.NowMS())
	if err != nil {
		return runErr(err)
	}
	if mismatch != nil {
		return mismatchConflict("", result, mismatch)
	}

	warnStaleTargets(w, result.StaleTargets)

	var message string
	if !w.JSONMode {
		message = fmt.Sprintf("%s verified: the manifest matches the current ready set (%s)",
			result.Dispatch, verdictTally(result.Rows))
	}
	w.Success(result, message)
	return nil
}

// mismatchConflict is P9's refusal, rendered once for both the verbs that can
// raise it (`dispatch verify` and `dispatch close --backfill-from`).
//
// The differing BYTES, both sides, escaped on their way to a terminal: a
// manifest row carries an executor hint and a metadata bag, which are
// workflow-author strings and therefore untrusted text (gates-trust §5.7 R11).
//
// The per-row summary rides ABOVE the refusal (DKT-243). The refusal itself
// still names the first offending row and shows its bytes — unchanged — but a
// dispatch where several steps moved mid-flight used to report one of them and
// hide the rest, costing a manual per-step confirm round before a `close` that
// reconciles the same state without complaint.
//
// `prefix` is empty for `dispatch verify`, so that verb's refusal is
// byte-identical to what it has always printed; the reconcile path passes its
// stage name (DKT-580) so an operator learns WHICH of three stages refused
// without the diagnostic itself being reworded.
func mismatchConflict(
	prefix string, result *engine.VerifyResult, mismatch *engine.RowMismatch,
) error {
	return cmdErr(fmt.Errorf(
		"%s%s does not match its current rendering (manifest row %d)\n%s"+
			"  stored:     %s\n"+
			"  recomputed: %s",
		prefix, result.Dispatch, mismatch.Position, renderRowVerdicts(result.Rows),
		renderRowOrAbsent(mismatch.Stored), renderRowOrAbsent(mismatch.Computed)),
		output.ErrConflict)
}

// renderRowVerdicts is DKT-243's per-row block: every stored row that is not
// plainly matched, named with what happened to it.
//
// Matched rows are TALLIED rather than listed — a manifest is mostly matched
// rows, and printing them would bury the handful that are not. What an
// operator needs before deciding between another `verify`, a per-step confirm
// round, and a `close` is which rows recorded (fine), which shifted (fine
// after a look), and which went genuinely missing (not fine).
func renderRowVerdicts(rows []engine.RowVerdict) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  rows: %s\n", verdictTally(rows))
	for _, r := range rows {
		if r.Verdict == engine.RowMatched {
			continue
		}
		fmt.Fprintf(&b, "    [%d] %s %s: %s\n",
			r.Position, r.Step, exec.Render(r.Instance), r.Verdict)
	}
	return b.String()
}

// verdictTally counts the verdicts in stored order of first appearance, so the
// same manifest always renders the same summary.
func verdictTally(rows []engine.RowVerdict) string {
	if len(rows) == 0 {
		return "no rows"
	}
	counts := map[string]int{}
	var order []string
	for _, r := range rows {
		if counts[r.Verdict] == 0 {
			order = append(order, r.Verdict)
		}
		counts[r.Verdict]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, v := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[v], v))
	}
	return strings.Join(parts, ", ")
}

// renderRowOrAbsent names an absent row rather than printing an empty string.
//
// A step that legitimately completed — recorded, failed, skipped, or
// superseded — never reaches this rendering: the engine's DKT-10 clause
// skips a terminal step's row before a mismatch is even built. An EMPTY
// `Computed` here therefore means the narrower, more alarming case: the
// stored row's step is STILL non-terminal and yet is no longer offerable —
// claimed since the manifest opened, excluded by a scope conflict with a
// row admitted since, or similar. Without this name it would render as a
// blank line, which reads as a rendering bug rather than as the fact it is.
func renderRowOrAbsent(row string) string {
	if row == "" {
		return "(no row at this position)"
	}
	return exec.Render(row)
}

// ---- close -----------------------------------------------------------------

var dispatchCloseCmd = &cobra.Command{
	Use:   "close",
	Short: "Reconcile and close the open manifest",
	Long: `Close the open dispatch, but only if the run has no discrepancy.

There are exactly two discrepancy classes, both computed and never stored:

  claimed-but-unrecorded   a step claimed or running whose activity is older
                           than ` + "`dispatch.grace`" + `. Its resolution is LEASE
                           EXPIRY: the lease lapses, ` + "`next`" + ` reaps the step,
                           and the discrepancy dissolves on its own.

  usage-rows-missing       a step that was CLAIMED, finished after the run was
                           activated, and recorded no usage, in a run that has
                           ever opened a dispatch. A step nobody ever claimed
                           owes nothing and is never one of these, whatever
                           terminal status it carries. Its resolutions are
                           backfill-usage, which records what the relay
                           measured, and --accept-missing-usage, which settles
                           the question without answering it.

--accept-missing-usage does not accept the other class. Claimed-but-unrecorded
has its own resolution, and a flag that accepted both would let a relay close
over work that is still running.

--accept-missing-usage DOES NOT REQUIRE AN OPEN DISPATCH. It records a statement
about steps, not about a manifest, and requiring one made the refusal a cycle:
next offers no work until the discrepancy clears, and the named way to clear it
needed a dispatch that was not open. Without the flag, closing a manifest that
is not open is still a conflict.

The acceptance clears the discrepancy for next and dispatch open, not only for
the dispatch row — those two verbs refuse on exactly the same conditions.

--backfill-from PATH closes a wave in ONE invocation: it back-fills the usage in
PATH, verifies the manifest, and closes it, refusing at whichever stage fails
with that stage named. PATH is the same JSON array ` + "`backfill-usage --from-json`" + `
reads — [{"step","unit","quantity"}, ...] — and "-" reads stdin.

  docket dispatch close --run RUN-44 --backfill-from usage.json

It is exactly the three verbs, in the one order they can run in, calling the
same engine functions they call. Each stage is all-or-nothing on its own: a
failed back-fill runs no verify and no close, a failed verify runs no close, and
nothing is ever half-closed. --source and --on-duplicate mean what they mean on
` + "`backfill-usage`" + ` and apply to the back-fill stage, as does its
never-claimed-target warning: a conductor that traded three invocations for one
loses no advisory.

An open dispatch is REQUIRED with --backfill-from, because the verify stage
needs a manifest to compare. --accept-missing-usage still applies, to the close
stage; the standalone verbs are unchanged and remain the way to run the stages
one at a time.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchClose(cmd, getWriter(cmd))
	},
}

func runDispatchClose(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}
	accept, _ := cmd.Flags().GetBool("accept-missing-usage")

	// DKT-580: with `--backfill-from` this verb is the whole wave-close
	// pipeline. Without it, every line below is what it has always been —
	// criterion 2 is that the plain close did not change, so the reconcile is a
	// branch taken on the flag rather than a rewrite of the default path.
	if from, _ := cmd.Flags().GetString("backfill-from"); from != "" {
		return runDispatchReconcile(cmd, w, runID, from, accept)
	}

	outcome, err := engine.NewEngine().CloseDispatch(conn, runID, accept, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderCloseOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

// runDispatchReconcile is `dispatch close --backfill-from` (DKT-580): one
// invocation for back-fill, verify, and close.
//
// The conductor's wave close was four typed commands with a temp file between
// two of them, repeated once per wave. Nothing in it is a decision — the order
// is forced and the arguments are the same run — so every retype was a chance
// to point at the wrong journal or skip the verify, and a skipped verify is how
// a manifest closes over a ready set that moved underneath it.
func runDispatchReconcile(
	cmd *cobra.Command, w *output.Writer, runID int, from string, accept bool,
) error {
	rows, err := reconcileRows(cmd, from)
	if err != nil {
		return err
	}
	source, _ := cmd.Flags().GetString("source")
	onDuplicate, _ := cmd.Flags().GetString("on-duplicate")

	outcome, err := engine.NewEngine().ReconcileDispatch(
		getDB(cmd), runID, rows, source, onDuplicate, accept, model.NowMS())
	if err != nil {
		return reconcileErr(err)
	}

	// The same advisories the standalone verbs emit, from the same stages: a
	// conductor that switched to one invocation must not lose a warning it
	// would have seen across three.
	for _, sk := range outcome.Backfill.Skipped {
		w.Warn("skipped %s (%s) attempt %d: %q usage already recorded",
			sk.Step, exec.Render(sk.Instance), sk.Attempt, sk.Unit)
	}
	warnUnclaimedTargets(w, outcome.Backfill.Unclaimed)
	warnStaleTargets(w, outcome.Verify.StaleTargets)

	var message string
	if !w.JSONMode {
		message = renderReconcileOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

// reconcileErr maps a staged failure onto the taxonomy.
//
// A verify-stage MISMATCH is the one refusal whose diagnostic cannot be
// rebuilt from the error text — P9's evidence is the differing bytes and
// DKT-243's verdict block, both carried on the result — so it is rendered
// through the same function `dispatch verify` renders it with, with the stage
// named in front. Every other stage failure keeps its own message and its own
// exit code: StageError unwraps to the stage's error, so runErr's lookup finds
// the code the standalone verb would have exited with.
func reconcileErr(err error) error {
	var stage *engine.StageError
	if errors.As(err, &stage) && stage.Mismatch != nil {
		return mismatchConflict(
			stage.Stage+" stage: ", stage.Verify, stage.Mismatch)
	}
	return runErr(err)
}

// reconcileRows reads `--backfill-from`.
//
// It is `backfill-usage --from-json`'s reader, deliberately: the file a
// conductor pipes into one verb is the file it pipes into the other, and a
// second parser would be a second set of rules for the same bytes.
//
// A DIRECTORY IS REFUSED BY NAME. DKT-580 describes the flag as taking "a wave
// transcript dir or usage json", and core cannot read the former: a transcript
// tree is the harness's own format, and core inventing a reader for it would be
// core holding an opinion about how a relay records its spend — the same
// opinion `--source` and `unit` exist to avoid holding. The refusal names the
// tool that turns one into the other rather than failing as an unreadable file.
func reconcileRows(cmd *cobra.Command, path string) ([]engine.BackfillRow, error) {
	if path != "-" {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return nil, cmdErr(fmt.Errorf(
				"--backfill-from %s is a directory; core reads usage rows, not "+
					"transcript trees. Pass the usage JSON a journal tool "+
					`(wave-usage) produces — a JSON array of `+
					`{"step","unit","quantity"} — or "-" to pipe it on stdin`,
				path), output.ErrValidation)
		}
	}
	return backfillRowsFromJSON(cmd, path)
}

// renderReconcileOutcome is the human view: one line per stage, in the order
// they ran, each reusing the wording its standalone verb prints.
func renderReconcileOutcome(o *engine.ReconcileOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Back-filled %d usage row(s) across %d step(s) as %q\n",
		o.Backfill.Written, o.Backfill.Steps, o.Backfill.Source)
	fmt.Fprintf(&b, "%s verified: the manifest matches the current ready set (%s)\n",
		o.Verify.Dispatch, verdictTally(o.Verify.Rows))
	b.WriteString(renderCloseOutcome(o.Close))
	return b.String()
}

// ---- backfill-usage --------------------------------------------------------

var dispatchBackfillUsageCmd = &cobra.Command{
	Use:   "backfill-usage",
	Short: "Record usage for steps whose claimant could not report it",
	Long: `Record usage a relay measured but the claimant could not report itself.

engine-core §7's back-fill: a relay that measured its own spawns carries those
numbers into the ledger, with the source recorded. Usage otherwise rides only on
` + "`step complete --usage`" + `, which a claimant that cannot observe its own
consumption has no way to supply — leaving the ledger empty and the
usage-rows-missing discrepancy permanent.

Two forms, one per invocation:

  --step STEP-N --unit U --quantity Q     repeatable, one triple per row
  --from-json PATH                        a JSON array; "-" reads stdin

    [{"step": "STEP-12", "unit": "tokens", "quantity": 48211}, ...]

THE WHOLE BATCH IS ONE TRANSACTION. A back-fill that half-applied would leave a
dispatch neither closable nor honestly re-runnable.

Rows land against the step's RECORDED attempt. There is no flag to name a
different one: back-filling an arbitrary historical attempt is rewriting
history, and the ledger's (step, attempt, unit) key exists so a retried step's
second attempt records beside its first rather than over it.

` + "`--on-duplicate`" + ` decides what happens when a row's (step, attempt, unit)
is already in the ledger. ` + "`refuse`" + ` (the default) aborts the WHOLE batch —
right when a duplicate means real spend is about to be double-counted.
` + "`skip`" + ` passes that row over, records the rest, and NAMES every row it
skipped. Cross-wave duplicates are structural — a gate probed in wave N and
seated in wave N+1 emits usage in both journals — and refusing the batch for
them meant hand-filtering rows before every re-run (DKT-241).

A step NO WORKER EVER CLAIMED is back-filled anyway, and WARNED ABOUT by name
and status on stderr — plus an ` + "`unclaimed`" + ` array in ` + "`--json`" + `. Trusting the
relay stays the default: core cannot know that a step which never claimed cost
nothing, so it records the row. What it will not do is stay silent. A wave
journal whose join heuristic mis-attributes a claimant's usage lands it on a
` + "`pending`" + ` or swept-` + "`superseded`" + ` step, and ` + "`run report`" + ` then shows spend
on work that never ran with nothing to say where it came from. The warning names
the step so the journal that produced the row can be checked; a step that ran
and simply could not report — the case this verb exists for — never triggers it.

` + "`docket run report`" + ` lists what is already recorded, per step, under
` + "`step_usage`" + `: that is the verb to check before re-running.

` + "`--source`" + ` defaults to "backfilled" and is free text. Core enumerates no
sources, for the same reason it enumerates no units — who measured the work is
not core's opinion to hold.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchBackfillUsage(cmd, getWriter(cmd))
	},
}

// backfillOutcome is what the verb reports: the run, the rows written, and the
// source they carry.
type backfillOutcome struct {
	Run    string `json:"run"`
	Rows   int    `json:"rows"`
	Steps  int    `json:"steps"`
	Source string `json:"source"`
	// Skipped names the rows --on-duplicate=skip passed over. `omitempty`, so
	// the default refusing mode's payload is unchanged.
	Skipped []engine.SkippedRow `json:"skipped,omitempty"`
	// Unclaimed names the target steps no worker ever claimed (DKT-993). It
	// rides the payload because `Warn` is silent in JSON mode and a conductor
	// runs with `--json`: an advisory only a human could see is the same
	// silence this field exists to end. `omitempty`, so an ordinary back-fill's
	// payload is byte-identical to what it was.
	Unclaimed []engine.UnclaimedTarget `json:"unclaimed,omitempty"`
}

func runDispatchBackfillUsage(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}
	rows, err := backfillRows(cmd)
	if err != nil {
		return err
	}
	source, _ := cmd.Flags().GetString("source")
	onDuplicate, _ := cmd.Flags().GetString("on-duplicate")

	result, err := engine.NewEngine().BackfillUsage(
		conn, runID, rows, source, onDuplicate, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	// Every skip is NAMED, not counted. A conductor that asked for skip is
	// deciding whether a duplicate is the structural cross-wave kind or a real
	// double-count, and a bare number cannot answer that (DKT-241).
	for _, sk := range result.Skipped {
		w.Warn("skipped %s (%s) attempt %d: %q usage already recorded",
			sk.Step, exec.Render(sk.Instance), sk.Attempt, sk.Unit)
	}
	warnUnclaimedTargets(w, result.Unclaimed)

	outcome := &backfillOutcome{
		Run: model.FormatRunID(runID), Rows: result.Written,
		Steps: result.Steps, Source: result.Source, Skipped: result.Skipped,
		Unclaimed: result.Unclaimed,
	}

	var message string
	if !w.JSONMode {
		message = fmt.Sprintf("Back-filled %d usage row(s) across %d step(s) "+
			"as %q", outcome.Rows, outcome.Steps, outcome.Source)
	}
	w.Success(outcome, message)
	return nil
}

// backfillRows assembles the batch from whichever form the caller used.
//
// The two forms are mutually exclusive rather than merged: a caller passing
// both has two sources of truth for one batch and no way to say which wins, and
// silently concatenating them would write rows nobody asked for.
func backfillRows(cmd *cobra.Command) ([]engine.BackfillRow, error) {
	fromJSON, _ := cmd.Flags().GetString("from-json")
	steps, _ := cmd.Flags().GetStringSlice("step")

	if fromJSON != "" && len(steps) > 0 {
		return nil, cmdErr(fmt.Errorf(
			"--from-json and --step are two forms of the same batch; pass one"),
			output.ErrValidation)
	}
	if fromJSON != "" {
		return backfillRowsFromJSON(cmd, fromJSON)
	}
	return backfillRowsFromFlags(cmd, steps)
}

// backfillRowsFromFlags pairs the repeatable triples positionally: the Nth
// --step takes the Nth --unit and the Nth --quantity. A mismatched count is
// refused rather than zip-truncated, because a truncated batch writes a subset
// the caller never named.
func backfillRowsFromFlags(cmd *cobra.Command, steps []string) ([]engine.BackfillRow, error) {
	units, _ := cmd.Flags().GetStringSlice("unit")
	quantities, _ := cmd.Flags().GetFloat64Slice("quantity")

	if len(steps) == 0 {
		return nil, cmdErr(fmt.Errorf(
			"pass --step with its --unit and --quantity, or --from-json"),
			output.ErrValidation)
	}
	if len(units) != len(steps) || len(quantities) != len(steps) {
		return nil, cmdErr(fmt.Errorf(
			"got %d --step, %d --unit, and %d --quantity; each row needs all "+
				"three", len(steps), len(units), len(quantities)),
			output.ErrValidation)
	}

	out := make([]engine.BackfillRow, 0, len(steps))
	for i, ref := range steps {
		id, err := model.ParseStepID(ref)
		if err != nil {
			return nil, cmdErr(fmt.Errorf("invalid step ID %q: %w", ref, err),
				output.ErrValidation)
		}
		out = append(out, engine.BackfillRow{
			Step: id, Unit: units[i], Quantity: quantities[i],
		})
	}
	return out, nil
}

// backfillRowsFromJSON reads the batch form, which is what a relay with N
// spawns uses rather than N process launches.
func backfillRowsFromJSON(cmd *cobra.Command, path string) ([]engine.BackfillRow, error) {
	// `-` reads stdin, the same convention `guard spawn --rows` uses.
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, cmdErr(
				fmt.Errorf("reading the back-fill batch from stdin: %w", err),
				output.ErrGeneral)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, cmdErr(
				fmt.Errorf("reading the back-fill batch from %s: %w", path, err),
				output.ErrNotFound)
		}
	}

	var wire []struct {
		Step     string  `json:"step"`
		Unit     string  `json:"unit"`
		Quantity float64 `json:"quantity"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, cmdErr(fmt.Errorf(
			"reading the back-fill batch: %w — it is a JSON array of "+
				`{"step","unit","quantity"}`, err), output.ErrValidation)
	}

	out := make([]engine.BackfillRow, 0, len(wire))
	for i, r := range wire {
		id, err := model.ParseStepID(r.Step)
		if err != nil {
			return nil, cmdErr(fmt.Errorf("row %d: invalid step ID %q: %w",
				i, r.Step, err), output.ErrValidation)
		}
		out = append(out, engine.BackfillRow{
			Step: id, Unit: r.Unit, Quantity: r.Quantity,
		})
	}
	return out, nil
}

// ---- abandon ---------------------------------------------------------------

var dispatchAbandonCmd = &cobra.Command{
	Use:   "abandon",
	Short: "Give up on the open manifest unconditionally",
	Long: `Retire the open dispatch whether or not the run has discrepancies.

This is the CRASHED-RELAY path, and its unconditionality is the point: the relay
is gone and cannot resolve anything. A recovery verb that refused to recover
while a discrepancy existed would be the mechanism by which a crashed relay
wedges a run — exactly what the dispatch TTL and this verb exist to prevent.

Nothing is lost. The steps in the manifest were never claimed by opening it, so
they return to the ready set intact, and an executor that claimed one BEFORE the
crash finishes normally.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchAbandon(cmd, getWriter(cmd))
	},
}

func runDispatchAbandon(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}
	reason, _ := cmd.Flags().GetString("reason")

	outcome, err := engine.NewEngine().AbandonDispatch(conn, runID, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderCloseOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

func renderCloseOutcome(o *engine.CloseOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is %s (%s)", o.Dispatch, o.Status, o.Reason)
	for _, instance := range o.Accepted {
		fmt.Fprintf(&b, "\n  accepted missing usage: %s", exec.Render(instance))
	}
	return b.String()
}

// ---- waive-target ----------------------------------------------------------

var dispatchWaiveTargetCmd = &cobra.Command{
	Use:   "waive-target",
	Short: "Stop an adjudicated stale-target warning from re-firing",
	Long: `Record that a stale-target warning was investigated and ruled acceptable.

The stale-target advisory (` + "`dispatch open`/`verify`" + `) re-computes on every
invocation and, without this verb, has no memory: a warning an operator already
investigated re-fires unchanged at every subsequent open and verify of the same
(step, target) pair, costing an investigation each time.

A waiver names one target sha and one or more step instances — exactly the
strings the warning itself printed:

  docket dispatch waive-target --run RUN-52 --target 12f5006a \
      --step "review@1#0" --step "review@1#1" \
      --note "fix integrated as b68bf23; divergence is the later format pass"

The sha may be the 12-character prefix the warning renders (7 hex characters
minimum); matching is case-insensitive prefix against the recorded target.

THE WARNING MACHINERY STILL RUNS. A waiver suppresses only the exact pair it
names: the same step warning about a DIFFERENT sha, or the same sha on a step
no waiver names, warns exactly as before — a new divergence never rides an old
ruling. Waivers are RUN-SCOPED and die with their run; each one is recorded as
a ` + "`stale-target-waived`" + ` event, so the feed shows what standing precedent was
minted and why.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDispatchWaiveTarget(cmd, getWriter(cmd))
	},
}

// waiveTargetOutcome is what the verb reports: the run and the waivers minted.
type waiveTargetOutcome struct {
	Run     string                `json:"run"`
	Waivers []engine.WaivedTarget `json:"waivers"`
}

func runDispatchWaiveTarget(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := dispatchRunID(cmd)
	if err != nil {
		return err
	}
	steps, _ := cmd.Flags().GetStringSlice("step")
	target, _ := cmd.Flags().GetString("target")
	note, _ := cmd.Flags().GetString("note")

	waivers, err := engine.NewEngine().WaiveStaleTargets(
		conn, runID, steps, target, note, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	outcome := &waiveTargetOutcome{Run: model.FormatRunID(runID), Waivers: waivers}
	var message string
	if !w.JSONMode {
		var b strings.Builder
		fmt.Fprintf(&b, "Waived the stale-target warning for %.12s on %d step(s):",
			target, len(waivers))
		for _, wv := range waivers {
			fmt.Fprintf(&b, "\n  %s (waiver %d)", wv.Instance, wv.ID)
		}
		message = b.String()
	}
	w.Success(outcome, message)
	return nil
}

// ---- shared flags ----------------------------------------------------------

// dispatchRunID resolves the `--run` every dispatch verb requires.
//
// It is REQUIRED rather than defaulted to an active run: a dispatch is a
// statement about one run's batch, and a verb that guessed which run would make
// `dispatch abandon` — the destructive one — guess too.
func dispatchRunID(cmd *cobra.Command) (int, error) {
	ref, _ := cmd.Flags().GetString("run")
	return parseRunFlag(ref)
}

// ackSeqs parses the repeatable `--ack-reap SEQ` flag (§6.2).
//
// A non-numeric value is refused HERE rather than reaching the engine, so the
// error names the flag the user typed. The engine's own refusal (A9) is about a
// seq that names no reap, which is a different mistake with a different fix.
func ackSeqs(cmd *cobra.Command) ([]int64, error) {
	raw, _ := cmd.Flags().GetInt64Slice("ack-reap")
	for _, seq := range raw {
		if seq <= 0 {
			return nil, cmdErr(fmt.Errorf(
				"--ack-reap %d is not an event seq; pass the `seq` of the "+
					"`lease-reaped` event being acknowledged", seq),
				output.ErrValidation)
		}
	}
	return raw, nil
}

func init() {
	for _, c := range []*cobra.Command{
		dispatchOpenCmd, dispatchVerifyCmd, dispatchCloseCmd, dispatchAbandonCmd,
		dispatchBackfillUsageCmd, dispatchWaiveTargetCmd,
	} {
		c.Flags().String("run", "", "The run whose dispatch this is (required)")
		_ = c.MarkFlagRequired("run")
	}

	// `--limit` applies with the SAME ordering-then-slicing rule `next` uses, so
	// a relay can open a manifest for the batch size it can actually spawn and
	// get the highest-priority steps rather than an arbitrary subset.
	dispatchOpenCmd.Flags().Int("limit", 0, "Maximum manifest rows (0 = no limit)")
	dispatchOpenCmd.Flags().Int64Slice("ack-reap", nil,
		"Acknowledge a write-class reap by its `lease-reaped` event seq (repeatable)")

	dispatchCloseCmd.Flags().Bool("accept-missing-usage", false,
		"Close despite missing-usage discrepancies, recording the acceptance")

	// DKT-580's one-verb wave close. `--source` and `--on-duplicate` are the
	// SAME flags `backfill-usage` declares, with the same defaults, because
	// they configure the same stage; they are inert without `--backfill-from`.
	dispatchCloseCmd.Flags().String("backfill-from", "",
		`Back-fill this JSON array of {"step","unit","quantity"}, verify, `+
			`then close, in one invocation; "-" reads stdin`)
	dispatchCloseCmd.Flags().String("on-duplicate", "refuse",
		"With --backfill-from: what to do with a row whose (step, attempt, "+
			"unit) is already recorded: refuse the batch, or skip that row "+
			"and report it")
	dispatchCloseCmd.Flags().String("source", "",
		"With --backfill-from: who measured it; recorded on every row "+
			"(default \"backfilled\")")

	dispatchAbandonCmd.Flags().String("reason", "",
		"Why the manifest is being given up on; recorded in the event")

	// The back-fill's two forms. `--step/--unit/--quantity` pair positionally,
	// one row per triple; `--from-json` carries a whole batch.
	dispatchBackfillUsageCmd.Flags().StringSlice("step", nil,
		"Step whose usage is being recorded (repeatable)")
	dispatchBackfillUsageCmd.Flags().StringSlice("unit", nil,
		"Unit for the matching --step (repeatable); core has no default unit")
	dispatchBackfillUsageCmd.Flags().Float64Slice("quantity", nil,
		"Quantity for the matching --step and --unit (repeatable)")
	dispatchBackfillUsageCmd.Flags().String("from-json", "",
		`JSON array of {"step","unit","quantity"}; "-" reads stdin`)
	dispatchBackfillUsageCmd.Flags().String("on-duplicate", "refuse",
		"What to do with a row whose (step, attempt, unit) is already "+
			"recorded: refuse the batch, or skip that row and report it")
	dispatchBackfillUsageCmd.Flags().String("source", "",
		"Who measured it; recorded on every row (default \"backfilled\")")

	// DKT-742's waiver: the step instances and target sha exactly as the
	// stale-target warning printed them.
	dispatchWaiveTargetCmd.Flags().StringSlice("step", nil,
		"Step instance the warning named, e.g. \"review@1#0\" (repeatable)")
	dispatchWaiveTargetCmd.Flags().String("target", "",
		"The warned target sha — full, or the >=7-char prefix the warning renders")
	dispatchWaiveTargetCmd.Flags().String("note", "",
		"Why the warning is adjudicated acceptable; recorded on every waiver")
	_ = dispatchWaiveTargetCmd.MarkFlagRequired("step")
	_ = dispatchWaiveTargetCmd.MarkFlagRequired("target")

	dispatchCmd.AddCommand(dispatchOpenCmd, dispatchVerifyCmd,
		dispatchCloseCmd, dispatchAbandonCmd, dispatchBackfillUsageCmd,
		dispatchWaiveTargetCmd)
	rootCmd.AddCommand(dispatchCmd)
}
