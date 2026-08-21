package render

import (
	"fmt"
	"strings"
	"time"
)

// StepGateRow is the renderer's view of one recorded gate execution, mirroring
// StepArtifactRow's role: the CLI converts from its own wire shape so this
// package stays independent of internal/cli and internal/db.
type StepGateRow struct {
	Gate       string
	Ordinal    int
	Verdict    string
	Exit       *int
	DurationMS int64
	Truncated  bool
	Pre        bool
	Stub       bool
	// StubEntry: the trust entry that authorized this gate declared itself a
	// placeholder (DKT-265). Distinct from Stub, which marks a row migrated
	// from an S3 `gate_trail`.
	StubEntry  bool
	Reason     string
	OutputTail string
}

// gateOutputTailLines bounds how much captured output a non-pass row prints.
// The full output rides in `--json --full` (or `--json --gate NAME`); human
// mode shows enough to diagnose without flooding the terminal with a build log.
const gateOutputTailLines = 10

// RenderStepGates renders `step gates` for human mode.
//
// Passing rows print as table lines only; a fail or unmatched row additionally
// prints its reason and the tail of its captured output, since those two
// verdicts are exactly the ones an operator is here to diagnose.
func RenderStepGates(step string, rows []StepGateRow) string {
	if len(rows) == 0 {
		return EmptyState(
			fmt.Sprintf("%s has no recorded gate results.", step),
			"Gates record when they execute; a step whose gates have not run yet "+
				"records nothing. To see the step itself: docket step show "+step,
			false,
		)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%-24s %3s  %-11s %5s %10s  %s\n",
		"GATE", "ORD", "VERDICT", "EXIT", "DURATION", "FLAGS")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 66))

	var failed []StepGateRow
	for _, r := range rows {
		// NULL exit means no process existed (an unmatched gate); "-" keeps
		// that visibly distinct from an exit code of 0.
		exit := "-"
		if r.Exit != nil {
			exit = fmt.Sprintf("%d", *r.Exit)
		}
		var flags []string
		if r.Pre {
			flags = append(flags, "pre")
		}
		if r.Truncated {
			flags = append(flags, "truncated")
		}
		// ONE WORD PER CONCEPT, and `stub` belongs to the one an operator
		// typed. `--stub` on the trust entry, `stub = true` in the trust file,
		// and `stub` in this column are the same fact travelling; a reviewer
		// scanning FLAGS for hollow assurance finds the word they granted.
		//
		// The S3-migration marker gets its own honest name rather than sharing
		// this one. It says WHICH ERA produced the row, which is a different
		// question from what ran, and two facts under one word is how a
		// `secret-scan: pass` came to be unreadable in the first place.
		if r.StubEntry {
			flags = append(flags, "stub")
		}
		if r.Stub {
			flags = append(flags, "s3-migrated")
		}
		fmt.Fprintf(&b, "%-24s %3d  %-11s %5s %10s  %s\n",
			truncate(r.Gate, 24), r.Ordinal, r.Verdict, exit,
			(time.Duration(r.DurationMS) * time.Millisecond).String(),
			strings.Join(flags, ","))
		if r.Verdict != "pass" {
			failed = append(failed, r)
		}
	}

	for _, r := range failed {
		fmt.Fprintf(&b, "\n%s #%d (%s)", r.Gate, r.Ordinal, r.Verdict)
		if r.Reason != "" {
			fmt.Fprintf(&b, " — %s", r.Reason)
		}
		fmt.Fprintln(&b)
		if r.OutputTail != "" {
			for _, line := range strings.Split(r.OutputTail, "\n") {
				fmt.Fprintf(&b, "  %s\n", line)
			}
		}
	}

	return b.String()
}

// GateOutputTail returns the last gateOutputTailLines lines of a gate's
// captured output, with a marker when earlier lines were dropped.
func GateOutputTail(output string) string {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= gateOutputTailLines {
		return output
	}
	dropped := len(lines) - gateOutputTailLines
	// The marker names the flag that ACTUALLY carries the rest. Bare --json
	// summarizes too since DKT-425, so pointing a reader at it would send them
	// to a second tail rather than the log they asked for.
	tail := append([]string{fmt.Sprintf("... (%d earlier line(s); full output in --json --full)", dropped)},
		lines[dropped:]...)
	return strings.Join(tail, "\n")
}
