package cli

import (
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// stepGateResult is one recorded gate execution on the wire. It mirrors
// db.GateResultRow minus the row id and foreign keys, which name the storage
// rather than the fact.
//
// Exit is a pointer because NULL is meaningful: an unmatched gate never ran,
// so it has no exit code, and 0 would read as success (T11).
type stepGateResult struct {
	Gate       string   `json:"gate"`
	Ordinal    int      `json:"ordinal"`
	Verdict    string   `json:"verdict"`
	Exit       *int     `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	Argv       []string `json:"argv,omitempty"`
	// OutputBytes is how many bytes of capture this row STORES, and it is
	// always present whether or not the body rides along (DKT-425). It is the
	// field that keeps an elided body honest: `output_bytes: 174498` with no
	// `output` key says "there is a log here and you did not ask for it",
	// which an absent body alone cannot say.
	OutputBytes int `json:"output_bytes"`
	// Output is the full stored capture, and it is a POINTER because its
	// absence is a fact about the REQUEST, not about the gate. Under --full or
	// a matching --gate every row carries the key — an empty capture included,
	// as `""` — exactly as it did before DKT-425; outside that scope the key
	// is absent, and absent is distinguishable from "the gate printed
	// nothing" only because it is a different JSON shape rather than a
	// different string value.
	Output *string `json:"output,omitempty"`
	// OutputTail is the last few lines of a NON-PASS row's capture, bounded by
	// gateSummaryTailLines and gateSummaryTailBytes. It exists so a conductor
	// reading a summary can triage a failure without a second command; a
	// passing gate carries none, because a pass's log is the dead weight
	// DKT-425 was opened about (216KB from one step, 80% of it a passing
	// gate's).
	OutputTail string `json:"output_tail,omitempty"`
	Truncated  bool   `json:"truncated"`
	Pre        bool   `json:"pre"`
	Stub       bool   `json:"s3_migrated,omitempty"`
	// StubEntry is NOT omitempty. It is the field a consumer checks to ask
	// "was this assurance real", and an omitted key would make a false answer
	// indistinguishable from a docket too old to have one (DKT-265).
	StubEntry bool `json:"stub"`
	// SameOutputAsOrdinal names an EARLIER attempt of this same gate whose
	// stored capture is byte-identical to this one — a flaky-declared re-run
	// that re-emitted the same log. It is a pointer because ordinal 0 is the
	// ordinary first attempt and `omitempty` would erase the commonest answer.
	//
	// It marks the repetition; it does not merge the rows. Each attempt keeps
	// its own ordinal, verdict, exit, duration, and byte count, because "the
	// second attempt failed the same way" and "the gate ran once" are
	// different facts.
	SameOutputAsOrdinal *int   `json:"same_output_as_ordinal,omitempty"`
	Reason              string `json:"reason,omitempty"`
	CreatedAtMS         int64  `json:"created_at_ms"`
}

// stepGatesResult is the listing payload, shaped like stepArtifactsResult:
// the rows ride under a named key so the envelope can grow a sibling later.
type stepGatesResult struct {
	Step  string           `json:"step"`
	Gates []stepGateResult `json:"gates"`
}

// gateSummaryTailLines and gateSummaryTailBytes bound the tail a non-pass row
// carries in the default JSON summary.
//
// The line count matches human mode's tail so both channels show the same
// amount of a failure. The byte cap is the half human mode does not need: a
// terminal reader stops at ten lines of any width, whereas ten lines of a
// minified bundle or a base64 blob is a payload again.
const (
	gateSummaryTailLines = 10
	gateSummaryTailBytes = 2048
)

var stepGatesCmd = &cobra.Command{
	Use:   "gates STEP-N",
	Short: "List a step's recorded gate results",
	Long: `List every recorded gate execution for a step: verdict, exit code,
duration, captured output, and the reason behind an unmatched verdict or a
timeout.

READ-ONLY, and it WRITES NOTHING — no reap, no lease touch.

This is the diagnostics surface for a gate that parked a step: the row was
recorded at execution time, and before this verb existed reading it meant
re-running the gate out-of-band or opening the database by hand. Flaky-declared
re-runs are recorded individually, so one gate may list several ordinals — the
newest is the one the saga acted on. An attempt whose captured output is
byte-identical to an earlier attempt of the same gate says so in
` + "`same_output_as_ordinal`" + ` and drops the repeat instead of carrying it twice.

OUTPUT IS SUMMARIZED BY DEFAULT (DKT-425). Every row reports its verdict, exit,
duration and flags on both channels, and its stored size as ` + "`output_bytes`" + ` in
--json. BODIES are what the summary rations, because a PASSING gate's log is
the largest thing a step stores and the least useful thing to read.

  human    the table, plus the reason and the last few lines of output for
           every row that did not pass
  --json   the same facts, with ` + "`output_tail`" + ` on the rows that did not pass
  --full   --json with every row's complete ` + "`output`" + `, passing rows included
  --gate   --json with the complete ` + "`output`" + ` of the named gate only; the
           other rows stay summarized (repeatable, exact name match)

--full and --gate shape the --json payload; they do not change the human table.
Combining them is allowed and --full wins, since it already includes every row.
A --gate naming no recorded row widens nothing — the listing still names every
gate that ran, so a typo shows up as a body that did not appear next to the
` + "`output_bytes`" + ` that says one exists.

A step whose gates have not run lists nothing and exits 0. A step that does
not EXIST is NOT_FOUND, because "no gate results" would otherwise read as a
fact about a step that is not there.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStepGates(cmd, args, getWriter(cmd))
	},
}

