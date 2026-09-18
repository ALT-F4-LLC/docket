package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// repinFixturePolicyTOML is the policy.toml the repin fixtures pin: every
// verb that renders an offer row parses the pinned file, so a fixture that
// only needs the file to EXIST still needs one that parses and names the
// executors its workflow declares.
const repinFixturePolicyTOML = `
[policy]
version = 2

[variants]
tier = { model = "opus", effort = "high" }

[executors]
implement = { variant = "tier" }
review = { variant = "tier" }
`

// Routing — model/effort/variant on an executor row, voter_assignments on a
// vote row — resolves inside stepRow, the one row builder every producer
// shares. These tests pin the consequence: a dispatch manifest, `dispatch
// verify`'s recomputation, `step show`, and a context bundle's `step` carry
// exactly what `next --run` carries, and a row the run can no longer offer
// carries nothing. policyFixtureRun and findRow come from next_policy_test.go.

func assertWorkRouting(t *testing.T, where string, row *model.StepRow) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s: no row named %q", where, "work")
	}
	if row.Model != "opus" || row.Effort != "high" || row.Variant != "tier-a" {
		t.Errorf("%s: work row model/effort/variant = %q/%q/%q, want opus/high/tier-a",
			where, row.Model, row.Effort, row.Variant)
	}
}

func assertGateRouting(t *testing.T, where string, row *model.StepRow) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s: no row named %q", where, "gate")
	}
	want := map[string][3]string{
		"seat-a": {"opus", "high", "tier-a"},
		"seat-b": {"sonnet", "medium", "tier-b"},
	}
	if len(row.VoterAssignments) != len(want) {
		t.Fatalf("%s: got %d voter assignments, want %d: %+v",
			where, len(row.VoterAssignments), len(want), row.VoterAssignments)
	}
	for _, va := range row.VoterAssignments {
		w, ok := want[va.Voter]
		if !ok {
			t.Errorf("%s: unexpected voter %q", where, va.Voter)
			continue
		}
		if va.Model != w[0] || va.Effort != w[1] || va.Variant != w[2] {
			t.Errorf("%s: voter %s = %q/%q/%q, want %q/%q/%q",
				where, va.Voter, va.Model, va.Effort, va.Variant, w[0], w[1], w[2])
		}
	}
}

func assertUnrouted(t *testing.T, where string, row model.StepRow) {
	t.Helper()
	if row.Model != "" || row.Effort != "" || row.Variant != "" || len(row.VoterAssignments) != 0 {
		t.Errorf("%s: row carries routing it must not: model=%q effort=%q variant=%q voters=%+v",
			where, row.Model, row.Effort, row.Variant, row.VoterAssignments)
	}
}

// workStepID is the fixture's executor step, located through the offer so
// the test reads the same id a dispatcher would.
func workStepID(t *testing.T, rows []model.StepRow) int {
	t.Helper()
	row := findRow(rows, "work")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "work", rows)
	}
	id, err := model.ParseStepID(row.Step)
	testsupport.Must(t, err, "parsing %s: %v", row.Step, err)
	return id
}

// TestOpenDispatchRowsCarryRouting: the manifest a wave routes from carries
// the same resolution `next --run` does, on executor and vote rows both.
func TestOpenDispatchRowsCarryRouting(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	assertWorkRouting(t, "manifest", findRow(manifest.Rows, "work"))
	assertGateRouting(t, "manifest", findRow(manifest.Rows, "gate"))
}

// TestManifestMatchesNextUnderPinnedPolicy is TestManifestMatchesNext's
// byte-for-byte assertion over a run that pins a policy — the fixture that
// test uses pins none, so it cannot tell a manifest that resolved routing
// from one that dropped it.
func TestManifestMatchesNextUnderPinnedPolicy(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)

	answer, err := testEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	if len(manifest.Rows) != len(answer.Steps) || len(manifest.Rows) == 0 {
		t.Fatalf("manifest has %d rows, `next` offered %d", len(manifest.Rows), len(answer.Steps))
	}
	routed := false
	for i := range manifest.Rows {
		want, _, err := canonicalRowBytes(answer.Steps[i])
		testsupport.Must(t, err, "rendering the `next` row: %v", err)
		got, _, err := canonicalRowBytes(manifest.Rows[i])
		testsupport.Must(t, err, "rendering the manifest row: %v", err)
		if got != want {
			t.Errorf("row %d differs:\n  next:     %s\n  manifest: %s", i, want, got)
		}
		routed = routed || answer.Steps[i].Model != "" || len(answer.Steps[i].VoterAssignments) > 0
	}
	if !routed {
		t.Fatal("premise: the fixture must resolve routing on at least one row")
	}
}

