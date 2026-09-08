package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// INTEGRATION CHECK (DKT-1284) — `dispatch close` verifies every write-class
// step's own recorded commit actually reached the shared branch, replacing
// dotfiles' src/user/claude_code/workflows/integration-check.js: an agent
// listed `worktree-wf_*` worktrees and one agent per worktree ran `git
// merge-base --is-ancestor` then `git cherry`, and the conductor had to
// remember to launch it and paste its return into the close report — RUN-N
// shipped a close whose shared branch never advanced, found 19 hours later.
//
// The engine already holds every recorded sha (each write-class step's own
// `issue.diff` artifact, `{head, worktree}`) and knows the shared checkout, so
// this reuses the SAME two probes DKT-193's stale-target advisory already
// shells out with (Engine.IsAncestorFn, PatchContainedFn) — a second
// implementation of `git cherry` plumbing would be a second copy of exactly
// this question, and DKT-1033's zero-context patch-id refinement (a
// cherry-picked commit that shares a Makefile hunk with a sibling) already
// lives there.
//
// AC2 IS PATCH-EQUIVALENCE'S WHOLE JOB: the sanctioned integration flow
// cherry-picks a worktree's commit onto the shared branch, minting a NEW sha
// for identical content, so ancestry fails on every legitimately-integrated
// write-class step by construction — the check would refuse every ordinary
// close if it stopped at ancestry.

// UnintegratedStep is one write-class step whose recorded commit did not
// reach the shared branch — dispatch close's refusal, one row per finding
// (AC1).
type UnintegratedStep struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	SHA      string `json:"sha"`
	Worktree string `json:"worktree,omitempty"`
	// How is "unintegrated" (git ran and found no ancestry and no patch
	// match) or "cherry-error" (the patch probe itself could not run —
	// AC1's "a cherry error counts as unintegrated"). "ancestor" and
	// "patch-equivalent" never appear here: either one clears the step.
	How string `json:"how"`
}

// CheckedIntegration is one write-class step's commit and how this close
// accepted it — the audit trail a run report reads (AC3), and the record a
// LATER close of the same run trusts (DKT-1787).
type CheckedIntegration struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	SHA      string `json:"sha"`
	// How is "ancestor" or "patch-equivalent" when this pass asked git,
	// "resolved" when the recorded head is one `step annotate
	// --integrated-sha` verified and re-recorded (annotate_integration.go) and
	// git confirmed its ancestry again here, "skipped" when this close's
	// --skip-integration-check reason vouched for the commit instead — or,
	// with Prior set, the verdict the prior close recorded, carried forward
	// unchanged.
	How string `json:"how"`
	// Prior names the earlier close of this run that accepted this sha —
	// "DISPATCH-N", or "run" for an acceptance recorded with no manifest open
	// — present only when this pass honored that record rather than asking
	// git again. A writer sha that a conflicting cherry-pick was resolved by
	// hand is NEVER an ancestor and never patch-equivalent, so without this
	// every close after the one that accepted it would refuse on it again
	// (RUN-90, four steps, every close for the rest of the run).
	Prior string `json:"prior,omitempty"`
}

// IntegrationCheck is what a close's verification did, riding on CloseOutcome
// and the close event (AC3).
type IntegrationCheck struct {
	// Status is "verified" (every write-class step's commit reached the
	// shared branch) or "skipped" (--skip-integration-check named a reason).
	Status string `json:"status"`
	// Reason is the operator's stated reason for skipping, present only when
	// Status is "skipped".
	Reason string `json:"reason,omitempty"`
	// Checked names every write-class commit this close accepted and how:
	// asked of git this pass, honored from a prior close, or vouched for by
	// this close's skip reason. A skipped close asks git nothing, but still
	// records what its reason covered — that record is what lets the next
	// close honor it (integrationCandidatesTx).
	Checked []CheckedIntegration `json:"checked,omitempty"`
}

// integrationCandidate is one write-class step's own recorded commit. prior is
// the acceptance an earlier close of the run recorded for exactly this sha,
// or nil when git has to be asked.
type integrationCandidate struct {
	step, instance, sha, worktree string
	prior                         *CheckedIntegration
	// resolved marks a record the verified integration annotation wrote
	// (`resolved_from` in its payload): the sha names the commit the shared
	// branch carries for work whose original commit it could not have matched.
	// The check still asks git; the marker only names the verdict.
	resolved bool
}