func runStepGates(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := stepArg(args[0])
	if err != nil {
		return err
	}

	// Existence first, so a typoed id is NOT_FOUND instead of an empty
	// listing. LoadStepView is the same read `step show` performs.
	if _, err := engine.LoadStepView(conn, id, model.NowMS()); err != nil {
		return stepErr(err, stepLabel(id))
	}

	rows, err := db.GateResultsForStep(conn, id)
	if err != nil {
		return stepErr(err, stepLabel(id))
	}

	scope := gateOutputScopeOf(cmd)
	result := buildStepGatesResult(model.FormatStepID(id), rows, scope)

	var message string
	if !w.JSONMode {
		message = render.RenderStepGates(result.Step, gateRows(rows))
		// A flag that silently does nothing is worse than one that is not
		// offered, and these two shape a wire format the human table does not
		// use. The hint rides the human channel only: the JSON payload is a
		// wire format and must not grow prose (per `next`).
		if scope.requested() {
			message += stepGatesBodyFlagHint
		}
	}
	w.Success(result, message)
	return nil
}

// stepGatesBodyFlagHint names where --full and --gate actually land, for a
// reader who typed one without --json and would otherwise read the unchanged
// table as the flag having no effect.
const stepGatesBodyFlagHint = "\n--full and --gate widen the --json payload. " +
	"This table already shows the tail of every row that did not pass; " +
	"add --json to read a body in full.\n"

// gateOutputScope decides which rows carry their complete stored capture.
//
// The zero value is the default summary: no row carries a body.
type gateOutputScope struct {
	full  bool
	gates map[string]bool
}

// gateOutputScopeOf reads the scope off the command's flags. A command built
// without them — a test wiring a bare cobra.Command — reads as the default
// summary, which is the honest answer for a caller that asked for nothing.
func gateOutputScopeOf(cmd *cobra.Command) gateOutputScope {
	scope := gateOutputScope{}
	scope.full, _ = cmd.Flags().GetBool("full")
	names, _ := cmd.Flags().GetStringSlice("gate")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if scope.gates == nil {
			scope.gates = map[string]bool{}
		}
		scope.gates[name] = true
	}
	return scope
}

// includes reports whether this gate's rows carry their full body.
func (s gateOutputScope) includes(gate string) bool {
	return s.full || s.gates[gate]
}