// TestVerifyDispatchMatchesRoutedManifest: the recomputation `verify`
// compares against resolves through the same path the manifest did, so a
// routed manifest verifies as matched rather than shifted.
func TestVerifyDispatchMatchesRoutedManifest(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)
	openDispatch(t, conn, runID, 0, nowMS)

	result, mismatch, err := testEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify reported a mismatch at row %d:\n  stored:   %s\n  computed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Errorf("verify did not verify: %+v", result.Rows)
	}
}

// TestStepViewAndContextResolveAnOfferedRow: the read verbs render the same
// routing on a step the run can still offer, and none once it is handed out
// — the walk is keyed by the attempt an offer carries, and the claim bumped
// it.
func TestStepViewAndContextResolveAnOfferedRow(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)

	next, err := testEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	id := workStepID(t, next.Steps)

	view, err := LoadStepView(conn, id, nowMS)
	testsupport.Must(t, err, "step show: %v", err)
	assertWorkRouting(t, "step show (pending)", &view.Row)

	bundle, err := ReadContext(conn, id, nowMS)
	testsupport.Must(t, err, "step context: %v", err)
	assertWorkRouting(t, "step context (pending)", &bundle.Step)

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	assertUnrouted(t, "claim response context.step", claim.Context.Step)

	view, err = LoadStepView(conn, id, nowMS)
	testsupport.Must(t, err, "step show after claim: %v", err)
	assertUnrouted(t, "step show (claimed)", view.Row)

	bundle, err = ReadContext(conn, id, nowMS)
	testsupport.Must(t, err, "step context after claim: %v", err)
	assertUnrouted(t, "step context (claimed)", bundle.Step)
}

// TestOpenDispatchIsDormantWithNoPinnedPolicy: a run that pins no policy.toml
// opens a manifest whose rows are byte-identical to one opened before
// routing existed.
func TestOpenDispatchIsDormantWithNoPinnedPolicy(t *testing.T) {
	conn, runID := policyFixtureRun(t, "")
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	for _, row := range manifest.Rows {
		assertUnrouted(t, "manifest with no policy", row)
	}
}

// TestPinnedPolicyIsOpenedOnlyToRouteAnOffer: the policy file is read by the
// first rendered row that needs routing and by nothing else. Deleting it
// after activation leaves the inventory verbs and every terminal run's rows
// readable, while the scheduling verb that would route against it refuses.
func TestPinnedPolicyIsOpenedOnlyToRouteAnOffer(t *testing.T) {
	conn, dir := configRepo(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	writeConfigFile(t, dir, "workflows/policy-fixture.toml", policyFixtureWorkflow)
	policyPath := writeConfigFile(t, dir, "policy.toml", policyFixtureTOML)
	issue := createIssue(t, conn, "policy fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	next, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	id := workStepID(t, next.Steps)

	testsupport.Must(t, os.Remove(policyPath), "removing %s", filepath.Base(policyPath))

	if _, err := RunStepList(conn, run.ID, nowMS); err != nil {
		t.Errorf("step list opened the pinned policy: %v", err)
	}
	if _, err := testEngine().NextSteps(conn, run.ID, 0, nowMS); err == nil {
		t.Error("next --run offered rows it could not route: the pinned policy.toml is gone")
	}
	if _, err := LoadStepView(conn, id, nowMS); err == nil {
		t.Error("step show rendered an offerable row without its pinned policy")
	}

	_, _, err = MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		[]model.RunStatus{model.RunActive}, "fixture", nowMS)
	testsupport.Must(t, err, "abandon: %v", err)

	view, err := LoadStepView(conn, id, nowMS)
	testsupport.Must(t, err, "step show on an abandoned run: %v", err)
	assertUnrouted(t, "step show (abandoned run)", view.Row)
}
