package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Evidence references on findings (DKT-2451): a cast may cite
// `artifact:ARTIFACT-N` and `gate:<name>`, each resolved against the run the
// proposal was opened for BEFORE the cast records, and refused by name when
// it resolves against nothing.

// evidenceRun activates voteGateStatusSrc — a `seed` step emitting findings
// and a three-seat vote gate — records seed@0's artifact, opens the gate's
// proposal, and returns the run, the proposal, and the artifact's id.
func evidenceRun(t *testing.T, conn *sql.DB) (runID, proposalID, artifactID int) {
	t.Helper()
	runID = activatedVoteGateRun(t, conn)
	proposalID = openGateProposal(t, conn, testEngine(), runID)
	return runID, proposalID, artifactOf(t, conn, stepIDByInstance(t, conn, "seed@0"))
}

// secondEvidenceRun activates a SECOND run over the already-registered
// workflow, records its own seed@0, opens its own proposal, and returns the
// same three ids for it. Instances collide across runs, so every lookup here
// is run-scoped.
func secondEvidenceRun(t *testing.T, conn *sql.DB) (runID, proposalID, artifactID int) {
	t.Helper()
	e := testEngine()
	issue := createIssue(t, conn, "second subject", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate the second run: %v", err)

	claimAndCompleteInRun(t, conn, e, run.ID, "seed@0", "the second findings")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving the second run: %v", err)

	gate := stepInRun(t, conn, run.ID, "gate@0")
	proposalID, err = findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding the second gate's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for the second run's gate@0")
	}
	return run.ID, proposalID, artifactOf(t, conn, stepInRun(t, conn, run.ID, "seed@0").ID)
}

// stepInRun reads one run's step by instance — run-scoped, unlike
// stepIDByInstance, because two runs of one workflow share every instance.
func stepInRun(t *testing.T, conn *sql.DB, runID int, instance string) *db.Step {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND instance = ?`, runID, instance).Scan(&id)
	testsupport.Must(t, err, "finding %s in run %d: %v", instance, runID, err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "reading %s: %v", instance, err)
	return step
}

// artifactOf reads the newest artifact a step recorded.
func artifactOf(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM artifacts WHERE step_id = ? ORDER BY id DESC LIMIT 1`, stepID).Scan(&id)
	testsupport.Must(t, err, "reading the artifact of step %d: %v", stepID, err)
	return id
}

// recordGateResult inserts one gate result row for a step, the way the saga
// would after running the gate — the row is what `gate:<name>` resolves to.
func recordGateResult(t *testing.T, conn *sql.DB, runID, stepID int, gate string) {
	t.Helper()
	execSQL(t, conn,
		`INSERT INTO gate_results (run_id, step_id, gate, ordinal, verdict, created_at_ms)
		 VALUES (?, ?, ?, 0, 'pass', ?)`, runID, stepID, gate, nowMS)
}

// findingsCiting builds one blocker citing the given references.
func findingsCiting(refs ...string) *model.Findings {
	return &model.Findings{Blockers: []model.Finding{{Text: "reproduced", Evidence: refs}}}
}

// TestCastEvidenceResolvesAgainstTheProposalsRun is the accepting case: a
// cast citing an artifact the run holds and a gate the run recorded is
// admitted, the artifact reference is canonicalized, and the recorded cast
// carries both citations.
func TestCastEvidenceResolvesAgainstTheProposalsRun(t *testing.T) {
	conn := mustDB(t)
	runID, proposalID, artifactID := evidenceRun(t, conn)
	recordGateResult(t, conn, runID, stepIDByInstance(t, conn, "seed@0"), "build")

	// The bare numeric spelling is accepted on the way in...
	findings := findingsCiting("artifact:"+model.FormatArtifactID(artifactID)[len("ARTIFACT-"):], "gate:build")
	err := ValidateCastEvidence(conn, proposalID, findings)
	testsupport.Must(t, err, "ValidateCastEvidence: %v", err)

	// ...and canonicalized before anything stores it.
	want := "artifact:" + model.FormatArtifactID(artifactID)
	if got := findings.Blockers[0].Evidence; len(got) != 2 || got[0] != want || got[1] != "gate:build" {
		t.Fatalf("evidence after validation = %v, want [%s gate:build]", got, want)
	}

	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: "alice", Verdict: model.VerdictReject,
		Confidence: 0.9, DomainRelevance: 0.8, FindingsJSON: findings,
	})
	testsupport.Must(t, err, "CastVote: %v", err)
	votes, err := db.GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	if len(votes) != 1 || votes[0].FindingsJSON == nil {
		t.Fatalf("the cast did not record its structured findings: %+v", votes)
	}
	if got := votes[0].FindingsJSON.Blockers[0].Evidence; len(got) != 2 || got[0] != want {
		t.Errorf("recorded evidence = %v, want it to lead with %s", got, want)
	}
}

