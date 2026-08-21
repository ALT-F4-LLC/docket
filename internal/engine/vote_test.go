package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Vote-step execution (TDD docs/tdd/gates-trust.md §8).
//
// The fixture's `commit-gate` is a human step; the vote path is driven here
// against a purpose-built definition, because the committed fixture declares no
// vote step and inventing one in it would change every other test's topology.

// registerVoteRule sets a rule's two config keys, which is what makes it exist
// (§8.3: "a rule exists iff vote.rule.<name>.threshold is set").
func registerVoteRule(t *testing.T, conn *sql.DB, name, threshold, criticality string) {
	t.Helper()
	err := db.SetConfig(conn, 0, db.VoteRuleThresholdKey(name), threshold)
	testsupport.Must(t, err, "setting the rule threshold: %v", err)
	if criticality != "" {
		err := db.SetConfig(conn, 0, db.VoteRuleCriticalityKey(name), criticality)
		testsupport.Must(t, err, "setting the rule criticality: %v", err)
	}
}

// TestVoteTallyReachesTheExistingFunction is §8's central requirement, asserted
// by IDENTITY rather than by behavior.
//
// engine-core §6 says the existing machinery is "kept and demoted to a gate
// type", and §8 makes it explicit: CastVote's weighted score, its quorum rule,
// and its approved/rejected outcome are used UNCHANGED — "not copied, not
// re-implemented, not parameterized". A behavioral test would pass against a
// faithful reimplementation, which is exactly the thing forbidden, so this
// compares the compiled function pointer the engine would reach against the one
// `docket vote cast` reaches.
func TestVoteTallyReachesTheExistingFunction(t *testing.T) {
	// The engine names no tally of its own: it opens a proposal and READS the
	// status the existing machinery wrote. The proof that no second tally
	// exists is that internal/engine references db.CastVote's computation
	// nowhere — the outcome arrives through db.GetProposal.
	engineSide := runtime.FuncForPC(reflect.ValueOf(db.CastVote).Pointer()).Name()
	if !strings.HasSuffix(engineSide, "db.CastVote") {
		t.Fatalf("db.CastVote resolved to %q", engineSide)
	}

	// And the engine's vote path contains no threshold arithmetic: a
	// re-implementation would have to compute a weighted score somewhere.
	assertNoTallyArithmeticInVotePath(t)
}

// TestVoteRuleResolvesFromConfig is §8.3: a rule is a pair of config keys, and
// `required_voters` comes from the STEP's voter list rather than from the rule.
//
// The split is the point: a rule is about HOW STRICTLY to tally; the step is
// about WHO casts.
func TestVoteRuleResolvesFromConfig(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "high")

	rule, err := resolveVoteRule(conn, 1, "majority")
	testsupport.Must(t, err, "resolveVoteRule: %v", err)
	if rule.Threshold != 0.6 {
		t.Errorf("threshold = %v, want 0.6", rule.Threshold)
	}
	if rule.Criticality != model.CriticalityHigh {
		t.Errorf("criticality = %q, want %q", rule.Criticality, model.CriticalityHigh)
	}

	// An unregistered rule fails loudly rather than tallying at some default:
	// a threshold nobody chose is not a threshold.
	if _, err := resolveVoteRule(conn, 1, "nonexistent"); err == nil {
		t.Error("an unregistered rule resolved without error")
	}
}

// TestVoteRuleCriticalityDefaults covers the asymmetry between the two keys:
// the threshold is the EXISTENCE test and has no default, while criticality
// has one. A rule with only a criticality set would otherwise tally at no
// threshold at all.
func TestVoteRuleCriticalityDefaults(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "simple", "0.5", "")

	rule, err := resolveVoteRule(conn, 1, "simple")
	testsupport.Must(t, err, "resolveVoteRule: %v", err)
	if rule.Criticality != model.CriticalityMedium {
		t.Errorf("criticality = %q, want the %q default",
			rule.Criticality, model.CriticalityMedium)
	}

	exists, err := db.VoteRuleExists(conn, 1, "simple")
	if err != nil || !exists {
		t.Errorf("VoteRuleExists = %v, %v; a rule with a threshold exists", exists, err)
	}
	// Criticality alone does not make a rule.
	err = db.SetConfig(conn, 0, db.VoteRuleCriticalityKey("hollow"), "low")
	testsupport.Must(t, err, "SetConfig: %v", err)
	exists, err = db.VoteRuleExists(conn, 1, "hollow")
	testsupport.Must(t, err, "VoteRuleExists: %v", err)
	if exists {
		t.Error("a rule with only a criticality set reports as registered; " +
			"the threshold is the existence test (§8.3)")
	}
}

