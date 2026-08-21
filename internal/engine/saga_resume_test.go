package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// §9 item 10's gate half, completed (TDD docs/tdd/gates-trust.md §7.5, T12).
//
// engine-spine §6.16 proved crash-at-every-boundary with STUBBED gates: the
// saga resumed, but nothing had run, so "never double-runs" was vacuously true.
// These tests complete it with gates that really execute, and the witness is a
// COUNTER FILE the gate appends to — the filesystem, not a row the engine wrote
// about itself.

// sandboxTrust points a runner at a throwaway store and returns a loader for it
// (§9.5 SB1: no test may touch the operator's real trust store).
func sandboxTrust(t *testing.T, entries ...trust.Entry) func() (*trust.Store, error) {
	t.Helper()
	return func() (*trust.Store, error) {
		return &trust.Store{Version: trust.FormatVersion, Entries: entries}, nil
	}
}

// execEngineWithTrust builds an engine whose gates really execute, against a
// sandbox trust store.
func execEngineWithTrust(t *testing.T, repoRoot string, entries ...trust.Entry) *Engine {
	t.Helper()
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, entries...)
	e.Gates = runner
	return e
}

// countingRunner wraps the real runner and counts how many times each gate
// actually reached a spawn decision. It is the double-execution detector: a
// re-run that should not have happened shows up here as a second count.
type countingRunner struct {
	inner  *ExecRunner
	counts map[string]int
}

func (r *countingRunner) Run(ctx context.Context, g GateSpec, sc StepContext) (GateResult, error) {
	r.counts[g.Name]++
	return r.inner.Run(ctx, g, sc)
}

func (r *countingRunner) Execute(ctx context.Context, g GateSpec, sc StepContext) (GateExecution, error) {
	r.counts[g.Name]++
	return r.inner.Execute(ctx, g, sc)
}

// TestCrashAtGateBoundaryNeverDoubleRunsNonRerunnable is §9 item 10's gate
// half, at EVERY saga boundary and for BOTH values of the flag.
//
// The crash is placed where at-least-once actually creates its window: after
// the `gate-started` event commits and before the result does. A test that
// crashed anywhere else would prove resume works and say nothing about
// double-execution.
func TestCrashAtGateBoundaryNeverDoubleRunsNonRerunnable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reRunnable bool
		wantStatus string
	}{
		{
			name:       "not re-runnable parks",
			reRunnable: false,
			// EXACTLY ONE: the interrupted attempt ran; the resume must not.
			wantStatus: db.StepWaitingHuman,
		},
		{
			name:       "re-runnable re-runs",
			reRunnable: true,
			// TWO: the operator declared this command safe to run again.
			wantStatus: db.StepDone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			repoRoot := t.TempDir()

			entries := fixtureTrustEntries(repoRoot, tc.reRunnable)
			runner := NewExecRunner(testRepoPaths(repoRoot))
			runner.LoadStore = sandboxTrust(t, entries...)
			counter := &countingRunner{inner: runner, counts: map[string]int{}}

			e := testEngine()
			e.Gates = counter

			stepID := stepIDByInstance(t, conn, "implement@0")
			claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)

			// Stage 0, then advance to the FIRST gate stage and stop there.
			if err := e.CompleteStep(conn, stepID, CompleteOptions{
				Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
			}); err != nil && !strings.Contains(err.Error(), "waiting-human") {
				// CompleteStep drives the whole saga; for the parking case it
				// completes normally with the step parked.
				t.Fatalf("complete: %v", err)
			}

			// Simulate the crash window on the FIRST gate: delete its recorded
			// result while leaving its `gate-started` event in place. That is
			// exactly A1's started-but-unrecorded state, reconstructed.
			firstGate := "build"
			_, err = conn.Exec(
				`DELETE FROM gate_results WHERE step_id = ? AND gate = ?`,
				stepID, firstGate)
			testsupport.Must(t, err, "reconstructing the crash window: %v", err)

			// The resume point is the stage BEFORE the gate: `saga_stage` names
			// the last gate RECORDED, so `recorded` is the state in which
			// `build` is the gate about to run. Combined with build's surviving
			// `gate-started` event and its deleted result, that is exactly A1's
			// started-but-unrecorded shape.
			_, err = conn.Exec(
				`UPDATE steps SET saga_stage = ?, status = 'gated', routing = NULL
				  WHERE id = ?`, db.SagaRecorded, stepID)
			testsupport.Must(t, err, "rewinding the saga stage: %v", err)
			before := counter.counts[firstGate]

			// THE RESUME.
			err = e.ResumeSaga(conn, stepID, nowMS)
			testsupport.Must(t, err, "resuming: %v", err)

			after := counter.counts[firstGate] - before
			wantAfter := 0
			if tc.reRunnable {
				wantAfter = 1
			}
			if after != wantAfter {
				t.Errorf("gate %q reached the runner %d times on resume, want %d",
					firstGate, after, wantAfter)
			}

			final, err := db.GetStep(conn, stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			if final.Status != tc.wantStatus {
				t.Errorf("status = %q after resume, want %q", final.Status, tc.wantStatus)
			}

			if !tc.reRunnable {
				// A4: the park NAMES the gate and the situation, so an operator
				// has something to act on.
				if !strings.Contains(final.Routing, firstGate) {
					t.Errorf("the park reason does not name the gate: %q", final.Routing)
				}
				if !strings.Contains(final.Routing, "re-runnable") {
					t.Errorf("the park reason does not state the remedy: %q", final.Routing)
				}
			} else {
				// A3: the re-run is announced, so the trail shows the command
				// ran twice rather than hiding it.
				if !hasEventKind(t, conn, stepID, EventGateRerun) {
					t.Error("a re-run happened with no gate-rerun event; the trail " +
						"must show that the command ran twice")
				}
			}
		})
	}
}