// TestCastEvidenceRefusesAnotherRunsArtifact is the refusing case the issue
// names: an artifact exists, but in a different run from the one the
// proposal was opened for, and the refusal names both runs.
func TestCastEvidenceRefusesAnotherRunsArtifact(t *testing.T) {
	conn := mustDB(t)
	firstRun, firstProposal, _ := evidenceRun(t, conn)
	secondRun, _, secondArtifact := secondEvidenceRun(t, conn)

	err := ValidateCastEvidence(conn, firstProposal,
		findingsCiting("artifact:"+model.FormatArtifactID(secondArtifact)))
	if err == nil {
		t.Fatal("an artifact of another run was accepted as evidence")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	for _, needle := range []string{
		model.FormatArtifactID(secondArtifact),
		model.FormatRunID(secondRun), model.FormatRunID(firstRun),
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("refusal %q does not name %s", err, needle)
		}
	}
}

// TestCastEvidenceRefusesWhatDoesNotResolve covers the other unresolvable
// shapes, each refused as VALIDATION and naming the offending reference.
func TestCastEvidenceRefusesWhatDoesNotResolve(t *testing.T) {
	conn := mustDB(t)
	_, proposalID, artifactID := evidenceRun(t, conn)

	for _, tc := range []struct{ name, ref string }{
		{"an artifact nobody recorded", "artifact:ARTIFACT-" + model.FormatArtifactID(artifactID + 100)[len("ARTIFACT-"):]},
		{"a gate the run never ran", "gate:vuln-scan"},
		{"an unknown scheme", "step:STEP-1"},
		{"a reference with no scheme", "ARTIFACT-1"},
		{"a scheme with nothing after it", "gate:"},
		{"a malformed artifact id", "artifact:ARTIFACT-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCastEvidence(conn, proposalID, findingsCiting(tc.ref))
			if err == nil {
				t.Fatalf("%q was accepted as evidence", tc.ref)
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %q, want %q", code, CodeValidation)
			}
			if !strings.Contains(err.Error(), tc.ref) {
				t.Errorf("refusal %q does not name the reference %q", err, tc.ref)
			}
		})
	}
}

// TestCastEvidenceRefusesAnUnboundProposal: a proposal opened by hand — no
// vote step, no reap acknowledgment — has no run to resolve against, so its
// casts carry no evidence, and the refusal says so. A proposal that does not
// exist is NOT_FOUND, as it is for every other verb.
func TestCastEvidenceRefusesAnUnboundProposal(t *testing.T) {
	conn := mustDB(t)
	_, _, artifactID := evidenceRun(t, conn)

	adHoc, err := db.CreateProposal(conn, &model.Proposal{
		Description: "an operator's own ballot", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	err = ValidateCastEvidence(conn, adHoc, findingsCiting("artifact:"+model.FormatArtifactID(artifactID)))
	if err == nil {
		t.Fatal("evidence on a proposal bound to no run was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), model.FormatProposalID(adHoc)) {
		t.Errorf("refusal %q does not name the proposal", err)
	}

	err = ValidateCastEvidence(conn, adHoc+1000, findingsCiting("gate:build"))
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("a missing proposal returned %v, want %s", err, CodeNotFound)
	}
}

// TestFindingsCitingNothingNeedNoRun: findings without evidence — nil, or
// entries that cite nothing — pass with no read at all, so an ordinary cast
// is byte-for-byte the cast it always was, on a proposal that does not even
// exist.
func TestFindingsCitingNothingNeedNoRun(t *testing.T) {
	conn := mustDB(t)
	for _, f := range []*model.Findings{
		nil,
		{},
		{Concerns: []model.Finding{{Text: "asserted, not reproduced"}}},
	} {
		if err := ValidateCastEvidence(conn, 424242, f); err != nil {
			t.Errorf("findings %+v citing nothing were refused: %v", f, err)
		}
	}
}
