package engine

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The activation gate preflight (DKT-255).
//
// The fence report (fence_report.go) already answers "will this harvested
// command run" before the run starts. NOTHING ASKED THE SAME QUESTION OF A
// DECLARED GATE, so the gap was discovered mid-run, one gate at a time, when
// the gate fired and found nothing to run.
//
// Every one of the epoch's 34 gate-unmatched events was a missing-entry case —
// not a moved repo, not argv drift, not a prefix mismatch. Every one was
// knowable before the run started. Nine unmatched gates across the 2026-08-17
// fleet alone, each either pausing a run 2-7 minutes while an operator added
// the entry or silently skipping the AC commands the workflow declared; RUN-3's
// missing entries surfaced only because a review panel REJECTED the vote.
//
// IT WARNS, IT DOES NOT BLOCK — the issue's own stance, and the same one the
// fence report takes for the same reason. Some gates are legitimately absent on
// some machines, and activation is not the place to make that a hard stop. What
// the warning buys is that "you are about to run a workflow with 3 gates that
// cannot execute here" is a fact an operator can act on in seconds, instead of
// 20 minutes in.
//
// FENCE GATES ARE EXCLUDED. Their commands come from issue bodies and are
// resolved by the fence report against the exact argv harvested; a named gate
// has no argv until the store is consulted, and the question here is only
// whether an ENTRY OF THAT NAME EXISTS. Reporting a fence gate in both places
// would double-count it and, worse, report it here as unmatched whenever its
// body carried no fenced block.

// GatePreflight is one declared gate and whether this machine can run it.
type GatePreflight struct {
	// Gate is the gate name as the workflow declares it.
	Gate string `json:"gate"`
	// Workflows names every bound workflow declaring this gate, so an operator
	// adding one entry knows what it unblocks. Sorted, so two activations of
	// the same run render identically.
	Workflows []string `json:"workflows"`
	// Matched reports that a trust entry of this name resolves for this repo.
	Matched bool `json:"matched"`
	// Entry names the resolving entry, when one did.
	Entry string `json:"entry,omitempty"`
	// Stub reports that the resolving entry declared itself a placeholder
	// (DKT-265). A gate that WILL run and will measure nothing is a different
	// answer from one that will not run, and both are different from a real
	// check — an operator reading a green preflight should not have to open the
	// trust store to learn which they have.
	Stub bool `json:"stub,omitempty"`
	// Reason explains an unmatched gate, verbatim from the matcher, so the
	// preflight and the mid-run diagnostic say the same thing.
	Reason string `json:"reason,omitempty"`
}

