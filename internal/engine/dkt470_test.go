package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-470 — override-pass on a step whose threshold interposes another step
// silently bypasses the threshold and skips the interposed step, with no
// warning; and a step a routing predecessor has already decided against,
// permanently, reads as ordinary `pending` with no indication it will never
// become ready.

// interposeOverridePassSrc pairs a threshold-interposed VOTE step (the
// corpus's own shape, and the one that reproduces RUN-36/VPL-153: a `type =
// "vote"` step stays resolvable however it is currently routed, §6.10's
// exception, which is what lets an operator retry it back to `pending` after
// it was skipped) with a routing step that parks on its own failure —
// `override-pass` is the resolution reached for from that park.
const interposeOverridePassSrc = `
[pipeline]
name = "interpose-override-pass"
version = 1

[match]
kind = ["task"]

[[step]]
name = "verify"
executor = "verify"
emits = "report"
max_attempts = 1
on_fail = "waiting-human"
threshold = { "tribunal" = "any(status == blocked)" }

[[step]]
name = "tribunal"
after = ["verify"]
type = "vote"
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"

[[step]]
name = "finish"
after = ["tribunal"]
executor = "finish"
emits = "record"
`

// parkVerifyWaitingHuman exhausts verify@0's single attempt so it parks
// `waiting-human`, the state `override-pass` is reached for.
func parkVerifyWaitingHuman(t *testing.T, conn *sql.DB, e *Engine) int {
	t.Helper()
	stepID := stepIDByInstance(t, conn, "verify@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.FailStep(conn, stepID, claim.Token, "gate failed", "", nowMS),
		"fail: %v", err)
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: verify@0 = %q, want %q (max_attempts=1 should have "+
			"exhausted it)", got, db.StepWaitingHuman)
	}
	return stepID
}

// TestOverridePassSkipsInterposedTargetsNamesTheVote is the warning half of
// the fix: asked BEFORE override-pass commits anything, it names the
// interposed vote step by instance — the blast radius an operator approving
// override-pass could not previously see.
func TestOverridePassSkipsInterposedTargetsNamesTheVote(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)

	warnings := OverridePassSkipsInterposedTargets(conn, stepID)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if !strings.Contains(warnings[0], "tribunal") {
		t.Errorf("warning = %q, want it to name the interposed step", warnings[0])
	}
	if !strings.Contains(warnings[0], "verify@0") {
		t.Errorf("warning = %q, want it to name the step being overridden", warnings[0])
	}
}

// TestOverridePassSkipsInterposedTargetsEmptyWithoutAThreshold: a step with no
// threshold-target routing (an ordinary gate, or a threshold with only
// pass/fix-loop/waiting-human routings) warns about nothing — override-pass
// there really is just "pass", exactly as documented.
func TestOverridePassSkipsInterposedTargetsEmptyWithoutAThreshold(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	// The default corpus fixture's `implement` declares no threshold at all.
	stepID := stepIDByInstance(t, conn, "implement@0")
	if got := OverridePassSkipsInterposedTargets(conn, stepID); got != nil {
		t.Errorf("warnings = %v, want none for a step with no threshold", got)
	}
}

// TestOverridePassStillBypassesTheThreshold pins CURRENT behavior after the
// fix: an ACKNOWLEDGED override-pass (DKT-861's --drop-interposed) records a
// generic `pass` and the interposed vote is still skipped — the fix is the
// warning (above) and the pre-mutation refusal (dkt861_test.go), not a
// threshold evaluation. If a future change teaches override-pass to evaluate
// the threshold instead, this test is the one to revisit.
func TestOverridePassStillBypassesTheThreshold(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)

	testsupport.Must(t, e.ResolveStepDropInterposed(
		conn, stepID, ResolveOverridePass, "accepted", false, nowMS),
		"override-pass: %v", nil)

	if got := stepRouting(t, conn, "verify@0"); got != RoutingPass {
		t.Fatalf("verify@0 routing = %q, want the generic pass", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Errorf("tribunal@0 = %q after the override-pass, want %q — the "+
			"threshold's own condition (severity/status) was never consulted",
			got, db.StepSkipped)
	}
}

// TestUnroutedVoteAfterRetryReportsBlockedReason is the second fix's exact
// scenario: an interposed vote already skipped by an override-pass, then
// retried by an operator trying to recover it (§6.10's exception lets a
// `type="vote"` step resolve however it is currently routed). The retry
// returns it to `pending`, but the routing predecessor already recorded
// `pass` — permanently — so it will NEVER become ready. Before the fix
// nothing said so; `step show`/`step list`/`next` all rendered it as an
// ordinary, eventually-resolving `pending` wait.
func TestUnroutedVoteAfterRetryReportsBlockedReason(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)
	testsupport.Must(t, e.ResolveStepDropInterposed(
		conn, stepID, ResolveOverridePass, "accepted", false, nowMS),
		"override-pass: %v", nil)
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Fatalf("premise: tribunal@0 = %q, want %q", got, db.StepSkipped)
	}

	tribunalID := stepIDByInstance(t, conn, "tribunal@0")
	testsupport.Must(t, e.ResolveStep(conn, tribunalID, ResolveRetry, "trying to recover", nowMS),
		"retry: %v", nil)
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Fatalf("premise: tribunal@0 = %q after retry, want %q", got, db.StepPending)
	}

	// The scheduler-level fact: CondUnrouted, not CondPredecessors — the
	// routing predecessor is DONE, it simply never named this step.
	var tribunalStep *db.Step
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		tribunalStep = stepNamed(t, sched, "tribunal@0")
		ok, cond := sched.Ready(tribunalStep)
		if ok || cond != CondUnrouted {
			t.Fatalf("tribunal@0 after retry: ready=%v cond=%q, want the "+
				"CondUnrouted latch", ok, cond)
		}
		if got := BlockedReason(sched, tribunalStep); got != string(CondUnrouted) {
			t.Errorf("BlockedReason = %q, want %q", got, CondUnrouted)
		}
		if got := sched.UnroutedHoldReason(); !strings.Contains(got, "tribunal@0") {
			t.Errorf("UnroutedHoldReason = %q, want it to name tribunal@0", got)
		}
	})

	// `step show`'s own read path agrees.
	view, err := LoadStepView(conn, tribunalID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.BlockedReason != string(CondUnrouted) {
		t.Errorf("step show's BlockedReason = %q, want %q",
			view.Row.BlockedReason, CondUnrouted)
	}

	// `step list`'s inventory row agrees too.
	rows, err := RunStepList(conn, run.ID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	found := false
	for _, row := range rows {
		if row.Instance == "tribunal@0" {
			found = true
			if row.BlockedReason != string(CondUnrouted) {
				t.Errorf("step list's BlockedReason = %q, want %q",
					row.BlockedReason, CondUnrouted)
			}
		}
	}
	if !found {
		t.Fatalf("tribunal@0 not found in step list")
	}

	// `next --run`'s empty offer names it, on the RESULT rather than only on
	// stderr, so the assertion does not depend on output capture.
	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if answer.UnroutedReason == "" {
		t.Fatalf("next's UnroutedReason is empty with a permanently-unrouted "+
			"pending step in the run: %+v", answer)
	}
	if !strings.Contains(answer.UnroutedReason, "tribunal@0") {
		t.Errorf("UnroutedReason = %q, want it to name tribunal@0", answer.UnroutedReason)
	}
}
