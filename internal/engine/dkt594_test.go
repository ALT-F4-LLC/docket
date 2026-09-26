package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-594 — the two facts a post-mortem reader had to reconstruct by hand.
//
// The corpus took 41 commits in 4.3 days (9.55/day against 0.86/day the week
// before). Every analyst reading RUN-32 went to git to learn that its
// `ui-change@8` was five registered versions behind before they would trust a
// finding from it, because the run report carried no such field. And RUN-39,
// which repinned mid-flight, could only be read by correlating repin event seqs
// (5375/5376) against step ids (STEP-1350/1353) by hand — the `pins` table
// holds the CURRENT agreement and completed steps' rows are never rewritten, so
// nothing said which bytes a finished step actually consumed.

// registerVersion registers the fixture's TOML at another `[pipeline].version`,
// through the same parse-validate-lint path `workflow register` uses — which is
// what a corpus commit that edits a workflow produces.
func registerVersion(t *testing.T, conn *sql.DB, version int) *model.Workflow {
	t.Helper()
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	bumped := strings.Replace(
		string(src), "version = 1", "version = "+strconv.Itoa(version), 1)
	if bumped == string(src) {
		t.Fatalf("the fixture's `version = 1` line moved; this helper cannot bump it")
	}
	return registerSource(t, conn, []byte(bumped), fixturePath)
}

// pinnedWorkflow finds the report's staleness row for one pinned ref.
func pinnedWorkflow(
	t *testing.T, r *RunReport, ref string,
) PinnedWorkflowStaleness {
	t.Helper()
	for _, w := range r.PinnedWorkflows {
		if w.Ref == ref {
			return w
		}
	}
	t.Fatalf("the report carries no staleness row for %s: %+v", ref, r.PinnedWorkflows)
	return PinnedWorkflowStaleness{}
}

// attemptFor finds one step's attempt row by instance.
func attemptFor(t *testing.T, r *RunReport, instance string) StepAttempt {
	t.Helper()
	for _, a := range r.Attempts {
		if a.Instance == instance {
			return a
		}
	}
	t.Fatalf("the report carries no attempt row for %s", instance)
	return StepAttempt{}
}

// TestReportCountsCorpusVersionsSinceActivation is criterion 1: the
// subtraction an analyst was doing in git is in the document.
func TestReportCountsCorpusVersionsSinceActivation(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	// The corpus moves twice under the run, exactly as `just activate` does.
	registerVersion(t, conn, 2)
	registerVersion(t, conn, 3)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	got := pinnedWorkflow(t, report, "standard-change@1")
	if got.PinnedVersion != 1 {
		t.Errorf("pinned_version = %d, want 1", got.PinnedVersion)
	}
	if got.CurrentVersion != 3 {
		t.Errorf("current_version = %d, want 3 — the registry's binding head", got.CurrentVersion)
	}
	if got.Behind != 2 {
		t.Errorf("behind = %d, want 2; without it a reader has to derive the "+
			"staleness from git before trusting anything in this report", got.Behind)
	}
}

// TestCurrentPinReportsZeroBehind is the falsifier: a report that always said
// "behind" would pass the test above and be useless.
func TestCurrentPinReportsZeroBehind(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	got := pinnedWorkflow(t, report, "standard-change@1")
	if got.Behind != 0 || got.CurrentVersion != 1 {
		t.Errorf("a run pinning the registry's head reports behind=%d current=%d, "+
			"want 0 and 1", got.Behind, got.CurrentVersion)
	}
}

// TestRetiredVersionsAreNotCorpusAdvance: a version registered and then RETIRED
// is not somewhere the run could have gone. Binding filters retirement out
// first (bindableDefinitions), so a staleness count that included it would send
// an operator to chase a version nothing can bind.
func TestRetiredVersionsAreNotCorpusAdvance(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	registerVersion(t, conn, 2)
	_, err := db.DeprecateWorkflow(conn, run.ProjectID, "standard-change", 2, nowMS)
	testsupport.Must(t, err, "DeprecateWorkflow: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	got := pinnedWorkflow(t, report, "standard-change@1")
	if got.Behind != 0 {
		t.Errorf("behind = %d over a RETIRED version, want 0", got.Behind)
	}
	if got.CurrentVersion != 1 {
		t.Errorf("current_version = %d, want 1 — the highest version that still binds",
			got.CurrentVersion)
	}
}

// TestARunThatNeverRepinnedHasNoEpochs. One agreement is not a timeline: the
// `pins` table already states what every step of such a run read, and a
// `pin_epoch: 1` on every step of every report in the store would be a column
// of constants.
func TestARunThatNeverRepinnedHasNoEpochs(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.PinEpochs) != 0 {
		t.Errorf("a run that never repinned carries %d epoch(s): %+v",
			len(report.PinEpochs), report.PinEpochs)
	}
	for _, a := range report.Attempts {
		if a.PinEpoch != 0 {
			t.Errorf("%s carries pin_epoch %d on a run with one agreement",
				a.Instance, a.PinEpoch)
		}
	}
}