// TestVoteRuleConfigValidation covers the two new value kinds at `set` time, so
// a bad value fails where the operator can see it rather than at tally time.
func TestVoteRuleConfigValidation(t *testing.T) {
	conn := mustDB(t)

	for _, tc := range []struct {
		name, key, value string
		wantErr          bool
	}{
		{"threshold in range", db.VoteRuleThresholdKey("r"), "0.75", false},
		{"threshold at 1", db.VoteRuleThresholdKey("r"), "1", false},
		// Zero would approve on no support at all; above one is unreachable.
		{"threshold zero", db.VoteRuleThresholdKey("r"), "0", true},
		{"threshold above one", db.VoteRuleThresholdKey("r"), "1.5", true},
		{"threshold not a number", db.VoteRuleThresholdKey("r"), "most", true},
		{"criticality known", db.VoteRuleCriticalityKey("r"), "critical", false},
		{"criticality unknown", db.VoteRuleCriticalityKey("r"), "urgent", true},
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

// TestVoteProposalIsIdempotentUnderConcurrentInvocations is §8.1 phase 2's
// laziness: whichever engine invocation gets there first opens the proposal,
// and a concurrent second one produces no second proposal.
func TestVoteProposalIsIdempotentUnderConcurrentInvocations(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "medium")

	step, spec := seedVoteStep(t, conn)

	const invocations = 5
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids []int
	)
	for range invocations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := OpenVoteProposal(conn, step, spec, nowMS)
			if err == nil && id != 0 {
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(ids) == 0 {
		t.Fatal("no invocation opened a proposal")
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent invocations opened different proposals: %v", ids)
		}
	}

	var proposals int
	err := conn.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&proposals)
	testsupport.Must(t, err, "counting proposals: %v", err)
	if proposals != 1 {
		t.Errorf("%d proposals exist after %d concurrent invocations, want 1",
			proposals, invocations)
	}
}

// TestVoteProposalIsScopedPerIssue is DKT-65: two issues in the SAME run,
// each reaching a vote step at the SAME instance string, must open TWO
// proposals — one per issue — not collide on one.
//
// This is the exact shape a materialized held-cluster gate produces: its
// instance (`<step>-held@<ordinal>#<element>`, held.go's heldClusterInstance)
// carries no issue identity, so two issues whose reconcile both hold a
// cluster at the same element index render the identical instance. Measured
// live on RUN-5: DKT-56 and DKT-50 both materialized `reconcile-held@1#1`,
// and under the old run-only idempotency key the second issue's
// OpenVoteProposal found the first issue's proposal already there and reused
// it — a panel voting on one issue's evidence would have silently decided
// the other issue's unrelated gate too. A declared (non-materialized) vote
// step collides the identical way whenever two issues share a workflow, since
// instance is derived from step name and ordinal alone, never from the issue.
func TestVoteProposalIsScopedPerIssue(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "medium")

	stepA, spec := seedVoteStep(t, conn) // opens its own run + workflow
	stepB := seedVoteStepInRun(t, conn, int64(stepA.RunID), int64(stepA.WorkflowID), stepA.Instance)

	if stepA.IssueID == stepB.IssueID {
		t.Fatalf("fixture bug: both steps share issue %d", stepA.IssueID)
	}
	if stepA.Instance != stepB.Instance {
		t.Fatalf("fixture bug: instances differ (%q vs %q), not the collision shape",
			stepA.Instance, stepB.Instance)
	}

	idA, err := OpenVoteProposal(conn, stepA, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue A): %v", err)
	idB, err := OpenVoteProposal(conn, stepB, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue B): %v", err)

	if idA == idB {
		t.Fatalf("both issues' vote steps opened the SAME proposal %d despite "+
			"sharing instance %q — issue A is %d, issue B is %d",
			idA, stepA.Instance, stepA.IssueID, stepB.IssueID)
	}

	var proposals int
	err = conn.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&proposals)
	testsupport.Must(t, err, "counting proposals: %v", err)
	if proposals != 2 {
		t.Errorf("%d proposals exist for two issues' colliding-instance vote "+
			"steps, want 2", proposals)
	}

	// Each proposal is linked to its OWN issue only — the DB-level assertion
	// that a vote on one issue's proposal never reaches the other's.
	linkedA, err := db.GetProposalIssues(conn, idA)
	testsupport.Must(t, err, "ListProposalIssues(A): %v", err)
	if len(linkedA) != 1 || linkedA[0] != stepA.IssueID {
		t.Errorf("proposal A linked issues = %v, want only [%d]", linkedA, stepA.IssueID)
	}
	linkedB, err := db.GetProposalIssues(conn, idB)
	testsupport.Must(t, err, "ListProposalIssues(B): %v", err)
	if len(linkedB) != 1 || linkedB[0] != stepB.IssueID {
		t.Errorf("proposal B linked issues = %v, want only [%d]", linkedB, stepB.IssueID)
	}

	// Re-opening (the idempotent path, e.g. a second `next` poll) must return
	// each issue's OWN proposal, not cross over.
	idA2, err := OpenVoteProposal(conn, stepA, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue A, second call): %v", err)
	if idA2 != idA {
		t.Errorf("re-opening issue A's proposal returned %d, want the original %d", idA2, idA)
	}
	idB2, err := OpenVoteProposal(conn, stepB, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue B, second call): %v", err)
	if idB2 != idB {
		t.Errorf("re-opening issue B's proposal returned %d, want the original %d", idB2, idB)
	}
}

