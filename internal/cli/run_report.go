package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// `docket run report` — the ledger rollup (engine-core §1.1, §3.5).
//
// BUILT ON THE `internal/cli/stats.go` PATTERN, which is the repo's existing
// rollup verb: a flat result struct with JSON tags, one loader, a `renderXxx`
// for human mode and a `renderPlainXxx` for the no-color path, and
// `w.Success(result, message)`. Following it means the report gains `--json=v2`'s
// envelope, the color discipline, and the plain-text fallback without inventing
// any of them.

var runReportCmd = &cobra.Command{
	Use:   "report RUN-N",
	Short: "Roll up a run's usage, attempts, gates, actions, and artifacts",
	Long: `Report on a run: budget, steps, gates, actions, artifacts, metadata.

READ-ONLY. This verb computes effective status and WRITES NOTHING — not even a
lease reap — so polling it cannot advance a run.

It works on a run in ANY status. A ` + "`planning`" + ` run reports zeros; an abandoned
one reports the trail up to abandonment. A report that refused on a
non-terminal run would be useless during exactly the run you want to inspect.

STEP METADATA IS REPORTED TWICE, on purpose. ` + "`Metadata`" + ` rolls every key up
to its distinct values with counts — a run-level answer — and ` + "`Step metadata`" + `
prints each step's whole bag beside the status that step ended in. Grouping by
key is what discards which values two keys took TOGETHER on one step, so a bag
whose keys are a request and its resolution is readable only in the second
section. Core reads no key in either.

THE BUDGET NUMBERS ARE BARE. There is no currency and no unit: what they count
is the workflow's business. The report publishes the cap, where the cap came
from, the declared-cost floor, reported usage per unit, max(reported, floor),
and the burn rate — so a policy that wants to warn at some fraction of a cap
computes it from these numbers rather than from an opinion shipped here.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunReport(cmd, args[0], getWriter(cmd))
	},
}

func runRunReport(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	report, err := engine.LoadRunReport(conn, runID, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderRunReport(report)
	}
	w.Success(runReportResult{report}, message)
	return nil
}

// runReportResult wraps the report so `--json=v2` carries the run's row version
// on the nested run, exactly as `run status` does. The rollup is ABOUT the run
// rather than part of it, so it stays nested under `run` in both versions.
type runReportResult struct{ *engine.RunReport }

func (r runReportResult) VersionedPayload() any {
	type alias engine.RunReport
	return struct {
		Run any `json:"run"`
		*alias
	}{
		Run:   model.VersionedRun{Run: *r.RunReport.Run},
		alias: (*alias)(r.RunReport),
	}
}

// renderRunReport is the human-mode document.
//
// EVERY STORED STRING GOES THROUGH exec.Render (R11, gates-trust §5.7): gate and
// action names, metadata values, artifact kinds, breach reasons, and step
// instances are all attacker-influenced text on its way to a TERMINAL, and a TTY
// interprets control bytes. `--json` carries the RAW bytes — encoding/json
// escapes them by contract and the consumer is a program (E4).
func renderRunReport(r *engine.RunReport) string {
	if !render.ColorsEnabled() {
		return renderPlainRunReport(r)
	}

	section := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	value := lipgloss.NewStyle().Bold(true)

	var b strings.Builder

	// R1: the run.
	b.WriteString(section.Render("Run") + "\n")
	fmt.Fprintf(&b, "  %s %s\n", label.Render("Run:"), value.Render(r.Run.Ref()))
	fmt.Fprintf(&b, "  %s %s\n", label.Render("Status:"), value.Render(string(r.Run.Status)))
	if r.Run.Reason != "" {
		fmt.Fprintf(&b, "  %s %s\n", label.Render("Reason:"), exec.Render(r.Run.Reason))
	}
	if r.Run.Request != "" {
		fmt.Fprintf(&b, "  %s %s\n", label.Render("Request:"),
			exec.Render(requestSummary(r.Run.Request)))
	}
	if r.WallClockMS > 0 {
		fmt.Fprintf(&b, "  %s %s\n", label.Render("Wall clock:"),
			value.Render(formatDurationMS(r.WallClockMS)))
	}

	// R2: the budget.
	b.WriteString("\n" + section.Render("Budget") + "\n")
	writeBudgetLines(&b, r.Budget, func(k, v string) {
		fmt.Fprintf(&b, "  %s %s\n", label.Render(k), value.Render(v))
	})

	writeReportSections(&b, r,
		func(title string) { b.WriteString("\n" + section.Render(title) + "\n") },
		func(k, v string) { fmt.Fprintf(&b, "  %s %s\n", label.Render(k), value.Render(v)) })

	return strings.TrimRight(b.String(), "\n")
}

// renderPlainRunReport is the no-color path — the same document, same order,
// no styling.
func renderPlainRunReport(r *engine.RunReport) string {
	var b strings.Builder

	b.WriteString("Run\n")
	fmt.Fprintf(&b, "  %-14s %s\n", "Run:", r.Run.Ref())
	fmt.Fprintf(&b, "  %-14s %s\n", "Status:", r.Run.Status)
	if r.Run.Reason != "" {
		fmt.Fprintf(&b, "  %-14s %s\n", "Reason:", exec.Render(r.Run.Reason))
	}
	if r.Run.Request != "" {
		fmt.Fprintf(&b, "  %-14s %s\n", "Request:", exec.Render(requestSummary(r.Run.Request)))
	}
	if r.WallClockMS > 0 {
		fmt.Fprintf(&b, "  %-14s %s\n", "Wall clock:", formatDurationMS(r.WallClockMS))
	}

	b.WriteString("\nBudget\n")
	writeBudgetLines(&b, r.Budget, func(k, v string) {
		fmt.Fprintf(&b, "  %-14s %s\n", k, v)
	})

	writeReportSections(&b, r,
		func(title string) { fmt.Fprintf(&b, "\n%s\n", title) },
		func(k, v string) { fmt.Fprintf(&b, "  %-14s %s\n", k, v) })

	return strings.TrimRight(b.String(), "\n")
}

// writeBudgetLines emits R2's numbers through the caller's line writer, so the
// styled and plain renderers cannot disagree about WHICH numbers appear or in
// what order — only about how they look.
func writeBudgetLines(b *strings.Builder, budget engine.RunBudgetReport, line func(k, v string)) {
	cap := "unlimited"
	if budget.Cap > 0 {
		cap = fmt.Sprintf("%g (%s)", budget.Cap, budget.Source)
	}
	line("Cap:", cap)
	line("Floor:", fmt.Sprintf("%g", budget.Floor))
	line("Spend:", fmt.Sprintf("%g", budget.Spend))
	// The burn rate is ROUNDED for display and carried at full precision in
	// `--json`. It is a derived ratio of two measured numbers, so its trailing
	// digits are noise an operator would have to read past — while a consumer
	// computing a policy from it wants the number the division produced.
	line("Burn rate:", fmt.Sprintf("%.3f/hour", budget.BurnRate))

	if budget.Unit != "" {
		line("Cap counts:", exec.Render(budget.Unit))
	}
	for _, u := range budget.Reported {
		// The unit is stored, opaque, and claimant-supplied — so it is rendered
		// through the escaper like every other stored string (R11).
		line("Reported "+exec.Render(u.Unit)+":", fmt.Sprintf("%g", u.Quantity))
	}
	// The MEASURED dimension (DKT-238), when a run armed it. Its own lines,
	// not folded into the numbers above: it is a separate cap over a different
	// quantity, and a reader who cannot tell them apart cannot tell which one
	// to raise.
	if budget.UsageCap > 0 {
		if budget.UsageUnit == "" {
			line("Usage cap:", fmt.Sprintf(
				"%g (DORMANT: budget.usage.unit is unset, so it counts nothing)",
				budget.UsageCap))
		} else {
			line("Usage cap:", fmt.Sprintf("%g (measured %s)",
				budget.UsageCap, exec.Render(budget.UsageUnit)))
			line("Usage spend:", fmt.Sprintf("%g", budget.UsageSpend))
		}
	}
	if budget.BreachReason != "" {
		line("Breach:", exec.Render(budget.BreachReason))
	}
	// DKT-584: on a run whose panels cast, the budget section itself says the
	// seats' measured spend is excluded from the numbers above and where it
	// went instead — the silent omission is the bug. The note is core's own
	// constant (engine.VoteUsageExcludedNote), not stored text, so it is not
	// escaped: escaping it would quote a constant.
	if budget.VoteUsageNote != "" {
		line("Vote usage:", budget.VoteUsageNote)
	}
}

// writeReportSections emits R3 through R7. Shared for the same reason
// writeBudgetLines is: two renderers, one definition of the document.
func writeReportSections(
	b *strings.Builder, r *engine.RunReport,
	header func(title string), line func(k, v string),
) {
	dispositions := make(map[string]engine.IssueDisposition, len(r.Issues))
	for _, d := range r.Issues {
		dispositions[d.Issue] = d
	}
	stepLabel := stepLabeler(r.Attempts)

	// DKT-594's two pin sections, ahead of the step rollup because both are
	// statements about the AGREEMENT everything below ran under. A reader who
	// takes a finding out of this document needs to know the corpus moved
	// before they read the finding, not after.
	writePinnedWorkflows(r.PinnedWorkflows, header, line)
	writePinEpochs(r.PinEpochs, r.Attempts, stepLabel, header, line)

	if len(r.Steps) > 0 {
		header("Steps")
		for _, sc := range r.Steps {
			line(sc.Status+":", fmt.Sprintf("%d", sc.Count))
		}
		// Attempts are reported only where they exceed one: a step claimed once
		// is the ordinary case, and listing every one of them would bury the
		// retries an operator is looking for.
		var retried []engine.StepAttempt
		for _, a := range r.Attempts {
			if a.Attempts > 1 {
				retried = append(retried, a)
			}
		}
		if len(retried) > 0 {
			header("Attempts")
			for _, a := range retried {
				line(stepLabel(a)+":", fmt.Sprintf("%d", a.Attempts))
			}
		}

		// DKT-258: the steps whose STATUS does not say what happened to them.
		//
		// The counts above are a tally of words, and three of those words cover
		// outcomes that need opposite responses — `skipped` is both a tribunal
		// that never convened and one whose panel deliberated before an
		// operator resolved it; `failed-routed` is both a measured failure and
		// a step cascade-terminated by an issue-abandon without ever being
		// claimed. The engine records the difference in each step's routing and
		// the report never carried it.
		//
		// Only NON-PASS terminal rows are listed. A run's `done` steps are the
		// bulk of it and their routing says `pass`, which nobody is confused
		// about; printing all of them would bury the handful that a reader is
		// here to understand.
		var ended []engine.StepAttempt
		for _, a := range r.Attempts {
			nonPass := a.Routing != "" && a.Routing != engine.RoutingPass &&
				!strings.HasPrefix(a.Routing, engine.RoutingPass+":")
			// A vote step is listed whenever a panel EXISTED, whatever it
			// decided. An approved tribunal that an operator then resolved is
			// the same readability problem as a rejected one, and the count
			// section shows neither.
			if nonPass || a.Vote != "" {
				ended = append(ended, a)
			}
		}
		if len(ended) > 0 {
			header("How steps ended")
			for _, a := range ended {
				detail := a.Status
				if a.Vote != "" {
					detail += " after " + exec.Render(a.Vote)
				}
				if a.Routing != "" {
					detail += " — " + exec.Render(a.Routing)
				}
				// ...AND WHETHER THAT ROUTING IS STILL THE LAST WORD (DKT-403).
				//
				// A park's routing text is a QUESTION — "loop 4 would exceed
				// max_fix_loops = 3; `step resolve --as fix-round` authorizes
				// one more round". When an operator answered it by abandoning
				// the issue with `run abandon --issue`, the step went terminal
				// with that question still written on it, and this line
				// rendered an answered question as an open one. RUN-32's
				// post-mortem reader concluded from exactly this that two
				// resolved gates were "never resolved" and re-asked both.
				if resolved := stepResolution(a, dispositions); resolved != "" {
					detail += " — " + resolved
				}
				line(stepLabel(a)+":", detail)
			}
		}
	}

	// The issue-level rulings themselves (DKT-403), in full rather than as the
	// head the step lines carry.
	//
	// A SECTION AND an inline annotation, not one or the other. The annotation
	// is what a reader scanning "How steps ended" needs — they are looking at
	// the park text and must not walk away with it — and the section is where
	// the ruling is quoted at length and where an issue whose steps ALL ended
	// tidily (nothing to annotate) still gets its disposition published.
	if len(r.Issues) > 0 {
		header("How issues ended")
		for _, d := range r.Issues {
			detail := d.Disposition
			if d.By != "" {
				detail += " by " + exec.Render(d.By)
			}
			if d.Reason != "" {
				detail += " — " + exec.Render(requestSummary(d.Reason))
			}
			line(exec.Render(d.Issue)+":", detail)
		}
	}

	// E21's section (docs/tdd/runs-dispatch.md §8.7), folded into R3's
	// neighborhood because it is a count over the run's transitions rather than
	// over its steps.
	//
	// It is §9 item 2 published as a number: "every transition traceable to
	// next/gate/threshold/human input" is a claim an operator can check here
	// without leaving the report, and `events list` is where they go for the
	// individual rows if a count looks wrong.
	//
	// The actor is core's own closed vocabulary — four values, none of them
	// stored — so it is NOT escaped. Escaping it would quote a constant.
	if len(r.Actors) > 0 {
		header("Attribution")
		for _, a := range r.Actors {
			line(a.Actor+":", fmt.Sprintf("%d", a.Count))
		}
	}

	writeVerdicts(r.Gates, "Gates", header, line)
	writeVerdicts(r.Actions, "Actions", header, line)

	if len(r.Artifacts) > 0 {
		header("Artifacts")
		for _, a := range r.Artifacts {
			line(a.Artifact+":", fmt.Sprintf("%s from %s  %s  %d bytes",
				exec.Render(a.Kind), exec.Render(a.Producer),
				shortSHA(a.SHA256), a.Bytes))
		}
	}

	writeMetadataRollup(r.Metadata, "Metadata", header, line)
	// The bags THE ROLLUP JUST COLLAPSED, one step per line (DKT-868) —
	// immediately below the rollup for the same reason "Step usage" sits below
	// the budget's per-unit totals: the rollup is the headline and this is the
	// detail behind it.
	writeStepMetadata(r.Attempts, stepLabel, header, line)
	// The casts' own claims (DKT-71): the one spend the usage ledger cannot
	// attribute, rolled up the same opaque way.
	writeMetadataRollup(r.VoteMetadata, "Vote metadata", header, line)

	// The seats' own spend (DKT-95), summed per unit — rendered under the
	// same opaque-unit rule the budget's Reported lines follow.
	if len(r.VoteUsage) > 0 || r.VoteUsageCoverage.Casts > 0 {
		header("Vote usage")
		for _, u := range r.VoteUsage {
			line(exec.Render(u.Unit)+":",
				fmt.Sprintf("%g across %d seat report(s)", u.Quantity, u.Rows))
		}
		// COVERAGE IS PRINTED WHENEVER SEATS CAST, even with no rows at all
		// (DKT-257). Before it, `vote_usage` was empty and the section simply
		// did not appear — and an absent section reads as "no panels ran",
		// which is a different and much more comfortable claim than "panels ran
		// and none of them said what they cost". The ledger held zero rows for
		// an entire store epoch while 21+ seat-votes did real verification
		// work, ~40k output tokens per panel, and nothing distinguished the two.
		if c := r.VoteUsageCoverage; c.Casts > 0 {
			detail := fmt.Sprintf("%d of %d seat(s) reported spend", c.Reported, c.Casts)
			if c.Silent() > 0 {
				detail += fmt.Sprintf(
					" — %d reported NOTHING, so their spend is missing from this "+
						"run's totals, not zero", c.Silent())
				// The seating path(s) of the missing seats, ON the coverage
				// line (DKT-733): "which seat path fails to report" is the
				// diagnosis question, and the count alone could not answer it.
				// The paths are core's closed vocabulary, not escaped.
				if paths := silentSeatPathCounts(r.SilentVoteSeats); paths != "" {
					detail += " (" + paths + ")"
				}
			}
			line("Coverage:", detail)
			// The identity behind the count, one aimable line per seat: the
			// proposal is what `vote backfill-usage` takes. Voter and role are
			// stored, caster-supplied text on its way to a terminal (R11).
			for _, s := range r.SilentVoteSeats {
				who := exec.Render(s.Voter)
				if s.Role != "" {
					who += " as " + exec.Render(s.Role)
				}
				line("Silent:", fmt.Sprintf("%s seat %s  (%s)", s.Proposal, who, s.Path))
			}
			if len(r.SilentVoteSeats) > 0 {
				line("Backfill:",
					"`docket vote backfill-usage <proposal>` records these "+
						"seats' spend after the fact")
			}
		}
	}

	// The ledger row by row (DKT-241). The budget section above sums the same
	// rows per unit; this is what answers "which STEPS already have usage" —
	// the question a back-fill's duplicate refusal raises and that no read
	// verb could answer.
	if len(r.StepUsage) > 0 {
		header("Step usage")
		for _, u := range r.StepUsage {
			line(u.Step+":", fmt.Sprintf("%s attempt %d  %s %g  (%s)",
				exec.Render(u.Instance), u.Attempt,
				exec.Render(u.Unit), u.Quantity, exec.Render(u.Source)))
		}
	}
}

// silentSeatPathCounts renders the silent seats' seating paths with counts —
// `conversational-gate: 8, vote-step: 4` — for the coverage line (DKT-733).
// Sorted by path name: a total order, so two renders of one run are
// byte-identical (R9). Empty when there is nothing to attribute.
func silentSeatPathCounts(seats []engine.SilentVoteSeat) string {
	if len(seats) == 0 {
		return ""
	}
	counts := make(map[string]int, 2)
	for _, s := range seats {
		counts[s.Path]++
	}
	paths := make([]string, 0, len(counts))
	for p := range counts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, fmt.Sprintf("%s: %d", p, counts[p]))
	}
	return "silent by seating path — " + strings.Join(parts, ", ")
}

// stepLabeler decides how a step-attempt line NAMES its step (DKT-405).
//
// An instance label is unique within an issue and repeats across them: every
// issue expanded from one workflow carries the same `implement@0`,
// `reconcile@0`, `verify@2`. On the 4-issue RUN-32 that made whole sections
// unreadable — "Attempts" printed `"implement@0": 2` twice with nothing to
// tell the rows apart, and "How steps ended" printed `"reconcile@0": done —
// "fix-loop"` three times. A reader cannot act on a line that does not say
// which issue it is about, and cannot even tell a repeated row from a
// duplicate-rendering bug.
//
// The issue id rides ONLY where it disambiguates. On a single-issue run every
// line is already unambiguous, the id would be the same on all of them, and
// prefixing there would be a constant column repeated down the page — noise
// that also churns the golden output of every single-issue report. So the
// decision is made once per document, from the attempt rows themselves: two or
// more distinct issues among them and every line carries its issue.
//
// The pair is quoted TOGETHER rather than as two quoted tokens: both halves
// are stored text on their way to a terminal and must go through exec.Render
// (R11), and `"HRN-300 verify@2":` keeps the label reading as one name where
// `"HRN-300" "verify@2":` reads as two columns that happen to be adjacent.
//
// It returns the NAME WITHOUT the trailing colon, and the two sections that
// print it as a key add one. DKT-594's epoch section lists several of these
// inside one line's value, where a colon per name would read as a broken
// key-value pair — and a labeler that could only produce keys would have forced
// that section to re-derive the naming rule and be free to disagree with it.
func stepLabeler(attempts []engine.StepAttempt) func(engine.StepAttempt) string {
	bare := func(a engine.StepAttempt) string { return exec.Render(a.Instance) }

	issues := make(map[string]struct{}, 2)
	for _, a := range attempts {
		if a.Issue != "" {
			issues[a.Issue] = struct{}{}
		}
	}
	if len(issues) < 2 {
		return bare
	}
	return func(a engine.StepAttempt) string {
		// A row with no issue attribution at all keeps the bare label. Inventing
		// one would be a fabrication, and the rows that HAVE one still carry it.
		if a.Issue == "" {
			return bare(a)
		}
		return exec.Render(a.Issue + " " + a.Instance)
	}
}

// writePinnedWorkflows is DKT-594's staleness section: per pinned workflow, how
// far the registry has moved past the version this run is expanded from.
//
// IT PRINTS THE UP-TO-DATE ROWS TOO. The section exists because every analyst
// on RUN-32 went to git to find out whether the pinned `ui-change@8` was
// current before trusting a finding from the run — and "the report did not
// mention it" is not an answer to that question, it is the same absence that
// sent them to git. A line saying `current` is the answer; a missing line is
// indistinguishable from a report that never looked.
func writePinnedWorkflows(
	rows []engine.PinnedWorkflowStaleness,
	header func(title string), line func(k, v string),
) {
	if len(rows) == 0 {
		return
	}
	header("Pinned workflows")
	for _, w := range rows {
		// The name is a workflow author's opaque string on its way to a
		// terminal, so both refs go through the escaper (R11).
		current := exec.Render(fmt.Sprintf("%s@%d", w.Name, w.CurrentVersion))
		var detail string
		switch {
		case w.CurrentVersion == 0:
			// Every version of the name is retired, or the name is gone from
			// this project's registry. Not "current" — there is nothing to be
			// current WITH, and a run pinning a name nothing can bind any more
			// is a fact a reader must not have to infer from a silence.
			detail = "0 behind — no registered version of this name still binds"
		case w.Behind == 0 && w.CurrentVersion != w.PinnedVersion:
			// The pinned version sits ABOVE the binding head: its own version,
			// or every version over it, was retired after this run froze.
			detail = "0 behind — the highest version that still binds is " + current
		case w.Behind == 0:
			detail = "current"
		default:
			detail = fmt.Sprintf("%d version(s) behind — current is %s",
				w.Behind, current)
		}
		line(exec.Render(w.Ref)+":", detail)
	}
}

// writePinEpochs is DKT-594's second section: the run's pin-agreement timeline,
// and which steps' recorded work ran under each agreement.
//
// PRESENT ONLY ON A RUN THAT REPINNED — the engine leaves PinEpochs empty
// otherwise, and a run whose agreement never moved needs no reconciliation: the
// `pins` table already states the bytes every one of its steps read.
//
// The steps ride on the epoch's own line rather than as a `pin_epoch` column on
// every step line. The question this answers is "which side of the repin was
// this step on", which is a partition, and a partition is read by looking at
// the two groups — RUN-39's post-mortem was reconstructing exactly this
// grouping by hand from event seqs and step ids. `--json` carries the inverse
// (each attempt's own `pin_epoch`) for a consumer joining the other way.
func writePinEpochs(
	epochs []engine.PinEpoch, attempts []engine.StepAttempt,
	stepLabel func(engine.StepAttempt) string,
	header func(title string), line func(k, v string),
) {
	if len(epochs) < 2 {
		return
	}
	header("Pin epochs")

	ran := make(map[int][]string, len(epochs))
	for _, a := range attempts {
		if a.PinEpoch > 0 {
			ran[a.PinEpoch] = append(ran[a.PinEpoch], stepLabel(a))
		}
	}

	for _, e := range epochs {
		detail := fmt.Sprintf("%s at seq %d", e.Origin, e.FromSeq)
		if e.FromSeq == 0 {
			// The activation event was pruned away. Saying "at seq 0" would
			// name a sequence number that never existed.
			detail = e.Origin
		}
		for _, c := range e.Changes {
			switch {
			case c.Dropped:
				detail += fmt.Sprintf(" — %s %s dropped (was %s)",
					c.Kind, exec.Render(c.Ref), shortSHA(c.OldSHA256))
			default:
				detail += fmt.Sprintf(" — %s %s %s->%s",
					c.Kind, exec.Render(c.Ref),
					shortSHA(c.OldSHA256), shortSHA(c.NewSHA256))
			}
		}
		if e.Reason != "" {
			detail += " — " + exec.Render(reasonHead(e.Reason, 72))
		}
		line(fmt.Sprintf("Epoch %d:", e.Epoch), detail)
		// A step list per epoch, and NOTHING when an epoch ran no step. An
		// empty list would read as "these steps ran and produced nothing";
		// the absence says the agreement governed no recorded work, which is
		// the ordinary shape of a repin performed to unwedge a run.
		if steps := ran[e.Epoch]; len(steps) > 0 {
			line(fmt.Sprintf("  ran under %d:", e.Epoch), strings.Join(steps, ", "))
		}
	}
}

// stepResolution is the annotation that keeps a step's last recorded routing
// from reading as the last WORD on it (DKT-403).
//
// It fires only where the step's own record cannot already carry the answer:
//
//   - The step must be TERMINAL-BUT-UNANSWERED — `failed-routed` or
//     `waiting-human`. Those are the two statuses whose routing text is a
//     standing question. A `skipped` or `superseded` step recorded its own
//     decision and does not need one attached.
//   - Its routing must not ALREADY name the abandon. The routing path writes
//     `abandon-issue: <note>` on the deciding step and `abandon-issue:
//     cascade: …` on every step it terminated; annotating those would repeat
//     the same fact twice on one line.
//
// What is left is precisely the `run abandon --issue` path, which terminalizes
// remaining steps without touching `routing` — the shape that produced
// RUN-32's report of two parked gates whose parks had in fact been ruled on.
func stepResolution(
	a engine.StepAttempt, dispositions map[string]engine.IssueDisposition,
) string {
	if a.Status != db.StepFailedRouted && a.Status != db.StepWaitingHuman {
		return ""
	}
	if strings.HasPrefix(a.Routing, workflow.OnFailAbandonIssue) {
		return ""
	}
	d, ok := dispositions[a.Issue]
	if !ok {
		return ""
	}
	out := fmt.Sprintf("later resolved: %s %s", exec.Render(d.Issue), d.Disposition)
	if d.By != "" {
		out += " by " + exec.Render(d.By)
	}
	if d.Reason != "" {
		// THE HEAD, not the ruling. A full multi-paragraph rationale on a step
		// line would push the routing it is qualifying off the screen; the
		// section below quotes it at length and `--json` carries all of it.
		// No %q around it: exec.Render IS strconv.Quote, and quoting the
		// quoted string would render an operator's own words as `"\"…\""`.
		out += fmt.Sprintf(" (%s)", exec.Render(reasonHead(d.Reason, 72)))
	}
	return out
}

// reasonHead trims a recorded ruling to one readable clause, marking the trim
// so a reader knows there is more rather than mistaking the head for the whole.
func reasonHead(reason string, max int) string {
	head, rest, cut := strings.Cut(strings.TrimSpace(reason), "\n")
	head = strings.TrimSpace(head)
	// Runes, not bytes: a byte cut lands mid-codepoint and emits a replacement
	// character in the middle of an operator's own words.
	if runes := []rune(head); len(runes) > max {
		return strings.TrimSpace(string(runes[:max])) + "…"
	}
	if cut && strings.TrimSpace(rest) != "" {
		return head + " …"
	}
	return head
}

func writeMetadataRollup(
	rollup []db.MetadataKeyRollup, title string,
	header func(title string), line func(k, v string),
) {
	if len(rollup) == 0 {
		return
	}
	header(title)
	for _, key := range rollup {
		var parts []string
		for _, v := range key.Values {
			parts = append(parts, fmt.Sprintf("%s %d", exec.Render(v.Value), v.Count))
		}
		// The KEY is escaped too. It is a workflow author's opaque string,
		// and core has no list of which keys exist — R7's whole point.
		line(exec.Render(key.Key)+":", strings.Join(parts, ", "))
	}
}

// writeStepMetadata is DKT-868's section: each step's whole bag on ONE LINE,
// beside the status that step ended in.
//
// WHY A SECOND SECTION RATHER THAN A RESHAPED FIRST ONE. The rollup above
// answers a run-level question — which values did this key take, and how often
// — and is the right shape for it. What it cannot answer is which values two
// keys took TOGETHER on one step, because grouping by key is exactly what
// discards the pairing. RUN-51's rollup published a key whose partner key never
// showed the value it resolved to: a real mismatch, on one step, that the
// document could not name. Recovering it meant `docket step show` per step, and
// the audit that found this ran ~90 of them across 19 runs. Deleting the rollup
// to fix that would trade a run-level answer for a per-step one; both questions
// are real, so the document carries both.
//
// THE STATUS RIDES ON THE LINE because the bag's completeness depends on it. A
// dispatcher's `step claim --metadata` (DKT-592) lands before the work runs, and
// a step that then failed or was reaped carries only that half — so a bag with
// a request and no resolution is a FINDING on a failed step and a defect on a
// done one, and a line that did not say which invited the wrong reading. In the
// rollup those two rows were indistinguishable, which is why drift concentrated
// in failures read as no drift at all.
//
// NO KEY IS NAMED HERE (docs/design/genericity.md, R7). The section prints
// whatever keys a bag holds, sorted, and never compares two of them: `a=1, b=2`
// on one line is all core does, and what that pair MEANS is the workflow
// author's business.
func writeStepMetadata(
	attempts []engine.StepAttempt, stepLabel func(engine.StepAttempt) string,
	header func(title string), line func(k, v string),
) {
	var rows []engine.StepAttempt
	for _, a := range attempts {
		if len(a.Metadata) > 0 || a.MetadataUnreadable {
			rows = append(rows, a)
		}
	}
	if len(rows) == 0 {
		return
	}
	header("Step metadata")
	for _, a := range rows {
		detail := a.Status
		if a.MetadataUnreadable {
			// Said out loud rather than skipped. An absent bag reads as "the
			// dispatcher recorded nothing", which is the comfortable claim and
			// the wrong one — the row exists and its bytes do not decode.
			line(stepLabel(a)+":", detail+" — metadata is stored but does not "+
				"decode as an object; `docket step show` has the raw bytes")
			continue
		}
		// Sorted keys, so two renders of one run are byte-identical (R9) — a
		// bare map range would emit a different document per invocation.
		keys := make([]string, 0, len(a.Metadata))
		for k := range a.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			// Both halves are a workflow author's opaque strings on their way to
			// a TERMINAL (R11), and the value goes through db's own renderer so
			// this section and the rollup above spell one value identically.
			parts = append(parts, fmt.Sprintf("%s=%s",
				exec.Render(k), exec.Render(db.RenderMetadataValue(a.Metadata[k]))))
		}
		line(stepLabel(a)+":", detail+" — "+strings.Join(parts, ", "))
	}
}

