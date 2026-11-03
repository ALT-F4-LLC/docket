package engine

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The activation trust report.
//
// At plan approval the session surfaces what activation will bind — including
// every harvested fenced command, verbatim — so what the human approves is what
// was actually read, not a summary.
//
// The threat is the LIVE-repo case rather than the clone: an attacker who can
// file an issue writes a fenced command into its body. The gate still needs a
// matching trust entry, so filing the issue grants nothing — but the operator
// should see an `unmatched` command BEFORE the run rather than discover it
// afterwards.
//
// IT IS A REPORT, NOT A GATE. Activation SUCCEEDS with unmatched commands:
// they simply will not run, and their gates route per `on_fail` when reached.
// Refusing activation would let an issue author block a run by adding an
// untrusted fence line — the same denial of service the pre-gate path avoids.

// BuildFenceReport resolves every harvested fence command against the trust
// store, for the run's bound issues.
//
// The trust store is read ONCE for the whole report, the same snapshot
// discipline the runner applies at the gate: a report assembled from several
// reads could disagree with itself mid-render.
func BuildFenceReport(
	conn store.LegacyDB, runID int, loadStore func() (*trust.Store, error), identityPath string,
) ([]FenceReport, error) {
	fences, err := listRunFences(conn, runID)
	if err != nil {
		return nil, err
	}
	if len(fences) == 0 {
		return nil, nil
	}

	// Which gate references which tag, so the report can name the gate an
	// operator will see in the run rather than only the tag.
	gateByTag, err := gateNamesByTag(conn, runID)
	if err != nil {
		return nil, err
	}
	return fenceReports(fences, gateByTag, loadStore, identityPath), nil
}

// listRunFences reads the run's harvested commands through a read snapshot of
// the pool. The snapshot is closed before it returns, so the caller can go on
// reading through the pool.
func listRunFences(conn store.LegacyDB, runID int) ([]store.RunFence, error) {
	snap, err := store.StoreOn(conn).BeginReadSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading harvested commands: %w", err)
	}
	defer snap.Rollback()

	fences, err := snap.ListFencesTx(runID)
	if err != nil {
		return nil, fmt.Errorf("reading harvested commands: %w", err)
	}
	return fences, nil
}

// fenceReports resolves each harvested command against one read of the trust
// store, in the order the fences arrive.
func fenceReports(
	fences []store.RunFence, gateByTag map[string]string,
	loadStore func() (*trust.Store, error), identityPath string,
) []FenceReport {
	trustStore, storeErr := loadStore()
	identity, identityErr := trust.RepoIdentity(identityPath)

	out := make([]FenceReport, 0, len(fences))
	for _, f := range fences {
		report := FenceReport{
			Issue:   model.FormatID(f.IssueID),
			Gate:    gateByTag[f.Tag],
			Tag:     f.Tag,
			Ordinal: f.Ordinal,
			Command: f.Command,
		}

		switch {
		case storeErr != nil:
			report.Reason = fmt.Sprintf("the trust store could not be read: %v", storeErr)
		case identityErr != nil:
			report.Reason = fmt.Sprintf("the repo path could not be resolved: %v", identityErr)
		default:
			report.Matched, report.Entry, report.Reason =
				matchFenceCommand(trustStore, identity, report.Gate, f.Command)
		}
		out = append(out, report)
	}
	return out
}

// matchFenceCommand resolves one command exactly as the runner will.
//
// It tokenizes with the same splitter and matches with the same matcher, so the
// report cannot say `matched` for something the gate will refuse. A report that
// used a looser check than the runner would be worse than no report: it would
// tell an operator a command is approved when it is not, which is the opposite
// of what the report is for.
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
func gateNamesByTag(conn store.LegacyDB, runID int) (map[string]string, error) {
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

// RenderFenceReport writes the human-mode report.
//
// "VERBATIM" MEANS EVERY BYTE IS ACCOUNTED FOR, NOT THAT RAW BYTES REACH THE
// TERMINAL. The command and the reason render through the escaping renderer,
// which is LOSSLESS: a command containing no control bytes renders identically
// to its stored form, and one that does renders its escapes visibly rather than
// executing them against the operator's cursor. The report's whole backstop is
// that what is displayed is what is approved, so any divergence
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
func buildFenceReportTx(tx store.LegacyTx, runID int) ([]FenceReport, error) {
	fences, err := store.TxOn(tx).ListFencesTx(runID)
	if err != nil {
		return nil, fmt.Errorf("reading harvested commands: %w", err)
	}
	if len(fences) == 0 {
		return nil, nil
	}

	gateByTag, err := gateNamesByTagTx(tx, runID)
	if err != nil {
		return nil, err
	}
	return fenceReports(fences, gateByTag, trust.Load, resolvePaths().Identity), nil
}

// gateNamesByTagTx is gateNamesByTag inside a transaction.
func gateNamesByTagTx(tx store.LegacyTx, runID int) (map[string]string, error) {
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