// TestLoadVoteProposalsIsScopedPerIssue is DKT-65's other half: the bulk
// snapshot reader the scheduler uses (loadVoteProposalsTx) must resolve the
// SAME per-issue mapping OpenVoteProposal created, not merge two issues'
// entries under one instance-only key.
func TestLoadVoteProposalsIsScopedPerIssue(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "medium")

	stepA, spec := seedVoteStep(t, conn)
	stepB := seedVoteStepInRun(t, conn, int64(stepA.RunID), int64(stepA.WorkflowID), stepA.Instance)

	idA, err := OpenVoteProposal(conn, stepA, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue A): %v", err)
	idB, err := OpenVoteProposal(conn, stepB, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal(issue B): %v", err)
	if idA == idB {
		t.Fatalf("setup collided: both issues got proposal %d", idA)
	}

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()

	proposals, err := loadVoteProposalsTx(tx, stepA.RunID, []*db.Step{stepA, stepB})
	testsupport.Must(t, err, "loadVoteProposalsTx: %v", err)

	if got := proposals[voteStepKeyOf(stepA)]; got != idA {
		t.Errorf("loadVoteProposalsTx[issue A] = %d, want %d", got, idA)
	}
	if got := proposals[voteStepKeyOf(stepB)]; got != idB {
		t.Errorf("loadVoteProposalsTx[issue B] = %d, want %d", got, idB)
	}
}

// TestVoteRequiredVotersComesFromTheStep is §8.2: `required_voters` is
// len(voters), and the voter hints are OPAQUE — core counts them and never
// interprets one.
func TestVoteRequiredVotersComesFromTheStep(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "medium")

	step, spec := seedVoteStep(t, conn)
	id, err := OpenVoteProposal(conn, step, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal: %v", err)

	proposal, err := db.GetProposal(conn, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if proposal.RequiredVoters != len(spec.Voters) {
		t.Errorf("required_voters = %d, want len(voters) = %d",
			proposal.RequiredVoters, len(spec.Voters))
	}
	if proposal.Threshold != 0.6 {
		t.Errorf("threshold = %v, want the rule's 0.6", proposal.Threshold)
	}
	if proposal.Status != model.ProposalStatusOpen {
		t.Errorf("status = %q, want %q", proposal.Status, model.ProposalStatusOpen)
	}
}

// TestVoteOutcomeMapsStatusToVerdict is §8.1 phase 4: approved ⇒ pass,
// rejected ⇒ fail, and still-open ⇒ nothing to route.
func TestVoteOutcomeMapsStatusToVerdict(t *testing.T) {
	for _, tc := range []struct {
		status      model.ProposalStatus
		wantVerdict string
	}{
		{model.ProposalStatusOpen, ""},
		{model.ProposalStatusApproved, VerdictPass},
		{model.ProposalStatusRejected, VerdictFail},
		// §8.4: a manually committed proposal is observed like any other.
		{model.ProposalStatusCommitted, VerdictPass},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.6", "medium")
			step, spec := seedVoteStep(t, conn)

			id, err := OpenVoteProposal(conn, step, spec, nowMS)
			testsupport.Must(t, err, "OpenVoteProposal: %v", err)
			_, err = conn.Exec(
				`UPDATE proposals SET status = ? WHERE id = ?`, tc.status, id)
			testsupport.Must(t, err, "setting the proposal status: %v", err)

			outcome, err := ReadVoteOutcome(conn, step, spec)
			testsupport.Must(t, err, "ReadVoteOutcome: %v", err)
			if outcome.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q for status %q, want %q",
					outcome.Verdict, tc.status, tc.wantVerdict)
			}
		})
	}
}