// repinFixture is the RUN-39 shape: one pinned contract that a corpus install
// is about to replace mid-run. It returns the activated run, the pinned ref,
// the hash the run froze, and the config root the ref resolves under.
func repinFixture(t *testing.T, conn *sql.DB) (
	run *model.Run, ref, before, root string,
) {
	t.Helper()
	run, _ = activatedRun(t, conn)
	root = t.TempDir()
	ref = "contracts/implement.md"
	before = pinAFile(t, conn, run.ID, root, ref, "BEFORE\n")
	return run, ref, before, root
}

// TestCompletedStepReportsTheAgreementItRanUnder is criterion 2's first half:
// a step that finished BEFORE the repin says so, even though the pins table
// has since moved on.
func TestCompletedStepReportsTheAgreementItRanUnder(t *testing.T) {
	conn := mustDB(t)
	run, ref, before, root := repinFixture(t, conn)

	e := testEngine()
	implID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement@0: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, implID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: nowMS,
	}), "complete implement@0")

	// The corpus install, and the operator's recovery.
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, ref), []byte("AFTER\n"), 0o644), "replacing the contract")
	outcome, err := repinRunIn(conn, run.ID, "corpus install 2026-08-23", nowMS+1000,
		[]string{root})
	testsupport.Must(t, err, "repinRunIn: %v", err)
	if len(outcome.Repinned) != 1 {
		t.Fatalf("repinned %d pin(s), want 1: %+v", len(outcome.Repinned), outcome.Repinned)
	}

	report, err := LoadRunReport(conn, run.ID, nowMS+2000)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.PinEpochs) != 2 {
		t.Fatalf("the timeline has %d epoch(s), want activation + one repin: %+v",
			len(report.PinEpochs), report.PinEpochs)
	}
	if report.PinEpochs[0].Origin != PinEpochActivation {
		t.Errorf("epoch 1 origin = %q, want %q",
			report.PinEpochs[0].Origin, PinEpochActivation)
	}
	repin := report.PinEpochs[1]
	if repin.Epoch != 2 || repin.Origin != PinEpochRepin {
		t.Errorf("epoch 2 = %d %q, want 2 %q", repin.Epoch, repin.Origin, PinEpochRepin)
	}
	if repin.Reason != "corpus install 2026-08-23" {
		t.Errorf("epoch 2 reason = %q, want the operator's own", repin.Reason)
	}
	if len(repin.Changes) != 1 || repin.Changes[0].Ref != ref ||
		repin.Changes[0].OldSHA256 != before {
		t.Errorf("epoch 2 changes = %+v, want %s moving off %s",
			repin.Changes, ref, before)
	}

	// THE ACCEPTANCE CRITERION. The step completed before the repin's seq, so
	// the bytes it consumed are epoch 1's — which is the fact RUN-39's analysts
	// recovered by hand from seq numbers.
	if got := attemptFor(t, report, "implement@0"); got.PinEpoch != 1 {
		t.Errorf("implement@0 reports pin_epoch %d, want 1 — it recorded before "+
			"the repin, so it consumed the ORIGINAL bytes", got.PinEpoch)
	}
}

// TestStepRunAfterARepinReportsTheNewAgreement is the other half, and the
// falsifier for the test above: an epoch that were merely "the run's first"
// would pass there and be wrong here.
func TestStepRunAfterARepinReportsTheNewAgreement(t *testing.T) {
	conn := mustDB(t)
	run, ref, _, root := repinFixture(t, conn)

	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, ref), []byte("AFTER\n"), 0o644), "replacing the contract")
	_, err := repinRunIn(conn, run.ID, "corpus install", nowMS+1000, []string{root})
	testsupport.Must(t, err, "repinRunIn: %v", err)

	e := testEngine()
	implID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implID, ClaimOptions{Owner: "w", NowMS: nowMS + 2000})
	testsupport.Must(t, err, "claim implement@0: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, implID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: nowMS + 2000,
	}), "complete implement@0")

	report, err := LoadRunReport(conn, run.ID, nowMS+3000)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if got := attemptFor(t, report, "implement@0"); got.PinEpoch != 2 {
		t.Errorf("implement@0 reports pin_epoch %d, want 2 — it was claimed AFTER "+
			"the repin, so it consumed the new bytes", got.PinEpoch)
	}
}

// TestAStepThatHasNotRunCarriesNoEpoch. A pending step's agreement is whatever
// the pins table holds when it is finally claimed; stamping it with an epoch
// would answer a question about the future with a fact about the past.
func TestAStepThatHasNotRunCarriesNoEpoch(t *testing.T) {
	conn := mustDB(t)
	run, ref, _, root := repinFixture(t, conn)

	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, ref), []byte("AFTER\n"), 0o644), "replacing the contract")
	_, err := repinRunIn(conn, run.ID, "corpus install", nowMS+1000, []string{root})
	testsupport.Must(t, err, "repinRunIn: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS+2000)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	got := attemptFor(t, report, "implement@0")
	if got.Status != db.StepReady && got.Status != db.StepPending {
		t.Fatalf("implement@0 is %q; this test needs a step that has not run", got.Status)
	}
	if got.PinEpoch != 0 {
		t.Errorf("a %s step reports pin_epoch %d, want none", got.Status, got.PinEpoch)
	}
}