// integrationCandidatesTx collects one candidate per TERMINAL write-class
// step of the run that recorded a commit — each step's own MOST RECENT
// `issue.diff` artifact (highest artifact id when a retry re-recorded one) —
// and marks each one an earlier close of this run already accepted.
//
// The prior record is the `dispatch-closed` event's own `integration.checked`
// list (what a close writes, verified or skipped), keyed by step AND sha: a
// step that re-recorded a new head after the close that accepted its old one
// is asked of git again.
//
// It runs inside the caller's transaction; the git questions run only after
// it ends (§6: no subprocess inside one) — the same split
// staleTargetCandidates/staleTargets already use for the identical reason.
func integrationCandidatesTx(tx *sql.Tx, sched *Scheduler, runID int) ([]integrationCandidate, error) {
	artifacts, err := db.ListRunArtifactsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	own := make(map[int]*db.Artifact, len(artifacts))
	for _, a := range artifacts {
		if a.Kind != ArtifactKindIssueDiff {
			continue
		}
		if prev, ok := own[a.StepID]; !ok || a.ID > prev.ID {
			own[a.StepID] = a
		}
	}
	accepted, err := priorIntegrationsTx(tx, runID)
	if err != nil {
		return nil, err
	}

	var out []integrationCandidate
	for _, step := range sched.Steps() {
		if !sched.writeClassOf(step.Class) || !db.StepTerminal(step.Status) {
			continue
		}
		a, ok := own[step.ID]
		if !ok || a.Payload == "" {
			continue
		}
		var record struct {
			Head         string `json:"head"`
			Worktree     string `json:"worktree"`
			ResolvedFrom string `json:"resolved_from"`
		}
		if json.Unmarshal([]byte(a.Payload), &record) != nil || record.Head == "" {
			// No commit recorded — the step's work produced nothing to
			// verify (an empty round, or a step whose `issue.diff` never
			// resolved to a head at all). Nothing to check is not a finding.
			continue
		}
		id := model.FormatStepID(step.ID)
		out = append(out, integrationCandidate{
			step: id, instance: step.Instance,
			sha: record.Head, worktree: record.Worktree,
			prior:    accepted[id+"@"+record.Head],
			resolved: record.ResolvedFrom != "",
		})
	}
	return out, nil
}

