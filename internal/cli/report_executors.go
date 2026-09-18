package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// `docket report executors` — the cross-run ledger (DKT-2453).
//
// BUILT ON THE `run report` PATTERN: a flat result struct with JSON tags, one
// engine loader, a styled and a plain renderer sharing one line writer, and
// `w.Success(result, message)`.

var reportExecutorsCmd = &cobra.Command{
	Use:   "executors",
	Short: "Group routings, rulings, reaps, and cluster corroboration per executor hint and voter name across runs",
	Long: `Roll up, across every run in a window, what became of the steps each
executor hint ran and the panels each voter name sat on.

Per EXECUTOR HINT (the opaque ` + "`executor`" + ` a step declared): runs and steps
carrying the hint, steps routed ` + "`fix-loop`" + `, ` + "`step resolve --as override-pass`" + `
rulings, reaps and the subset a relay forced with ` + "`step reap`" + `, and — over
every ` + "`aggregate`" + ` round whose step declares ` + "`source_field`" + ` — the clusters a
step with the hint contributed to, split into unique (one member),
corroborated (more than one), and held.

Per VOTER NAME: runs and casts, casts by verdict, and how many of the vote
steps the name cast on then routed ` + "`fix-loop`" + `, were resolved
` + "`override-pass`" + `, or were held-cluster ballots. A sealed, still-open ballot
is withheld, as it is everywhere.

READ-ONLY AND OPERATOR-FACING. This verb writes nothing, ` + "`next`" + ` never
consults it, and nothing here reaches a seat: routing policy stays outside
the store, and a track record fed back into a panel becomes an incentive to
agree with it. ` + "`reaps`" + `, ` + "`forced_reaps`" + `, and ` + "`override_passes`" + ` read the event
log, so a ruling ` + "`events prune`" + ` removed leaves all three together.

The window is this project's runs, or the whole store with --all-projects;
--since RUN-N keeps runs from that id on, --since DATE (2026-09-01, or an
RFC 3339 timestamp) keeps runs created from that instant on.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReportExecutors(cmd, getWriter(cmd))
	},
}

// reportExecutorsResult is the ledger plus the window it was read over, so a
// consumer of the document knows what "runs" counted without re-deriving the
// invocation.
type reportExecutorsResult struct {
	*engine.ExecutorLedger
	// Scope is `project` or `store`, and Since echoes the flag's canonical
	// form (`RUN-N`, or an RFC 3339 instant) when one was given.
	Scope string `json:"scope"`
	Since string `json:"since,omitempty"`
}

func runReportExecutors(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	sinceRaw, _ := cmd.Flags().GetString("since")
	sinceRun, sinceMS, sinceLabel, err := parseLedgerSince(sinceRaw)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	opts := engine.ExecutorLedgerOptions{SinceRun: sinceRun, SinceMS: sinceMS}
	scope := "store"
	if all, _ := cmd.Flags().GetBool("all-projects"); !all {
		opts.ProjectID = getProjectID(cmd)
		scope = "project"
	}

	ledger, err := engine.LoadExecutorLedger(conn, opts)
	if err != nil {
		return runErr(err)
	}
	result := reportExecutorsResult{ExecutorLedger: ledger, Scope: scope, Since: sinceLabel}

	var message string
	if !w.JSONMode {
		message = renderExecutorLedger(result)
	}
	w.Success(result, message)
	return nil
}

// parseLedgerSince reads --since as either a run id (`RUN-N`, or a bare N)
// or an instant (`2026-09-01`, or RFC 3339), and returns the floor in the
// form the loader takes plus the canonical label the document echoes.
//
// A bare number is a run id and never a year: a retro window is "every run
// since the last retro", which is a run, and a date carries dashes.
func parseLedgerSince(raw string) (sinceRun int, sinceMS int64, label string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, "", nil
	}
	if id, err := model.ParseRunID(raw); err == nil {
		return id, 0, model.FormatRunID(id), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if at, err := time.Parse(layout, raw); err == nil {
			return 0, at.UnixMilli(), at.UTC().Format(time.RFC3339), nil
		}
	}
	return 0, 0, "", fmt.Errorf(
		"--since %q: want a run (RUN-N), a date (2026-09-01), or an RFC 3339 timestamp", raw)
}

// renderExecutorLedger is the human-mode document. Hints and voter names are
// stored, author-supplied strings on their way to a terminal, so they go
// through exec.Render (R11); the column labels are core's own.
func renderExecutorLedger(r reportExecutorsResult) string {
	if !render.ColorsEnabled() {
		return renderPlainExecutorLedger(r)
	}
	section := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	value := lipgloss.NewStyle().Bold(true)

	var b strings.Builder
	writeExecutorLedger(&b, r,
		func(title string) { b.WriteString(section.Render(title) + "\n") },
		func(k, v string) { fmt.Fprintf(&b, "  %s %s\n", label.Render(k), value.Render(v)) })
	return strings.TrimRight(b.String(), "\n")
}

// renderPlainExecutorLedger is the no-color path — the same document, same
// order, no styling.
func renderPlainExecutorLedger(r reportExecutorsResult) string {
	var b strings.Builder
	writeExecutorLedger(&b, r,
		func(title string) { fmt.Fprintf(&b, "%s\n", title) },
		func(k, v string) { fmt.Fprintf(&b, "  %-24s %s\n", k, v) })
	return strings.TrimRight(b.String(), "\n")
}

// writeExecutorLedger emits the sections through the caller's writers, so the
// styled and plain renderers cannot disagree about WHICH facts appear or in
// what order — only about how they look.
func writeExecutorLedger(
	b *strings.Builder, r reportExecutorsResult,
	header func(title string), line func(k, v string),
) {
	header("Window")
	line("Runs:", fmt.Sprintf("%d", r.Runs))
	line("Scope:", r.Scope)
	if r.Since != "" {
		line("Since:", r.Since)
	}

	if len(r.Executors) > 0 {
		b.WriteString("\n")
		header("Executors")
		for _, e := range r.Executors {
			line(exec.Render(e.Executor)+":", fmt.Sprintf(
				"runs %d, steps %d, fix-loop %d, override-pass %d, reaps %d (forced %d), "+
					"clusters unique %d / corroborated %d / held %d",
				e.Runs, e.Steps, e.FixLoopRoutes, e.OverridePasses, e.Reaps, e.ForcedReaps,
				e.UniqueClusters, e.CorroboratedClusters, e.HeldClusters))
		}
	}

	if len(r.Voters) > 0 {
		b.WriteString("\n")
		header("Voters")
		for _, v := range r.Voters {
			line(exec.Render(v.Voter)+":", fmt.Sprintf(
				"runs %d, casts %d (approve %d, approve-with-concerns %d, reject %d), "+
					"fix-loop %d, override-pass %d, held %d",
				v.Runs, v.Casts, v.Approve, v.ApproveWithConcerns, v.Reject,
				v.FixLoopRoutes, v.OverridePasses, v.HeldClusters))
		}
	}

	if len(r.Executors) == 0 && len(r.Voters) == 0 {
		b.WriteString("\n")
		line("Ledger:", "no steps or casts in the window")
	}
}

func init() {
	reportExecutorsCmd.Flags().String("since", "",
		"Keep runs from RUN-N on, or created from a date (2026-09-01) or RFC 3339 timestamp on")
	reportExecutorsCmd.Flags().Bool("all-projects", false,
		"Read every project's runs instead of this project's")
	reportCmd.AddCommand(reportExecutorsCmd)
}
