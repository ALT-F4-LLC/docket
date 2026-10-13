package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-861 — `step resolve --as override-pass` silently dropped interposed gate
// steps, warning only after the mutation. Observed on RUN-61: the operator
// chose override-pass on verify@2 BECAUSE the offered option promised the run
// would proceed to its verify-tribunal gate; the generic pass skipped the
// tribunal instead, the run rolled to `done`, and the panel the operator
// explicitly bought never ruled. The DKT-470 warning named exactly this, but
// only beside a resolution already committed.
//
// The remedy under test: resolveStep REFUSES override-pass on a step whose
// threshold interposes other step(s) unless the operator passes
// --drop-interposed (ResolveStepDropInterposed), and the refusal carries the
// DKT-470 sentence — the consequence is presented BEFORE anything mutates. A
// step with no interposed targets resolves exactly as it always has, no flag
// required. The fixture is dkt470_test.go's interposeOverridePassSrc: a
// verify step whose threshold interposes a tribunal vote.

// TestOverridePassRefusedWithoutDropInterposed is acceptance criterion (i):
// with interposed dependents and no acknowledgment, the resolution is refused
// with the warning text, and NOTHING mutates — the step stays parked, the
// interposed vote stays pending, no routing is recorded.
func TestOverridePassRefusedWithoutDropInterposed(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)

	err = e.ResolveStep(conn, stepID, ResolveOverridePass, "accepted", nowMS)
	if err == nil {
		t.Fatal("override-pass with interposed dependents and no " +
			"--drop-interposed was accepted")
	}
	// The refusal IS the warning: the pre-mutation message carries the same
	// sentence the post-hoc warning used, plus the acknowledgment it asks for.
	for _, want := range []string{
		"tribunal", "verify@0", "will NOT be routed", "--drop-interposed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}

	// Nothing mutated: the refusal precedes the transaction.
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Errorf("verify@0 = %q after the refusal, want it still %q",
			got, db.StepWaitingHuman)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Errorf("tribunal@0 = %q after the refusal, want it still %q",
			got, db.StepPending)
	}
	// The park's own routing record is untouched — no `pass` was written.
	if got := stepRouting(t, conn, "verify@0"); got != string(db.StepWaitingHuman) {
		t.Errorf("verify@0 routing = %q after the refusal, want the park's "+
			"own %q record unchanged", got, db.StepWaitingHuman)
	}
}

// TestOverridePassBatchRefusedWithoutDropInterposed: --batch rides the same
// verb, so the acknowledgment gate covers it too — a standing authorization is
// exactly the ruling that must not slip past the interposed-step consequence.
func TestOverridePassBatchRefusedWithoutDropInterposed(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)

	err = e.ResolveStepBatch(conn, stepID, ResolveOverridePass, "accepted", nowMS)
	if err == nil {
		t.Fatal("batch override-pass with interposed dependents and no " +
			"--drop-interposed was accepted")
	}
	if !strings.Contains(err.Error(), "--drop-interposed") {
		t.Errorf("refusal = %q, want it to name --drop-interposed", err)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Errorf("tribunal@0 = %q after the refusal, want it still %q",
			got, db.StepPending)
	}
}

// Acceptance criterion (ii) — the acknowledged resolution proceeds and the
// interposed step still ends up skipped exactly as today — is pinned by
// dkt470_test.go's TestOverridePassStillBypassesTheThreshold, which now
// resolves through ResolveStepDropInterposed. The CLI half (the warning
// printed BEFORE the mutation on the acknowledged path) is
// internal/cli/step_resolve_interposed_test.go.

// TestOverridePassWithoutInterposedNeedsNoFlag is acceptance criterion (iii),
// the regression guard: a step whose threshold interposes nothing resolves
// under the plain, unacknowledged call exactly as before.
func TestOverridePassWithoutInterposedNeedsNoFlag(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	// The default corpus fixture: verify's threshold routes only to the
	// reserved fix-loop / waiting-human vocabulary — no step-name targets.
	activatedRun(t, conn)

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: verify@0 = %q, want %q", got, db.StepWaitingHuman)
	}

	stepID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t,
		e.ResolveStep(conn, stepID, ResolveOverridePass, "accepted", nowMS),
		"override-pass on a step with no interposed targets: %v", nil)
	if got := stepStatus(t, conn, "verify@0"); got != db.StepDone {
		t.Errorf("verify@0 = %q after the override-pass, want %q", got, db.StepDone)
	}
	if got := stepRouting(t, conn, "verify@0"); got != RoutingPass {
		t.Errorf("verify@0 routing = %q, want %q", got, RoutingPass)
	}
}

// TestDropInterposedRequiresOverridePass mirrors the --batch flag-combo guard:
// the acknowledgment waives a refusal only override-pass can trigger, so on
// any other resolution it is refused rather than silently accepted.
func TestDropInterposedRequiresOverridePass(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(interposeOverridePassSrc), "interpose-op.toml")
	issue := createIssue(t, conn, "op", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := parkVerifyWaitingHuman(t, conn, e)

	err = e.ResolveStepDropInterposed(conn, stepID, ResolveSkip, "nope", false, nowMS)
	if err == nil {
		t.Fatal("--drop-interposed with --as skip was accepted")
	}
	if !strings.Contains(err.Error(), ResolveOverridePass) {
		t.Errorf("refusal = %q, want it to name %s", err, ResolveOverridePass)
	}
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Errorf("verify@0 = %q after the refusal, want it still %q",
			got, db.StepWaitingHuman)
	}
}
