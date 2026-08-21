package render

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Step rendering — human mode for `next --run` and `step show`.
//
// Every column here is a CORE concept: an id, a rendered instance identity, the
// issue and run it belongs to, an opaque executor hint, an opaque class, a
// retry count, a cost, and a TTL. Nothing names an instance concept, and the
// genericity gate (scripts/qa/genericity.sh) checks these bytes like any other
// core surface (§6.11, docs/design/genericity.md).

// RenderStepRows renders a `next --run` result as a table.
func RenderStepRows(rows []model.StepRow) string {
	if len(rows) == 0 {
		return EmptyState(
			"No ready steps.",
			"Steps become ready when their predecessors finish: docket run status",
			false,
		)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%-10s %-24s %-10s %-9s %-20s %-12s %-8s %s\n",
		"STEP", "INSTANCE", "ISSUE", "KIND", "EXECUTOR", "CLASS", "ATTEMPT", "TTL")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 108))

	for _, row := range rows {
		fmt.Fprintf(&b, "%-10s %-24s %-10s %-9s %-20s %-12s %-8d %s\n",
			row.Step,
			truncate(row.Instance, 24),
			row.Issue,
			row.Kind,
			dashIfEmpty(truncate(row.Executor, 20)),
			dashIfEmpty(truncate(row.Class, 12)),
			row.Attempt,
			fmt.Sprintf("%ds", row.LeaseTTLS),
		)
	}

	return b.String()
}

// RenderStepDetail renders one step for `step show`.
//
// The status shown is the EFFECTIVE one (§6.2) — computed at read, never
// written back — so a step whose lease lapsed reads as pending here even though
// the row still carries the stale owner.
func RenderStepDetail(row model.StepRow, routing, sagaStage string, owner string, expiresMS int64) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n", row.Step, row.Instance)
	fmt.Fprintf(&b, "  status:    %s\n", row.Status)
	// blocked names WHY a pending step is not ready (DKT-470): without it, a
	// step waiting on its next predecessor and a step whose interposed
	// routing was already decided against it forever both render the same
	// bare `pending`, and only reading the event log told the two apart.
	if row.BlockedReason != "" {
		fmt.Fprintf(&b, "  blocked:   %s\n", row.BlockedReason)
	}
	fmt.Fprintf(&b, "  issue:     %s\n", row.Issue)
	fmt.Fprintf(&b, "  run:       %s\n", row.Run)
	fmt.Fprintf(&b, "  kind:      %s\n", row.Kind)
	if row.Executor != "" {
		fmt.Fprintf(&b, "  executor:  %s\n", row.Executor)
	}
	if row.Class != "" {
		fmt.Fprintf(&b, "  class:     %s\n", row.Class)
	}
	// The vote facts, on a vote step and nowhere else. `proposal` is the id
	// every existing vote verb is addressed by, so a reader who sees the gate
	// here can cast on it without deriving anything.
	if len(row.Voters) > 0 {
		fmt.Fprintf(&b, "  voters:    %s\n", strings.Join(row.Voters, ", "))
	}
	if row.Proposal != "" {
		fmt.Fprintf(&b, "  proposal:  %s\n", row.Proposal)
	}
	fmt.Fprintf(&b, "  attempt:   %d\n", row.Attempt)

	// A held lease is reported with its owner, mirroring the v6 `lease` object's
	// rule: a field that is not a fact yet does not appear.
	if owner != "" {
		fmt.Fprintf(&b, "  owner:     %s\n", owner)
		fmt.Fprintf(&b, "  expires:   %d\n", expiresMS)
	}
	if routing != "" {
		fmt.Fprintf(&b, "  routing:   %s\n", routing)
	}
	// A step mid-saga is engine-owned and needs no lease to advance (§6.8);
	// showing the resume point is how an operator sees that rather than
	// wondering why an unclaimed step is not `done`.
	if sagaStage != "" {
		fmt.Fprintf(&b, "  saga:      %s (resumes under any engine invocation)\n", sagaStage)
	}

	return b.String()
}

// RenderStepGateSummary is `step show`'s gate block (DKT-63).
//
// `step show` printed NO GATE SECTION AT ALL, so the surface an operator
// reaches for to ask "why is this step parked" was silent about the gates that
// parked it. gate_results held build, tests, and self-hygiene at exit 1 and the
// verb showed none of them; a conductor reading this verb and the event feed on
// 2026-08-16 reported all three failures as passes.
//
// It is a SUMMARY, not `step gates`. That verb owns the full table with reasons
// and output tails, and duplicating it here would make the two drift; what
// `step show` owes is the answer to "did the gates pass", plus the pointer to
// where the detail lives. So each line is a verdict, a name, and an exit code,
// and a non-passing summary names the verb that explains it.
//
// The VERDICT LEADS, because that is the question being asked. A re-run's
// ordinal appears only when there was one — a gate that ran once should not
// have to explain what `#0` means.
func RenderStepGateSummary(step string, rows []StepGateRow) string {
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("  gates:\n")
	failing := 0
	for _, r := range rows {
		name := r.Gate
		if r.Ordinal > 0 {
			name = fmt.Sprintf("%s (re-run %d)", r.Gate, r.Ordinal)
		}
		if r.Pre {
			name += " [pre]"
		}
		fmt.Fprintf(&b, "    %-10s %s", dashIfEmpty(r.Verdict), name)
		// NULL exit means no process existed (an unmatched gate). It stays
		// ABSENT rather than rendering as 0, which would read as a pass — the
		// gate-forgery confusion db.GateResultRow's own comment warns about.
		if r.Exit != nil {
			fmt.Fprintf(&b, "  exit %d", *r.Exit)
		}
		b.WriteString("\n")
		if r.Verdict != "pass" {
			failing++
		}
	}
	if failing > 0 {
		fmt.Fprintf(&b,
			"    %d gate(s) did not pass; reasons and output: docket step gates %s\n",
			failing, step)
	}
	return b.String()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
