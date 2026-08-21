package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The activation trust report (TDD docs/tdd/gates-trust.md §7.7, threat T16).
//
// §2, verbatim: "At plan approval the session surfaces what activation will
// bind — including every harvested fenced command, verbatim — so what the human
// approves is what was actually read, not a summary."
//
// T16 is the LIVE-repo case rather than the clone: an attacker who can file an
// issue writes a fenced command into its body. The gate still needs a matching
// trust entry, so filing the issue grants nothing — but the operator should see
// an `unmatched` command BEFORE the run rather than discover it afterwards.
//
// IT IS A REPORT, NOT A GATE (S3). Activation SUCCEEDS with unmatched commands:
// they simply will not run, and their gates route per `on_fail` when reached.
// Refusing activation would let an issue author block a run by adding an
// untrusted fence line — the same denial of service PG2 avoids for pre-gates.

// BuildFenceReport resolves every harvested fence command against the trust
// store, for the run's bound issues.
//
// The trust store is read ONCE for the whole report, the same snapshot
// discipline §7.2 M1 applies at the gate: a report assembled from several reads
// could disagree with itself mid-render.
func BuildFenceReport(
	conn *sql.DB, runID int, loadStore func() (*trust.Store, error), identityPath string,
) ([]FenceReport, error) {
	rows, err := conn.Query(
		`SELECT f.issue_id, f.tag, f.ordinal, f.command
		   FROM run_fences f WHERE f.run_id = ?
		  ORDER BY f.issue_id, f.tag, f.ordinal`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading harvested commands: %w", err)
	}
	defer rows.Close()

	type harvested struct {
		issueID int
		tag     string
		ordinal int
		command string
	}
	var all []harvested
	for rows.Next() {
		var h harvested
		if err := rows.Scan(&h.issueID, &h.tag, &h.ordinal, &h.command); err != nil {
			return nil, fmt.Errorf("reading a harvested command: %w", err)
		}
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	// Which gate references which tag, so the report can name the gate an
	// operator will see in the run rather than only the tag.
	gateByTag, err := gateNamesByTag(conn, runID)
	if err != nil {
		return nil, err
	}

	store, storeErr := loadStore()
	identity, identityErr := trust.RepoIdentity(identityPath)

	out := make([]FenceReport, 0, len(all))
	for _, h := range all {
		report := FenceReport{
			Issue:   model.FormatID(h.issueID),
			Gate:    gateByTag[h.tag],
			Tag:     h.tag,
			Ordinal: h.ordinal,
			Command: h.command,
		}

		switch {
		case storeErr != nil:
			report.Reason = fmt.Sprintf("the trust store could not be read: %v", storeErr)
		case identityErr != nil:
			report.Reason = fmt.Sprintf("the repo path could not be resolved: %v", identityErr)
		default:
			report.Matched, report.Entry, report.Reason =
				matchFenceCommand(store, identity, report.Gate, h.command)
		}
		out = append(out, report)
	}
	return out, nil
}

// matchFenceCommand resolves one command exactly as the runner will.
//
// It tokenizes with the same splitter (§5.2 K2) and matches with the same
// matcher (§7.2), so the report cannot say `matched` for something the gate
// will refuse. A report that used a looser check than the runner would be worse
// than no report: it would tell an operator a command is approved when it is
// not, which is the opposite of what T16 asks for.
func matchFenceCommand(
	store *trust.Store, identity, gate, command string,
) (matched bool, entry, reason string) {
	argv, err := exec.Split(command)
	if err != nil {
		return false, "", fmt.Sprintf(
			"the command could not be read as an argv: %v", err)
	}
	if gate == "" {
		return false, "", "no gate in this run harvests this block's tag"
	}
	m := store.Lookup(identity, gate, argv)
	if !m.Matched {
		return false, "", m.Reason
	}
	return true, m.Entry.Name, ""
}

// gateNamesByTag maps a fence tag to the gate that harvests it, across the
// run's pinned workflow definitions.
func gateNamesByTag(conn *sql.DB, runID int) (map[string]string, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, def := range defs {
		if def == nil {
			continue
		}
		for _, step := range def.Steps {
			for _, gate := range step.Gates {
				if tag, ok := fenceTag(gate); ok {
					out[tag] = gate.Name
				}
			}
		}
	}
	return out, nil
}

// fenceTag extracts `<tag>` from a `fence:<tag>` source.
func fenceTag(gate workflow.Gate) (string, bool) {
	const prefix = "fence:"
	if len(gate.Source) > len(prefix) && gate.Source[:len(prefix)] == prefix {
		return gate.Source[len(prefix):], true
	}
	return "", false
}

// RenderFenceReport writes §7.7 S1's human-mode report.
//
// "VERBATIM" MEANS EVERY BYTE IS ACCOUNTED FOR, NOT THAT RAW BYTES REACH THE
// TERMINAL (T18, §5.7). The command and the reason render through the escaping
// renderer, which is LOSSLESS: a command containing no control bytes renders
// identically to its stored form, and one that does renders its escapes
// visibly rather than executing them against the operator's cursor. D14's whole
// backstop is that what is displayed is what is approved, so any divergence
// between the two is an attack on the ratified control, not a cosmetic bug.
func RenderFenceReport(w interface{ Write([]byte) (int, error) }, reports []FenceReport) {
	if len(reports) == 0 {
		return
	}
	fmt.Fprintf(w, "\nharvested commands (%d):\n", len(reports))
	for _, r := range reports {
		status := "unmatched"
		if r.Matched {
			status = "matched"
			if r.Entry != "" {
				status = "matched: " + r.Entry
			}
		}
		fmt.Fprintf(w, "  [%s] %s %s  (%s)\n",
			r.Issue, r.Gate, exec.Render(r.Command), status)
		if r.Reason != "" {
			fmt.Fprintf(w, "      %s\n", exec.Render(r.Reason))
		}
	}
}

// buildFenceReportTx is BuildFenceReport against an open transaction, for the
// dry run — whose harvested rows exist only inside the transaction it is about
// to discard, and so cannot be read through the pool.
func buildFenceReportTx(tx *sql.Tx, runID int) ([]FenceReport, error) {
	rows, err := tx.Query(
		`SELECT issue_id, tag, ordinal, command FROM run_fences
		  WHERE run_id = ? ORDER BY issue_id, tag, ordinal`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading harvested commands: %w", err)
	}
	var (
		issueIDs []int
		tags     []string
		ordinals []int
		commands []string
	)
	for rows.Next() {
		var (
			issueID, ordinal int
			tag, command     string
		)
		if err := rows.Scan(&issueID, &tag, &ordinal, &command); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading a harvested command: %w", err)
		}
		issueIDs = append(issueIDs, issueID)
		tags = append(tags, tag)
		ordinals = append(ordinals, ordinal)
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(commands) == 0 {
		return nil, nil
	}

	gateByTag, err := gateNamesByTagTx(tx, runID)
	if err != nil {
		return nil, err
	}

	store, storeErr := trust.Load()
	identity, identityErr := trust.RepoIdentity(resolvePaths().Identity)

	out := make([]FenceReport, 0, len(commands))
	for i := range commands {
		report := FenceReport{
			Issue:   model.FormatID(issueIDs[i]),
			Gate:    gateByTag[tags[i]],
			Tag:     tags[i],
			Ordinal: ordinals[i],
			Command: commands[i],
		}
		switch {
		case storeErr != nil:
			report.Reason = fmt.Sprintf("the trust store could not be read: %v", storeErr)
		case identityErr != nil:
			report.Reason = fmt.Sprintf("the repo path could not be resolved: %v", identityErr)
		default:
			report.Matched, report.Entry, report.Reason =
				matchFenceCommand(store, identity, report.Gate, commands[i])
		}
		out = append(out, report)
	}
	return out, nil
}

// gateNamesByTagTx is gateNamesByTag inside a transaction.
func gateNamesByTagTx(tx *sql.Tx, runID int) (map[string]string, error) {
	defs, err := StepDefinitionsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, def := range defs {
		if def == nil {
			continue
		}
		for _, step := range def.Steps {
			for _, gate := range step.Gates {
				if tag, ok := fenceTag(gate); ok {
					out[tag] = gate.Name
				}
			}
		}
	}
	return out, nil
}
