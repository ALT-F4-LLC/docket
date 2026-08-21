package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Held clusters decided by a TALLY instead of by one operator.
//
// `vote.hold.rule` and `vote.hold.voters` mint a hold's steps as `type="vote"`
// rather than `type="human"`. Everything else about a hold is unchanged, and
// the escalation is one-directional: a tally that passes lets the engine's
// computed value stand, and a tally that does NOT pass parks the step for an
// operator. It never routes the aggregate itself, so a panel that could not
// agree cannot produce the same effect as an operator who declined.
//
// The default is the whole of the prior behavior, and TestHeldStepMintsHuman*
// below is what holds it there.

// configureHoldTally sets both keys, which is what makes a hold a tally.
func configureHoldTally(t *testing.T, conn *sql.DB, rule string, voters string) {
	t.Helper()
	registerVoteRule(t, conn, rule, "0.6", "high")
	err := db.SetConfig(conn, 0, db.KeyVoteHoldRule, rule)
	testsupport.Must(t, err, "setting %s: %v", db.KeyVoteHoldRule, err)
	err = db.SetConfig(conn, 0, db.KeyVoteHoldVoters, voters)
	testsupport.Must(t, err, "setting %s: %v", db.KeyVoteHoldVoters, err)
}

// nextRun runs `next --run` over the fixture's run, which is what drives a
// vote step's lifecycle in production (§8.1 phases 2, 4, and 5).
func nextRun(t *testing.T, conn *sql.DB, e *Engine) *ReadySteps {
	t.Helper()
	ready, err := e.NextSteps(conn, 1, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	return ready
}

// setProposalStatus finishes a tally the way the existing machinery would.
//
// It writes the proposal's STATUS rather than casting votes, for the reason
// TestVoteOutcomeMapsStatusToVerdict does the same: the engine READS an outcome
// the existing tally computed, and a test that cast votes here would be testing
// db.CastVote's arithmetic a second time instead of the engine's reading of it.
func setProposalStatus(t *testing.T, conn *sql.DB, id int, status model.ProposalStatus) {
	t.Helper()
	_, err := conn.Exec(`UPDATE proposals SET status = ? WHERE id = ?`, status, id)
	testsupport.Must(t, err, "setting the proposal status: %v", err)
}

// heldProposalID resolves the proposal a held step opened, through the row the
// engine reports rather than through the idempotency key — so the test reads
// the same field an operator does.
func heldProposalID(t *testing.T, conn *sql.DB, e *Engine, instance string) int {
	t.Helper()
	view, err := LoadStepView(conn, stepIDByInstance(t, conn, instance), nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.Proposal == "" {
		t.Fatalf("%s carries no proposal id on its row", instance)
	}
	id, err := model.ParseProposalID(view.Row.Proposal)
	testsupport.Must(t, err, "parsing %q: %v", view.Row.Proposal, err)
	return id
}

// driveHeldVote takes a hold all the way to the tally's verdict: mint, open the
// proposal, finish it with the given status, and let the next invocation route
// on it. It returns the held instances in their stable order.
func driveHeldVote(
	t *testing.T, conn *sql.DB, e *Engine, payload string, status model.ProposalStatus,
) []string {
	t.Helper()
	driveToReconcile(t, conn, e, payload)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	// Phase 2: the first invocation that observes the step ready opens its
	// proposal.
	nextRun(t, conn, e)
	for _, instance := range held {
		setProposalStatus(t, conn, heldProposalID(t, conn, e, instance), status)
	}
	// Phases 4 and 5: read the outcome and route on it.
	nextRun(t, conn, e)
	return held
}

// ---------------------------------------------------------------------------
// The default: unconfigured is EXACTLY the prior behavior
// ---------------------------------------------------------------------------

// TestHeldStepMintsHumanWhenUnconfigured is the strict no-op default.
//
// It is the first test in the file because everything below only earns its
// place if this holds: an instance that never configured a tally must observe a
// hold it cannot distinguish from the one it got before these keys existed.
func TestHeldStepMintsHumanWhenUnconfigured(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	if held.Kind != workflow.TypeHuman {
		t.Fatalf("kind = %q with no tally configured, want %q — an unset key "+
			"must not change how a hold is asked", held.Kind, workflow.TypeHuman)
	}

	// And the synthesized spec is the human one, inheriting the routing step's
	// `on_fail` rather than the vote form's park.
	defs, err := StepDefinitions(conn, held.RunID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	tally, err := loadHoldTally(conn, held.RunID)
	testsupport.Must(t, err, "loadHoldTally: %v", err)
	if tally.configured() {
		t.Fatal("an unset pair of keys reads as a configured tally")
	}
	spec := materializedSpec(defs[held.WorkflowID], held, tally)
	if spec == nil || spec.Type != workflow.TypeHuman {
		t.Fatalf("synthesized spec type = %v, want %q", spec, workflow.TypeHuman)
	}
	if len(spec.Voters) != 0 || spec.VoteRule != "" {
		t.Errorf("a human-minted hold carries voters %v / rule %q",
			spec.Voters, spec.VoteRule)
	}
}

// TestHeldStepMintsHumanWhenHalfConfigured pins the both-or-neither rule.
//
// Either key alone is incoherent — a rule with nobody to cast under it tallies
// zero votes forever, and a roster with no rule has no threshold to be measured
// against — so a half-configured instance falls back to the form that still
// gets an answer.
func TestHeldStepMintsHumanWhenHalfConfigured(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rule, voters string
	}{
		{"rule without voters", "panel", ""},
		{"voters without rule", "", "alice,bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()

			registerVoteRule(t, conn, "panel", "0.6", "high")
			err := db.SetConfig(conn, 0, db.KeyVoteHoldRule, tc.rule)
			testsupport.Must(t, err, "SetConfig: %v", err)
			err = db.SetConfig(conn, 0, db.KeyVoteHoldVoters, tc.voters)
			testsupport.Must(t, err, "SetConfig: %v", err)

			driveToReconcile(t, conn, e, clusteredPayload)

			held := heldStep(t, conn, "reconcile-held@0#0")
			if held.Kind != workflow.TypeHuman {
				t.Errorf("kind = %q with only one key set, want %q",
					held.Kind, workflow.TypeHuman)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Configured: the mint, and the two verdicts
// ---------------------------------------------------------------------------

// TestHeldStepMintsVoteWhenConfigured is the feature: the same hold, asked of a
// roster instead of one operator.
func TestHeldStepMintsVoteWhenConfigured(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	if held.Kind != workflow.TypeVote {
		t.Fatalf("kind = %q, want %q", held.Kind, workflow.TypeVote)
	}
	// Everything else about the hold is unchanged: it is still the row
	// expansion did not write, still at the routing step's own ordinal, still
	// pending with no executor.
	if !held.Materialized {
		t.Error("materialized = 0; a vote-minted hold is still a computed question")
	}
	if held.Executor != "" {
		t.Errorf("executor = %q; a gate step has none", held.Executor)
	}

	defs, err := StepDefinitions(conn, held.RunID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	tally, err := loadHoldTally(conn, held.RunID)
	testsupport.Must(t, err, "loadHoldTally: %v", err)
	spec := materializedSpec(defs[held.WorkflowID], held, tally)
	if spec.Type != workflow.TypeVote {
		t.Errorf("spec type = %q, want %q", spec.Type, workflow.TypeVote)
	}
	if spec.VoteRule != "panel" {
		t.Errorf("vote_rule = %q, want the configured rule", spec.VoteRule)
	}
	if strings.Join(spec.Voters, ",") != "alice,bob,carol" {
		t.Errorf("voters = %v, want the configured roster", spec.Voters)
	}
	// The park is the safety property: a tally that does not pass hands the
	// question to an operator rather than routing the aggregate.
	if got := spec.EffectiveOnFail(); got != workflow.OnFailWaitingHuman {
		t.Errorf("on_fail = %q, want %q — a failed tally must escalate, not route",
			got, workflow.OnFailWaitingHuman)
	}
}

// TestHeldVoteStillRefusesClaim: a vote-minted hold is no more claimable than a
// human-minted one. A dispatcher that spawns on every `next` row must not spawn
// a worker for it.
func TestHeldVoteStillRefusesClaim(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "reconcile-held@0#0"),
		ClaimOptions{Owner: "someone", NowMS: nowMS})
	if err == nil {
		t.Fatal("a vote-minted held step was claimed")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}
}

// TestHeldVotePassLetsTheComputedValueStand is the approve-computed
// equivalence (H14): a tally that passes is the same answer `step approve`
// gives, so the aggregated value stands and the run proceeds.
func TestHeldVotePassLetsTheComputedValueStand(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, clusteredPayload, model.ProposalStatusApproved)

	step := heldStep(t, conn, held[0])
	if step.Status != db.StepDone {
		t.Fatalf("status = %q after a passing tally, want %q", step.Status, db.StepDone)
	}
	if !routingIs(step.Routing, RoutingPass) {
		t.Errorf("routing = %q, want %q", step.Routing, RoutingPass)
	}

	// The cluster is recorded as resolved, exactly as an approval records it —
	// the tally answered the question, so the payload says so.
	elements := resolvedElements(t, conn)
	if resolved, _ := elements[0][KeyOperatorResolved].(bool); !resolved {
		t.Error("the approved cluster is not marked resolved")
	}

	// And the aggregate's saga is no longer holding: a decided hold must
	// un-defer the routing step, or the run sits gated on a question that has
	// been answered.
	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage == db.SagaHeld {
		t.Error("reconcile@0 is still holding after its hold was decided by vote")
	}
}

// TestHeldVoteFailParksForTheOperator is the ratified escalation: ANY
// non-approval goes to the operator, and the panel never overrides the held
// value itself.
func TestHeldVoteFailParksForTheOperator(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, clusteredPayload, model.ProposalStatusRejected)

	step := heldStep(t, conn, held[0])
	if step.Status != db.StepWaitingHuman {
		t.Fatalf("status = %q after a failed tally, want %q — a failed vote "+
			"parks the gate for the operator rather than routing the aggregate",
			step.Status, db.StepWaitingHuman)
	}

	// The aggregate is STILL HOLDING. A park is not a decision, so nothing
	// downstream may proceed on it.
	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage != db.SagaHeld {
		t.Errorf("reconcile@0's saga is at %q; a parked hold is undecided and "+
			"must keep gating", routing.SagaStage)
	}
	if ready := readyInstances(t, conn); contains(ready, "verify@0") {
		t.Error("a downstream step became ready while a parked hold was undecided")
	}
	if resolved, _ := resolvedElements(t, conn)[0][KeyOperatorResolved].(bool); resolved {
		t.Error("a failed tally marked the cluster resolved; it decided nothing")
	}
}

// ---------------------------------------------------------------------------
// The operator's verbs on a parked vote-minted hold
// ---------------------------------------------------------------------------

// TestParkedHeldVoteAcceptsApproveWithValue is the whole point of parking
// rather than routing: the operator gets the SAME verbs a human-minted hold
// has always accepted, including the schema-validated correction.
func TestParkedHeldVoteAcceptsApproveWithValue(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, multiClusterPayload, model.ProposalStatusRejected)

	err := e.DecideStepValue(conn, stepIDByInstance(t, conn, held[0]),
		true, "the blocker report is right", "high", nowMS)
	testsupport.Must(t, err, "approving a parked vote-minted hold: %v", err)

	step := heldStep(t, conn, held[0])
	if step.Status != db.StepDone || !routingIs(step.Routing, RoutingPass) {
		t.Fatalf("status/routing = %q/%q after approve, want done/pass",
			step.Status, step.Routing)
	}

	elements := resolvedElements(t, conn)
	if got, _ := elements[0]["severity"].(string); got != "high" {
		t.Errorf("severity = %q, want the operator's corrected value", got)
	}
	if got, _ := elements[0][KeyOperatorSetFrom].(string); got != "low" {
		t.Errorf("operator_set_from = %q, want the computed value it replaced", got)
	}

	// A value outside the pinned schema's declared enum is still refused, on
	// this path as on the other.
	err = e.DecideStepValue(conn, stepIDByInstance(t, conn, held[1]),
		true, "", "urgent", nowMS)
	if err == nil {
		t.Error("a value outside the declared enum was accepted on a parked vote hold")
	}
}

// TestParkedHeldVoteAcceptsReject: the escalating answer still escalates, and
// the aggregate routes per its own `on_fail` — the same consequence a rejected
// human-minted hold has.
func TestParkedHeldVoteAcceptsReject(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, clusteredPayload, model.ProposalStatusRejected)

	err := e.DecideStep(conn, stepIDByInstance(t, conn, held[0]),
		false, "escalated", nowMS)
	testsupport.Must(t, err, "rejecting a parked vote-minted hold: %v", err)

	step := heldStep(t, conn, held[0])
	if step.Status != db.StepDone {
		t.Fatalf("status = %q after reject, want %q (H14: both verbs record a "+
			"decision)", step.Status, db.StepDone)
	}
	if routingIs(step.Routing, RoutingPass) {
		t.Errorf("routing = %q; a rejection did not pass", step.Routing)
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage == db.SagaHeld {
		t.Error("reconcile@0 is still holding after its hold was rejected")
	}
}

// TestHeldVoteRefusesApproveBeforeTheTallyParksIt keeps the two deciders in
// order: the panel answers first.
//
// Approving while the proposal is still open would take the panel's turn away
// mid-question and leave an orphaned open proposal behind. The refusal names
// `step resolve`, which is the verb that genuinely moves a run past an open
// vote.
func TestHeldVoteRefusesApproveBeforeTheTallyParksIt(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)
	nextRun(t, conn, e) // opens the proposal; the vote is now open

	err := e.DecideStep(conn, stepIDByInstance(t, conn, "reconcile-held@0#0"),
		true, "", nowMS)
	if err == nil {
		t.Fatal("an open vote was overruled by approve")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), "resolve") {
		t.Errorf("the refusal names no verb that moves the step: %v", err)
	}
}