// requested reports whether the caller asked for any body at all.
func (s gateOutputScope) requested() bool {
	return s.full || len(s.gates) > 0
}

// buildStepGatesResult shapes recorded rows into the wire payload under scope.
//
// It is a pure function of (rows, scope) so the payload can be asserted on
// directly: what this verb RETURNS by default is the whole of DKT-425, and a
// test of it has to be able to weigh the answer.
func buildStepGatesResult(step string, rows []db.GateResultRow, scope gateOutputScope) stepGatesResult {
	result := stepGatesResult{
		Step:  step,
		Gates: make([]stepGateResult, 0, len(rows)),
	}

	// The first ordinal that stored each distinct (gate, capture). Rows arrive
	// in insertion order and ordinals ascend per gate, so the first sighting is
	// always the earlier attempt.
	firstOrdinal := map[string]int{}

	for _, r := range rows {
		row := stepGateResult{
			Gate: r.Gate, Ordinal: r.Ordinal, Verdict: r.Verdict,
			Exit: r.Exit, DurationMS: r.DurationMS, Argv: r.Argv,
			OutputBytes: len(r.Output), Truncated: r.Truncated, Pre: r.Pre,
			Stub: r.Stub, StubEntry: r.StubEntry,
			Reason: r.Reason, CreatedAtMS: r.CreatedAtMS,
		}

		if r.Output != "" {
			key := r.Gate + "\x00" + r.Output
			if first, seen := firstOrdinal[key]; seen {
				earlier := first
				row.SameOutputAsOrdinal = &earlier
			} else {
				firstOrdinal[key] = r.Ordinal
			}
		}

		switch {
		case scope.includes(r.Gate):
			// The caller asked for this gate's body: every row of it carries
			// the complete capture, repeats included. `--full` means full.
			body := r.Output
			row.Output = &body
		case r.Verdict == db.GateVerdictPass:
			// A pass carries no body at all. Its verdict, exit, and size are
			// the whole of what it has to say.
		case row.SameOutputAsOrdinal != nil:
			// An identical repeat points at the attempt that already showed
			// the tail rather than printing it again.
		default:
			row.OutputTail = gateSummaryTail(r.Output)
		}

		result.Gates = append(result.Gates, row)
	}

	return result
}

// gateSummaryTail returns the end of a capture, bounded by both line count and
// bytes, with no marker inside the string.
//
// The human tail's marker ("full output in --json") is prose for a terminal
// and would be a lie inside the JSON it points at. A JSON reader compares
// `output_tail` against `output_bytes` to see how much was left behind, and
// asks for the rest with --gate.
func gateSummaryTail(out string) string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > gateSummaryTailLines {
		lines = lines[len(lines)-gateSummaryTailLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > gateSummaryTailBytes {
		// Cutting on a byte boundary can sever a multi-byte rune. Dropping the
		// severed head keeps the string valid UTF-8 rather than shipping a
		// replacement character a reader would mistake for gate output.
		tail = strings.ToValidUTF8(tail[len(tail)-gateSummaryTailBytes:], "")
	}
	return tail
}

// gateRows converts db rows into the renderer's shape, per artifactRows: the
// conversion lives here so internal/render keeps its independence from
// internal/db.
func gateRows(rows []db.GateResultRow) []render.StepGateRow {
	out := make([]render.StepGateRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, render.StepGateRow{
			Gate: r.Gate, Ordinal: r.Ordinal, Verdict: r.Verdict,
			Exit: r.Exit, DurationMS: r.DurationMS, Truncated: r.Truncated,
			Pre: r.Pre, Stub: r.Stub, StubEntry: r.StubEntry, Reason: r.Reason,
			OutputTail: render.GateOutputTail(r.Output),
		})
	}
	return out
}

func init() {
	stepGatesCmd.Flags().Bool("full", false,
		"Include every gate's complete captured output in --json")
	stepGatesCmd.Flags().StringSlice("gate", nil,
		"Include this gate's complete captured output in --json (repeatable)")
	stepCmd.AddCommand(stepGatesCmd)
}