// BuildGatePreflight resolves every gate the run's bound workflows declare
// against the trust store.
//
// The store is read ONCE for the whole report, the same snapshot discipline
// §7.2 M1 applies at the gate and the fence report applies to itself: a report
// assembled from several reads could disagree with itself mid-render.
//
// A gate is looked up BY NAME WITH A NIL ARGV, which is exactly what
// gate_exec.go's pre-match does — so a gate this reports as matched is a gate
// whose name resolves for this repo. It deliberately does not promise more: the
// argv check happens at spawn against the entry's own argv, and a preflight
// that pretended to settle it would tell an operator a gate is ready when it is
// not, which is worse than saying nothing.
func BuildGatePreflight(
	defs map[int]*workflow.Definition,
	loadStore func() (*trust.Store, error), identityPath string,
) ([]GatePreflight, error) {
	// Gate name -> the workflows declaring it. A map because one gate is
	// commonly declared by several steps of several workflows, and an operator
	// wants one row per MISSING ENTRY, not one per declaration site.
	declaring := map[string]map[string]bool{}
	for _, def := range defs {
		if def == nil {
			continue
		}
		label := fmt.Sprintf("%s@%d", def.Pipeline.Name, def.Pipeline.Version)
		for _, step := range def.Steps {
			for _, gate := range step.Gates {
				if _, isFence := fenceTag(gate); isFence {
					continue
				}
				if gate.Name == "" {
					continue
				}
				if declaring[gate.Name] == nil {
					declaring[gate.Name] = map[string]bool{}
				}
				declaring[gate.Name][label] = true
			}
		}
	}
	if len(declaring) == 0 {
		return nil, nil
	}

	store, storeErr := loadStore()
	identity, identityErr := trust.RepoIdentity(identityPath)

	names := make([]string, 0, len(declaring))
	for name := range declaring {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]GatePreflight, 0, len(names))
	for _, name := range names {
		row := GatePreflight{Gate: name, Workflows: sortedKeys(declaring[name])}
		switch {
		case storeErr != nil:
			row.Reason = fmt.Sprintf("the trust store could not be read: %v", storeErr)
		case identityErr != nil:
			row.Reason = fmt.Sprintf("the repo path could not be resolved: %v", identityErr)
		default:
			m := store.Lookup(identity, name, nil)
			if m.Matched {
				row.Matched = true
				if m.Entry != nil {
					row.Entry, row.Stub = m.Entry.Name, m.Entry.Stub
				}
			} else {
				row.Reason = m.Reason
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RenderGatePreflight writes the human-mode warning, and writes NOTHING when
// every gate resolves.
//
// Silence on success is the point. An activation already prints a bound-issue
// roster, a pin count, a fence report, and any scope warnings; a fifth block
// saying "all 6 gates are fine" on every run is how an operator learns to skip
// the region of the screen where the one that is not fine will appear.
//
// A STUBBED gate is listed too, under its own heading. It resolves and will
// run, so it is not a warning — but "secret-scan will execute /usr/bin/true"
// is a fact about this run's assurance that belongs beside the roster the
// operator is approving (DKT-265).
func RenderGatePreflight(w interface{ Write([]byte) (int, error) }, rows []GatePreflight) {
	var missing, stubs []GatePreflight
	for _, r := range rows {
		switch {
		case !r.Matched:
			missing = append(missing, r)
		case r.Stub:
			stubs = append(stubs, r)
		}
	}

	if len(missing) > 0 {
		fmt.Fprintf(w,
			"\nwarning: %d declared gate(s) have no trust entry here and will not run:\n",
			len(missing))
		for _, r := range missing {
			fmt.Fprintf(w, "  %s  (declared by %s)\n",
				exec.Render(r.Gate), exec.Render(joinList(r.Workflows)))
			if r.Reason != "" {
				fmt.Fprintf(w, "      %s\n", exec.Render(r.Reason))
			}
		}
		fmt.Fprintf(w,
			"  Add them with `docket trust add <name> -- <argv>`, or proceed: "+
				"an unmatched gate records `unmatched` and routes per its step's on_fail.\n")
	}

	if len(stubs) > 0 {
		fmt.Fprintf(w,
			"\nnote: %d declared gate(s) resolve to a STUB entry and will measure nothing:\n",
			len(stubs))
		for _, r := range stubs {
			fmt.Fprintf(w, "  %s  (entry %s)\n",
				exec.Render(r.Gate), exec.Render(r.Entry))
		}
	}
}

// joinList renders a name list for one line of human output.
func joinList(names []string) string {
	switch len(names) {
	case 0:
		return "no workflow"
	case 1:
		return names[0]
	default:
		out := names[0]
		for _, n := range names[1:] {
			out += ", " + n
		}
		return out
	}
}

// buildGatePreflightTx is BuildGatePreflight against an open transaction, for
// the dry run — whose pinned definitions exist only inside the transaction it
// is about to discard, and so cannot be read through the pool.
func buildGatePreflightTx(tx *sql.Tx, runID int) ([]GatePreflight, error) {
	defs, err := StepDefinitionsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	return BuildGatePreflight(defs, trust.Load, resolvePaths().Identity)
}

// HoldPolicy is who will answer a hold this run mints (DKT-266).
//
// A hold is the one step in a run no author declared — the engine mints it when
// a `hold_spread` trips — so who answers it is not visible anywhere a workflow
// author or a run's operator normally looks. It is `vote.hold.rule` plus
// `vote.hold.voters` in engine config, and if BOTH are set the hold is minted
// as a vote step; otherwise it is minted for one operator.
//
// The audit that produced this issue found 52 step-held events, 50 resolved by
// an operator directly, and ZERO hold-panel votes, against config that reads
// `set` in both projects. The engine was doing exactly what it was told — the
// config postdates those runs, and TestHeldStepMintsVoteWhenConfigured has
// always proven the path. What was missing is that NOTHING SAYS WHICH POLICY IS
// IN FORCE, so a configured surface that is inert and one that is broken look
// identical, and answering "which is it" took a source audit.
type HoldPolicy struct {
	// Rule is the configured `vote.hold.rule`, empty when unset.
	Rule string `json:"rule,omitempty"`
	// Voters is the configured `vote.hold.voters`, empty when unset.
	Voters []string `json:"voters,omitempty"`
	// Panel reports the effective answer: true when a hold this run mints will
	// be decided by a tally, false when it goes to one operator.
	//
	// It is a DERIVED field rather than something a reader recomputes from the
	// two above, because the derivation is the thing that was invisible: both
	// keys must be non-empty, and a half-configured pair silently means "one
	// operator" — which is the state an operator who set only one of them would
	// least expect and least easily notice.
	Panel bool `json:"panel"`
}

// LoadHoldPolicy reads the run's project's hold-decision policy.
func LoadHoldPolicy(conn *sql.DB, runID int) (HoldPolicy, error) {
	tally, err := loadHoldTally(conn, runID)
	if err != nil {
		return HoldPolicy{}, err
	}
	return holdPolicyOf(tally), nil
}

func holdPolicyOf(t holdTally) HoldPolicy {
	return HoldPolicy{Rule: t.rule, Voters: t.voters, Panel: t.configured()}
}

// RenderHoldPolicy writes the activation's one-line disclosure.
//
// IT PRINTS ONLY WHEN SOMETHING IS CONFIGURED. A project that has never touched
// these keys gets the default — one operator decides — and saying so on every
// activation would be a line about a feature nobody is using, in a report whose
// unread regions are the problem the gate preflight above is careful about.
//
// A HALF-CONFIGURED PAIR IS THE CASE THIS EXISTS FOR, so it prints loudest: the
// operator has said something and the engine is doing something else, which is
// precisely the silence the issue objects to.
func RenderHoldPolicy(w interface{ Write([]byte) (int, error) }, p HoldPolicy) {
	switch {
	case p.Panel:
		fmt.Fprintf(w, "\nholds on this run go to a panel: %s (%s)\n",
			exec.Render(p.Rule), exec.Render(joinList(p.Voters)))
	case p.Rule == "" && len(p.Voters) == 0:
		// Nothing configured: the default, and not worth a line.
	default:
		fmt.Fprintf(w,
			"\nwarning: the hold policy is HALF configured, so holds on this run "+
				"go to ONE OPERATOR, not to a panel.\n")
		if p.Rule == "" {
			fmt.Fprintf(w, "  vote.hold.voters is set (%s) but vote.hold.rule is not.\n",
				exec.Render(joinList(p.Voters)))
		} else {
			fmt.Fprintf(w, "  vote.hold.rule is set (%s) but vote.hold.voters is not.\n",
				exec.Render(p.Rule))
		}
		fmt.Fprintf(w, "  Both are required; set the missing one with `docket config set`.\n")
	}
}
