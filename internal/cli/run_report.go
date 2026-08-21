package cli

import (
	"fmt"
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
				line(stepLabel(a), fmt.Sprintf("%d", a.Attempts))
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
				line(stepLabel(a), detail)
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
			}
			line("Coverage:", detail)
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
func stepLabeler(attempts []engine.StepAttempt) func(engine.StepAttempt) string {
	bare := func(a engine.StepAttempt) string { return exec.Render(a.Instance) + ":" }

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
		return exec.Render(a.Issue+" "+a.Instance) + ":"
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
		line(exec.Render(c.Name)+":", summary)
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