// priorIntegrationsTx reads every acceptance the run's earlier closes
// recorded, keyed "STEP-N@sha", each carrying the close that recorded it and
// its verdict. Events are read oldest first, so the newest acceptance of a
// sha wins; a close older than this record (no `integration` key) contributes
// nothing.
func priorIntegrationsTx(tx *sql.Tx, runID int) (map[string]*CheckedIntegration, error) {
	rows, err := tx.Query(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq`,
		runID, EventDispatchClosed)
	if err != nil {
		return nil, fmt.Errorf("reading prior closes: %w", err)
	}
	defer rows.Close()

	out := map[string]*CheckedIntegration{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("reading prior closes: %w", err)
		}
		var data struct {
			Dispatch    string            `json:"dispatch"`
			Integration *IntegrationCheck `json:"integration"`
		}
		if json.Unmarshal([]byte(raw), &data) != nil || data.Integration == nil {
			continue
		}
		close := data.Dispatch
		if close == "" {
			close = "run"
		}
		for _, c := range data.Integration.Checked {
			out[c.Step+"@"+c.SHA] = &CheckedIntegration{
				Step: c.Step, Instance: c.Instance, SHA: c.SHA, How: c.How, Prior: close,
			}
		}
	}
	return out, rows.Err()
}

// checkIntegration runs the git questions OUTSIDE any transaction: ancestry
// first, patch-equivalence (`git cherry`, DKT-1033's zero-context refinement)
// when ancestry fails — the same order and the same two probes staleTargets
// asks of a review target, asked here of a write-class step's own commit.
//
// Verdicts are cached by sha, so a run with several write-class steps sharing
// one already-integrated tip pays one subprocess pair, not one per step.
func (e *Engine) checkIntegration(
	execRoot string, candidates []integrationCandidate,
) (checked []CheckedIntegration, unintegrated []UnintegratedStep) {
	if e == nil || e.IsAncestorFn == nil {
		// No git seam wired (a fake Engine in a unit test that does not care
		// about this check) — nothing is verified and nothing refuses; the
		// caller decides what an empty verdict means.
		return nil, nil
	}

	type verdict struct {
		how string
		ok  bool
	}
	cache := make(map[string]verdict, len(candidates))

	for _, c := range candidates {
		if c.prior != nil {
			// An earlier close of this run accepted exactly this sha; its
			// record is the authority, not a fresh git question the sha may
			// no longer be able to answer (a hand-resolved cherry-pick).
			checked = append(checked, *c.prior)
			continue
		}
		v, seen := cache[c.sha]
		if !seen {
			if ancestor, known := e.IsAncestorFn(execRoot, c.sha); known && ancestor {
				v = verdict{how: "ancestor", ok: true}
			} else if e.PatchContainedFn == nil {
				v = verdict{how: "unintegrated", ok: false}
			} else if contained, known := e.PatchContainedFn(execRoot, c.sha); known && contained {
				v = verdict{how: "patch-equivalent", ok: true}
			} else if known {
				v = verdict{how: "unintegrated", ok: false}
			} else {
				v = verdict{how: "cherry-error", ok: false}
			}
			cache[c.sha] = v
		}
		if v.ok {
			how := v.how
			if c.resolved && how == "ancestor" {
				// The annotation already verified this ancestry once; the
				// verdict names how the head came to be the branch's, so a
				// reader of the close can tell a resolved commit from one the
				// executor's own worktree landed verbatim.
				how = "resolved"
			}
			checked = append(checked, CheckedIntegration{
				Step: c.step, Instance: c.instance, SHA: c.sha, How: how,
			})
		} else {
			unintegrated = append(unintegrated, UnintegratedStep{
				Step: c.step, Instance: c.instance, SHA: c.sha,
				Worktree: c.worktree, How: v.how,
			})
		}
	}
	return checked, unintegrated
}

// integrationVerdict is DKT-1284's whole gate, run once per close attempt:
// collect this run's write-class candidates in a short READ-ONLY transaction
// (never committed, mirroring verifyDispatchTx), then either record them all
// as vouched for when the operator named a skip reason, or judge them against
// the shared checkout after the transaction ends.
//
// It returns EITHER a verdict to attach to a successful close (Status
// "verified" or "skipped") OR a non-empty unintegrated list for the caller to
// refuse on — never both, and never neither: a nil error with an empty
// unintegrated list always carries a non-nil verdict.
func (e *Engine) integrationVerdict(
	conn *sql.DB, runID int, defs map[int]*workflow.Definition, skipReason string, nowMS int64,
) (*IntegrationCheck, []UnintegratedStep, error) {
	// Read BEFORE the transaction opens: runExecRoot is its own pool read
	// (db.GetRun), and the pool is capped at one connection, so a call to it
	// from inside the transaction below would deadlock rather than fail.
	execRoot := runExecRoot(conn, runID)

	tx, err := conn.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("checking integration: %w", err)
	}
	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	candidates, err := integrationCandidatesTx(tx, sched, runID)
	// NEVER COMMITTED: this pass only reads, and the git questions below must
	// run with no transaction open (§6).
	tx.Rollback()
	if err != nil {
		return nil, nil, err
	}

	if skipReason != "" {
		// Git is asked nothing. What the reason vouched for is still written,
		// per candidate, so the next close honors it instead of refusing on a
		// commit the operator already ruled integrated (AC2: the record a
		// later close trusts is the engine's, written here or by a verified
		// close, never free-form step metadata).
		skipped := &IntegrationCheck{Status: "skipped", Reason: skipReason}
		for _, c := range candidates {
			if c.prior != nil {
				skipped.Checked = append(skipped.Checked, *c.prior)
				continue
			}
			skipped.Checked = append(skipped.Checked, CheckedIntegration{
				Step: c.step, Instance: c.instance, SHA: c.sha, How: "skipped",
			})
		}
		return skipped, nil, nil
	}

	checked, unintegrated := e.checkIntegration(execRoot, candidates)
	if len(unintegrated) > 0 {
		return nil, unintegrated, nil
	}
	return &IntegrationCheck{Status: "verified", Checked: checked}, nil, nil
}

// UnintegratedReason renders dispatch close's refusal, one line per finding —
// DiscrepancyReason's shape, for the same reason: an operator needs every
// unintegrated commit at once, not one refusal per retry.
func UnintegratedReason(steps []UnintegratedStep) string {
	var b strings.Builder
	for i, s := range steps {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %s: sha %.12s (%s)", s.Step, s.Instance, s.SHA, s.How)
		if s.Worktree != "" {
			fmt.Fprintf(&b, " in %s", s.Worktree)
		}
	}
	return b.String()
}
