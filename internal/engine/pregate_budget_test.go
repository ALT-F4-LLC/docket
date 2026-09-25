package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// The claim's pre-gate phase is bounded as a whole (TDD
// docs/tdd/gates-trust.md §7.6.2 PG5).
//
// An executor runs `docket step claim` under a 120s tool timeout. A claim
// whose pre-gates ran to their entries' 5m timeouts outlived it: the harness
// backgrounded the command, the token was never read, and the lease it
// minted needed a forced reap and an acknowledgment panel. The budget is the
// documented bound the claim returns within, and PG2/PG3 still hold: the
// claim succeeds with the cut-short measurement recorded as data.

// shrinkClaimBudget sets the budget for one test and restores it after.
func shrinkClaimBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	was := claimPreGateBudget
	claimPreGateBudget = budget
	t.Cleanup(func() { claimPreGateBudget = was })
}

// TestClaimReturnsWithinThePreGateBudget: a pre-gate that would run far past
// the budget is cut off at it, the claim returns with a token inside the
// budget, and the recorded row names the budget beside the entry's own
// timeout.
func TestClaimReturnsWithinThePreGateBudget(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)

	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// Far longer than the budget, under the entry's default 5m timeout.
	argv := []string{"/bin/sleep", "30"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	start := time.Now()
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	elapsed := time.Since(start)
	testsupport.Must(t, err, "claim: %v", err)

	// WITHIN THE BOUND: the budget plus the kill grace, with room for load,
	// and nowhere near the 30s the command wanted.
	if elapsed > 5*time.Second {
		t.Errorf("the claim took %s against a %s pre-gate budget", elapsed, claimPreGateBudget)
	}
	// AND IT SUCCEEDED: PG2/PG3 — the bound changes when the claim returns,
	// not whether.
	if claim.Token == "" {
		t.Fatal("the claim returned no token")
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepClaimed {
		t.Errorf("status = %q after the claim, want %q", step.Status, db.StepClaimed)
	}

	// THE MEASUREMENT IS DATA: recorded as a timeout that names the budget
	// and the entry's own bound, so a reader can tell "the claim cut this
	// short" from "the trust entry's timeout changed".
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("pre-gate results = %+v, want one", claim.Context.PreGates)
	}
	row := claim.Context.PreGates[0]
	if row.Verdict != VerdictFail {
		t.Errorf("verdict = %q, want %q — a gate the budget cut off ran and did "+
			"not finish, which is a measured failure, not an absence", row.Verdict, VerdictFail)
	}
	if !strings.Contains(row.Reason, "pre-gate budget") || !strings.Contains(row.Reason, "5m0s") {
		t.Errorf("the reason names neither the budget nor the entry's own timeout: %q", row.Reason)
	}
}

// TestSpentBudgetSkipsTheGate is the runner half for a gate whose turn comes
// after the budget is gone: nothing spawns, the row is `skipped` naming the
// budget, and the fields that describe a process stay empty.
func TestSpentBudgetSkipsTheGate(t *testing.T) {
	repoRoot, docketDir := treeLockRepo(t)
	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot,
		LockPath: docketDir + "/tree.lock",
	})
	argv := []string{"/bin/sleep", "30"}
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	start := time.Now()
	ex, err := runner.Execute(t.Context(), GateSpec{Name: "ac-commands"},
		StepContext{Deadline: time.Now().Add(-time.Second)})
	testsupport.Must(t, err, "Execute: %v", err)

	if time.Since(start) > time.Second {
		t.Errorf("a gate with no budget left still ran")
	}
	if len(ex.Results) != 1 {
		t.Fatalf("recorded %d rows, want 1: %+v", len(ex.Results), ex.Results)
	}
	row := ex.Results[0]
	if row.Verdict != VerdictSkipped || !strings.Contains(row.Reason, "pre-gate budget") {
		t.Errorf("row = %+v, want skipped with a reason naming the budget", row)
	}
	if row.Exit != nil || row.DurationMS != 0 || row.Output != "" {
		t.Errorf("a gate that never spawned recorded process fields: %+v", row)
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("execution verdict = %q, want %q — unmeasured is not a pass", ex.Verdict, VerdictFail)
	}
}