// TestParkedHeldVoteRefusesRetry closes the same trap `parkedByRejectedHold`
// closes one step over: retry resets the attempt counter, which is not what is
// blocking a step parked by a finished tally. The next `next` would read the
// SAME proposal — the idempotency key is (run, instance) — and park it again.
func TestParkedHeldVoteRefusesRetry(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, clusteredPayload, model.ProposalStatusRejected)

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, held[0]),
		ResolveRetry, "", nowMS)
	if err == nil {
		t.Fatal("retry was accepted on a step parked by a finished tally")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), "approve") {
		t.Errorf("the refusal does not name the verb that works: %v", err)
	}
}

// TestParkedHeldVoteResolvesWithoutStrandingTheRun covers the path that only
// became reachable when holds could be minted as votes.
//
// `step resolve` was refused on a human-minted hold — it is created `pending`
// with kind `human`, which satisfies neither arm of R11's test — so the verb
// could not reach a held step at all. A vote-minted hold parked by a failed
// tally is `waiting-human`, so `resolve` is offered on it like any other parked
// step, and a resolution that decided the cluster without resuming the
// aggregate would leave the question answered and the run gated on it forever.
func TestParkedHeldVoteResolvesWithoutStrandingTheRun(t *testing.T) {
	for _, as := range []string{ResolveOverridePass, ResolveSkip} {
		t.Run(as, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()
			configureHoldTally(t, conn, "panel", "alice,bob,carol")

			held := driveHeldVote(t, conn, e, clusteredPayload,
				model.ProposalStatusRejected)

			err := e.ResolveStep(conn, stepIDByInstance(t, conn, held[0]),
				as, "operator call", nowMS)
			testsupport.Must(t, err, "resolving with --as %s: %v", as, err)

			routing := heldStep(t, conn, "reconcile@0")
			if routing.SagaStage == db.SagaHeld {
				t.Errorf("reconcile@0 is still holding after its cluster was "+
					"resolved --as %s; the question is answered and nothing "+
					"else can move the run", as)
			}
		})
	}
}

