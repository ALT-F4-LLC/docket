package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runStatusCmd = &cobra.Command{
	Use:   "status [RUN-N]",
	Short: "Show a run, or list runs",
	Long: `Show one run's status and step rollup, or list runs when given no ID.

READ-ONLY. This verb computes effective status and WRITES NOTHING — status
never lies just because nobody ran a scheduling command, and a read that
mutated would make "I only looked at it" untrue.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunStatus(cmd, args, getWriter(cmd))
	},
}

// runStatusResult is one run plus its rollup.
type runStatusResult struct {
	Run    *model.Run          `json:"run"`
	Issues int                 `json:"issues"`
	Steps  []model.StatusCount `json:"steps,omitempty"`
	Pins   []pinJSON           `json:"pins,omitempty"`
	// PinDrift is the unsound subset of Pins, checked against disk (DKT-408).
	// Present only when something drifted, so a sound run's output is
	// byte-identical to before the field existed. It makes the status view
	// state drift UNPROMPTED: a parked run whose pins a corpus install
	// replaced must warn before a conduct session attaches and dispatches
	// anything, not after a wave discovers it from render CONFLICTs.
	PinDrift []engine.PinVerdict `json:"pin_drift,omitempty"`
}

// pinJSON is §11.4's `pins` element: `{path, sha256}` for a file pin, and the
// same shape carrying `name@version` for a workflow pin. `kind` distinguishes
// them so a consumer never has to guess from the ref's shape.
type pinJSON struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// MarshalJSON on the wrapper keeps the run's own v1 shape nested under `run`,
// so a consumer reads one document rather than a run with rollup fields spliced
// into it — the rollup is about the run, not part of it.
func (r runStatusResult) VersionedPayload() any {
	return struct {
		Run      any                 `json:"run"`
		Issues   int                 `json:"issues"`
		Steps    []model.StatusCount `json:"steps,omitempty"`
		Pins     []pinJSON           `json:"pins,omitempty"`
		PinDrift []engine.PinVerdict `json:"pin_drift,omitempty"`
	}{
		Run:      model.VersionedRun{Run: *r.Run},
		Issues:   r.Issues,
		Steps:    r.Steps,
		Pins:     r.Pins,
		PinDrift: r.PinDrift,
	}
}

// runListResult is a Collection (reliability-delta §4.1), so v2 renders
// {items, total, truncated}. Total comes from a COUNT(*) that ignores LIMIT.
type runListResult struct {
	Runs  []*model.Run `json:"runs"`
	Total int          `json:"total"`
	limit int
}

func (r runListResult) CollectionItems() any { return runListPayload{runs: r.Runs} }
func (r runListResult) CollectionTotal() int { return r.Total }
func (r runListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Runs))
}

func runRunStatus(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	activeOnly, _ := cmd.Flags().GetBool("active")
	limit, _ := cmd.Flags().GetInt("limit")

	if len(args) == 1 {
		if activeOnly {
			return cmdErr(
				fmt.Errorf("--active filters a list; it does not apply to a single run"),
				output.ErrValidation)
		}
		return showOneRun(cmd, args[0], w)
	}

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	runs, total, err := db.ListRuns(conn, db.RunListOptions{
		ProjectID:  getProjectID(cmd),
		ActiveOnly: activeOnly, Limit: limit,
	})
	if err != nil {
		return runErr(err)
	}

	result := runListResult{Runs: runs, Total: total, limit: limit}
	var message string
	if !w.JSONMode {
		message = renderRunList(runs)
	}
	w.Success(result, message)
	return nil
}

func showOneRun(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	run, err := db.GetRun(conn, runID)
	if err != nil {
		return runErr(err)
	}

	runIssues, err := db.ListRunIssues(conn, runID)
	if err != nil {
		return runErr(err)
	}
	// EFFECTIVE counts (§6.2), exactly as this verb's contract states — the
	// same computation `step show`/`step list` render, so the rollup and a
	// step read taken at the same moment cannot disagree (DKT-468). The raw
	// GROUP BY this replaced counted a lapsed-lease claim as `claimed` while
	// `step show` reported the same step `ready`.
	steps, err := engine.EffectiveStatusCounts(conn, runID, model.NowMS())
	if err != nil {
		return runErr(err)
	}
	pins, err := db.ListPins(conn, runID)
	if err != nil {
		return runErr(err)
	}

	result := runStatusResult{Run: run, Issues: len(runIssues), Steps: steps}
	for _, p := range pins {
		result.Pins = append(result.Pins, pinJSON{Kind: p.Kind, Ref: p.Ref, SHA256: p.SHA256})
	}

	// The drift check reads DISK on purpose, breaking this verb's usual
	// database-only diet (DKT-408): a parked run whose pins a corpus install
	// replaced must say so in the status a resuming conductor reads FIRST,
	// not after a wave discovers it step by step. Still a read — VerifyPins
	// writes nothing — and still cheap: it hashes the run's pinned files, the
	// same work `verify-pins` does. Skipped for terminal runs, whose pins are
	// history rather than an agreement anything will read again.
	if run.Status == model.RunActive || run.Status == model.RunWaitingHuman {
		drift, err := engine.PinDrift(conn, runID)
		if err != nil {
			return runErr(err)
		}
		result.PinDrift = drift
	}

	var message string
	if !w.JSONMode {
		message = renderRunStatus(result)
	}
	w.Success(result, message)
	return nil
}

func renderRunList(runs []*model.Run) string {
	if len(runs) == 0 {
		return "No runs."
	}
	var b strings.Builder
	for _, run := range runs {
		fmt.Fprintf(&b, "%-10s %-14s", run.Ref(), run.Status)
		if run.Request != "" {
			fmt.Fprintf(&b, " %s", firstLine(run.Request))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderRunStatus(r runStatusResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n", r.Run.Ref(), r.Run.Status)
	if r.Run.Reason != "" {
		fmt.Fprintf(&b, "Reason: %s\n", r.Run.Reason)
	}
	if r.Run.Request != "" {
		fmt.Fprintf(&b, "Request: %s\n", requestSummary(r.Run.Request))
	}
	fmt.Fprintf(&b, "Issues: %d\n", r.Issues)

	if len(r.Steps) > 0 {
		var parts []string
		for _, sc := range r.Steps {
			parts = append(parts, fmt.Sprintf("%s %d", sc.Status, sc.Count))
		}
		fmt.Fprintf(&b, "Steps: %s\n", strings.Join(parts, ", "))
	}

	if len(r.Pins) > 0 {
		b.WriteString("Pins:\n")
		for _, p := range r.Pins {
			fmt.Fprintf(&b, "  %-9s %-40s %s\n", p.Kind, p.Ref, shortSHA(p.SHA256))
		}
	}

	if notice := engine.PinDriftNotice(r.PinDrift, r.Run.Ref()); notice != "" {
		b.WriteString(notice)
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// firstLine keeps a multi-line request from breaking a one-line-per-run table.
// A cut is marked with an ellipsis so a one-line rendering never reads as the
// whole text.
func firstLine(s string) string {
	trimmed := strings.TrimRight(s, "\n")
	if first, _, cut := strings.Cut(trimmed, "\n"); cut {
		return first + " …"
	}
	return trimmed
}

// requestSummary is the detail-view rendering of a stored request: the first
// line, then how many lines were cut and where the full text lives. The stored
// data is complete — only this rendering summarizes — and saying so is what
// keeps a multi-paragraph request from reading as a one-line one.
func requestSummary(s string) string {
	trimmed := strings.TrimRight(s, "\n")
	first, rest, cut := strings.Cut(trimmed, "\n")
	if !cut {
		return trimmed
	}
	more := strings.Count(rest, "\n") + 1
	return fmt.Sprintf("%s … (+%d more lines; --json carries the full text)", first, more)
}

func init() {
	runStatusCmd.Flags().Bool("active", false, "List only runs that are not done or abandoned")
	runStatusCmd.Flags().Int("limit", 50, "Maximum number of results")
	runCmd.AddCommand(runStatusCmd)
}