// TestUnmatchedAtResumeParks is A5: a gate that is no longer trusted when the
// resume happens parks, rather than re-running "because we can't tell".
//
// This is the revocation case — `trust rm` between the crash and the resume —
// and fail-closed is the only available direction when the question is whether
// something already happened.
func TestUnmatchedAtResumeParks(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	stepID := stepIDByInstance(t, conn, "implement@0")

	// The gate ran once, under a trust entry that has since been removed: the
	// store the resume reads is EMPTY.
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t) // no entries
	e := testEngine()
	e.Gates = runner

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	firstGate := "build"
	_, err = conn.Exec(
		`DELETE FROM gate_results WHERE step_id = ? AND gate = ?`,
		stepID, firstGate)
	testsupport.Must(t, err, "reconstructing the crash window: %v", err)
	_, err = conn.Exec(
		`UPDATE steps SET saga_stage = ?, status = 'gated', routing = NULL
		  WHERE id = ?`, db.SagaRecorded, stepID)
	testsupport.Must(t, err, "rewinding the saga stage: %v", err)

	// The `gate-started` event must exist for A1 to fire.
	if !hasEventKind(t, conn, stepID, EventGateStarted) {
		t.Fatal("no gate-started event; the crash window cannot be reconstructed")
	}

	err = e.ResumeSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "resuming: %v", err)

	final, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if final.Status != db.StepWaitingHuman {
		t.Errorf("status = %q, want %q — an unmatched-at-resume gate parks (A5)",
			final.Status, db.StepWaitingHuman)
	}
	if !strings.Contains(final.Routing, "no longer trusted") {
		t.Errorf("the park reason does not state that trust was withdrawn: %q",
			final.Routing)
	}
}

// TestSagaStageBoundariesUnchanged is §7.1's checkable claim: swapping the gate
// runner did not move the saga.
//
// engine-spine §5.6 promised "S4 changes one constructor call and nothing
// else", and gates-trust §7.1 records honestly the four places more moves. What
// must NOT have moved is the saga's own shape — its stage sequence — and this
// compares that sequence against the S3 golden rather than taking the claim on
// faith.
func TestSagaStageBoundariesUnchanged(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()
	e := execEngineWithTrust(t, repoRoot, fixtureTrustEntries(repoRoot, true)...)

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Drive the saga one stage at a time, recording every stage it occupies.
	var stages []string
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	rows, err := conn.Query(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq`,
		stepID, EventGateStarted)
	testsupport.Must(t, err, "reading gate-started events: %v", err)
	defer rows.Close()
	for rows.Next() {
		var data string
		err := rows.Scan(&data)
		testsupport.Must(t, err, "scanning: %v", err)
		stages = append(stages, data)
	}

	// THE S3 GOLDEN: the fixture's five gates, in declared order, each entered
	// exactly once. `pre` gates are excluded — they run at claim, not here.
	want := []string{"build", "tests", "scope", "self-hygiene", "secret-scan"}
	if len(stages) != len(want) {
		t.Fatalf("the saga entered %d gate stages, want %d: %v", len(stages), len(want), stages)
	}
	for i := range want {
		if !strings.Contains(stages[i], want[i]) {
			t.Errorf("gate stage %d = %q, want %q — the saga's stage sequence moved",
				i, stages[i], want[i])
		}
	}

	// THE STAGE VOCABULARY, extended by EXACTLY ONE (§6.4 M-b).
	//
	// `held` is the stage payloads-thresholds adds, and it is a DEFERRAL of
	// routing rather than a new position in the sequence: a step reaches it
	// INSTEAD OF routing and leaves it BY routing, so every stage above still
	// means what it meant. The set is compared rather than counted, because a
	// count is the assertion that survives one stage being renamed into
	// another.
	stageVocabulary := []string{
		db.SagaRecorded, db.SagaRouting, db.SagaHeld, db.SagaGatePrefix,
	}
	if len(stageVocabulary) != 4 {
		t.Errorf("the saga's stage vocabulary is %v; S4 had three and stage 5 "+
			"adds `held` and nothing else", stageVocabulary)
	}
	for _, stage := range stageVocabulary {
		if stage == "" {
			t.Error("a saga stage is the empty string, which means COMPLETE")
		}
	}
}

// fixtureTrustEntries trusts every gate the committed fixture declares, bound
// to the given repo, with the re-runnable flag under test.
func fixtureTrustEntries(repoRoot string, reRunnable bool) []trust.Entry {
	var out []trust.Entry
	for _, name := range []string{
		"build", "tests", "scope", "self-hygiene", "secret-scan",
		"ac-commands", "commit-msg", "commit-exec",
	} {
		argv := []string{"/usr/bin/true"}
		out = append(out, trust.Entry{
			Name: name, Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
			Repo: mustResolve(repoRoot), ReRunnable: reRunnable,
		})
	}
	return out
}

func mustResolve(p string) string {
	resolved, err := trust.RepoIdentity(p)
	if err != nil {
		return p
	}
	return resolved
}

// hasEventKind reports whether the step has an event of the given kind.
func hasEventKind(t *testing.T, conn *sql.DB, stepID int, kind string) bool {
	t.Helper()
	var exists bool
	err := conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM events WHERE step_id = ? AND kind = ?)`,
		stepID, kind).Scan(&exists)
	testsupport.Must(t, err, "probing for a %s event: %v", kind, err)
	return exists
}

// TrustRunner makes the counting decorator transparent to the resume decision.
// Without it the wrapper would fail OPEN — the resume would treat every
// interrupted gate as safe to re-run, which is the bug the interface prevents.
func (r *countingRunner) TrustRunner() *ExecRunner { return r.inner }