// TestOverridePassOnAHeldClusterRecordsTheResolution: `override-pass` records
// `done` with a `pass` routing, which heldDecision reads back as an approval —
// so it must leave the same payload record `approve` leaves, or the run
// proceeds over a cluster the payload still reports as unresolved.
func TestOverridePassOnAHeldClusterRecordsTheResolution(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	held := driveHeldVote(t, conn, e, clusteredPayload, model.ProposalStatusRejected)

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, held[0]),
		ResolveOverridePass, "accepting the computed value", nowMS)
	testsupport.Must(t, err, "override-pass: %v", err)

	elements := resolvedElements(t, conn)
	if resolved, _ := elements[0][KeyOperatorResolved].(bool); !resolved {
		t.Error("an overridden cluster is not marked resolved; the routing " +
			"says the run may proceed and the payload says nobody decided")
	}
	if got, _ := elements[0][KeyOperatorNote].(string); got != "accepting the computed value" {
		t.Errorf("operator_note = %q, want the note the operator gave", got)
	}
}

// TestHeldVoteSurvivesTheRosterBeingCleared: the MINTED KIND is what persists.
//
// Config supplies the roster; the step row supplies the question's type, fixed
// when the hold was minted. Clearing the keys mid-run must not retype an
// already-asked question into one no verb can answer.
func TestHeldVoteSurvivesTheRosterBeingCleared(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)
	held := heldStep(t, conn, "reconcile-held@0#0")

	err := db.SetConfig(conn, 0, db.KeyVoteHoldVoters, "")
	testsupport.Must(t, err, "clearing the roster: %v", err)

	defs, err := StepDefinitions(conn, held.RunID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	tally, err := loadHoldTally(conn, held.RunID)
	testsupport.Must(t, err, "loadHoldTally: %v", err)
	spec := materializedSpec(defs[held.WorkflowID], held, tally)
	if spec.Type != workflow.TypeVote {
		t.Errorf("spec type = %q after the roster was cleared, want %q — the "+
			"row's kind decides the form, never live config",
			spec.Type, workflow.TypeVote)
	}
}