func writeVerdicts(
	counts []db.VerdictCount, title string,
	header func(title string), line func(k, v string),
) {
	if len(counts) == 0 {
		return
	}
	header(title)
	for _, c := range counts {
		summary := fmt.Sprintf("pass %d, fail %d", c.Pass, c.Fail)
		if c.Unmatched > 0 {
			// `unmatched` is reported SEPARATELY from `fail` and only when it
			// happened: a command that was refused for want of a trust entry is
			// a different fact from one that ran and failed, and collapsing the
			// two would hide a trust problem inside a test failure.
			summary += fmt.Sprintf(", unmatched %d", c.Unmatched)
		}
		// `skipped` is reported for the same reason and with more urgency
		// (DKT-254). It counts rows that MEASURED NOTHING — the tree was gone,
		// or could not be bound — and before it had a column those rows landed
		// in none of the others and disappeared from this section entirely.
		// A verdict meaning "no evidence was collected" rendering as an absence
		// reads as green, which inverts it.
		if c.Skipped > 0 {
			summary += fmt.Sprintf(", skipped %d (measured nothing)", c.Skipped)
		}
		// STUB IS REPORTED WHERE THE PASS IS (DKT-265). A reader who sees
		// `secret-scan: pass 1` reasonably concludes a secret scan happened;
		// the only place that conclusion can be intercepted is beside the
		// number that invites it.
		//
		// The wording distinguishes the two cases the reviewer cares about.
		// "all stubs" means this gate's whole assurance is hollow — nothing it
		// reports was measured by anything. A partial count means some rows ran
		// a real command and some did not, which is the more confusing state
		// and so the one that gets an explicit ratio.
		if c.Stub > 0 {
			total := c.Pass + c.Fail + c.Unmatched
			if c.Stub >= total {
				summary += " — all stubs, nothing was measured"
			} else {
				summary += fmt.Sprintf(" — %d of %d ran a stub", c.Stub, total)
			}
		}
		// THE PRE MARKER RIDES ON THE NAME (DKT-862), because a pre-gate's
		// numbers are not the same KIND of fact as a blocking gate's: §11.1
		// runs it at claim as an input to the step, and PG4 keeps it out of the
		// saga's verdict, so `fail 1` here did not route anything. This section
		// rendered the two identically, and on RUN-61 a conductor reading it
		// nearly reported a fix round as burned on an advisory failure.
		//
		// `[pre]` is `step show`'s spelling, off the same `pre` column, so an
		// operator moving between the two surfaces reads one marker and not
		// two vocabularies for one fact.
		name := exec.Render(c.Name)
		// The four verdicts partition, so their sum is the row count `Pre`
		// overlaps — INCLUDING `skipped`, which pre-gates record whenever the
		// tree could not be bound and which is therefore the most common
		// pre-gate row of all.
		if total := c.Pass + c.Fail + c.Unmatched + c.Skipped; c.Pre > 0 {
			if c.Pre >= total {
				name += " [pre]"
			} else {
				// A name that ran BOTH ways in one run — the same gate declared
				// `pre` by one workflow and blocking by another — cannot take
				// the marker without claiming the whole tally was advisory. The
				// ratio says which part was.
				summary += fmt.Sprintf(
					" — %d of %d ran as a pre-gate (advisory, routed nothing)",
					c.Pre, total)
			}
		}
		line(name+":", summary)
	}
}

// formatDurationMS renders a wall clock in the largest unit that keeps it
// readable, without pulling in a duration parser for a display string.
func formatDurationMS(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%.1fm", float64(ms)/60_000)
	default:
		return fmt.Sprintf("%.1fh", float64(ms)/3_600_000)
	}
}

func init() {
	runCmd.AddCommand(runReportCmd)
}