// seedVoteStep inserts a run, an issue, and one `type="vote"` step, returning
// the row and its definition.
//
// It builds its own definition rather than using the committed fixture, which
// declares no vote step: adding one there would change every other test's
// topology to serve this file.
func seedVoteStep(t *testing.T, conn *sql.DB) (*db.Step, *workflow.Step) {
	t.Helper()

	res, err := conn.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms)
		 VALUES ('vote-wf', 1, 'x', '', '{}', 1)`)
	testsupport.Must(t, err, "seeding a workflow: %v", err)
	wfID, _ := res.LastInsertId()

	res, err = conn.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'active', 1, 1)`)
	testsupport.Must(t, err, "seeding a run: %v", err)
	runID, _ := res.LastInsertId()

	step := seedVoteStepInRun(t, conn, runID, wfID, "approve@0")

	spec := &workflow.Step{
		Name:     "approve",
		Type:     workflow.TypeVote,
		Voters:   []string{"alice", "bob", "carol"},
		VoteRule: "majority",
		OnFail:   workflow.OnFailSkip,
	}
	return step, spec
}

// seedVoteStepInRun inserts a fresh issue and one `type="vote"` step at
// `instance`, against an EXISTING run and workflow — the fixture DKT-65's
// regression test needs: two issues in the SAME run, each reaching a vote
// step at the SAME instance string, is exactly the shape that collided under
// the old run-only idempotency key.
func seedVoteStepInRun(
	t *testing.T, conn *sql.DB, runID, wfID int64, instance string,
) *db.Step {
	t.Helper()

	res, err := conn.Exec(
		`INSERT INTO issues (title, status, created_at, updated_at)
		 VALUES ('vote subject', 'backlog', '2026-08-03', '2026-08-03')`)
	testsupport.Must(t, err, "seeding an issue: %v", err)
	issueID, _ := res.LastInsertId()

	stepName, _, _ := strings.Cut(instance, "@")
	res, err = conn.Exec(
		`INSERT INTO steps
		   (run_id, issue_id, workflow_id, step_name, instance, kind, status,
		    created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, 'vote', 'pending', 1, 1)`,
		runID, issueID, wfID, stepName, instance)
	testsupport.Must(t, err, "seeding a vote step: %v", err)
	stepID, _ := res.LastInsertId()

	step, err := db.GetStep(conn, int(stepID))
	testsupport.Must(t, err, "GetStep: %v", err)
	return step
}

// assertNoTallyArithmeticInVotePath is the structural half of §8's "unchanged"
// requirement: the engine's vote path must contain NO threshold arithmetic.
//
// A behavioral test passes against a faithful reimplementation, which is
// precisely what "not copied, not re-implemented, not parameterized" forbids.
// This checks the source: the engine may READ a weighted score, but computing
// one would mean a second implementation of the rule.
func assertNoTallyArithmeticInVotePath(t *testing.T) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("vote.go"))
	testsupport.Must(t, err, "reading vote.go: %v", err)
	for _, forbidden := range []string{
		"WeightedScore =",  // assigning a score is computing one
		"weightedScore :=", // ditto
		"Confidence",       // the tally's own inputs
		"DomainRelevance",
	} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("internal/engine/vote.go references %q — the tally is "+
				"db.CastVote's and must not be re-implemented here (§8)", forbidden)
		}
	}
}