// TestHoldTallyConfigValidation covers the new list kind at `set` time, so a
// bad roster fails where the operator can see it rather than at mint time.
func TestHoldTallyConfigValidation(t *testing.T) {
	conn := mustDB(t)

	for _, tc := range []struct {
		name, key, value string
		wantErr          bool
	}{
		{"empty roster", db.KeyVoteHoldVoters, "", false},
		{"one voter", db.KeyVoteHoldVoters, "alice", false},
		{"several, spaced", db.KeyVoteHoldVoters, "alice, bob, carol", false},
		{"empty entry", db.KeyVoteHoldVoters, "alice,,bob", true},
		{"duplicate entry", db.KeyVoteHoldVoters, "alice,bob,alice", true},
		{"whitespace inside a name", db.KeyVoteHoldVoters, "alice smith,bob", true},
		{"empty rule", db.KeyVoteHoldRule, "", false},
		{"a rule name", db.KeyVoteHoldRule, "panel", false},
		{"a rule name with a space", db.KeyVoteHoldRule, "the panel", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.SetConfig(conn, 0, tc.key, tc.value)
			if tc.wantErr && err == nil {
				t.Errorf("SetConfig(%q, %q) succeeded, want a refusal", tc.key, tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("SetConfig(%q, %q): %v", tc.key, tc.value, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// guard stop yields to a deliberating panel (DKT-107)
// ---------------------------------------------------------------------------

// TestGuardStopYieldsToAnOpenVotePanel pins the wait-contract: while a
// vote-minted hold's proposal is OPEN, the panel decides out-of-session and
// `guard stop` must ALLOW — the deny was a toll paid at every turn-end for
// as long as a panel deliberated, and only stop-hook loop-prevention let the
// session yield at all. The exemption is exactly the proposal's lifetime:
// before it opens the step is dispatchable (`next` opens it), and once it is
// decided the step is dispatchable again (`next` routes it), so both of
// those states deny as before.
func TestGuardStopYieldsToAnOpenVotePanel(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "hold-panel", "seat-a,seat-b")

	driveToReconcile(t, conn, e, clusteredPayload)

	// Before phase 2: no proposal exists, so opening one is undone work.
	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if verdict.Allowed {
		t.Error("guard stop allowed before the proposal opened; opening it " +
			"is `next`'s work, which a stop would leave undone")
	}

	// Phase 2: the proposals open; the panel now deliberates out-of-session.
	nextRun(t, conn, e)

	verdict, err = GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if !verdict.Allowed {
		t.Errorf("guard stop denied while every open question was an open "+
			"vote proposal: %s\nyielding the turn IS the correct conductor "+
			"move here — the panels' completions arrive out-of-session",
			verdict.Reason)
	}

	// The tally decides; the step is dispatchable again until `next` routes.
	for _, instance := range heldInstances(t, conn) {
		setProposalStatus(t, conn, heldProposalID(t, conn, e, instance),
			model.ProposalStatusApproved)
	}
	verdict, err = GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if verdict.Allowed {
		t.Error("guard stop allowed while a DECIDED vote awaited routing; " +
			"reading the outcome is `next`'s work")
	}
}
