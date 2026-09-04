package engine

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Pre-gates at claim (TDD docs/tdd/gates-trust.md §7.6, T14).
//
// The fixture's `verify` step declares `gates = [{ name = "ac-commands",
// pre = true }]`, so it is the step every case here claims.

// TestPreGatesUseTheSameTrustPath is T14: an UNTRUSTED pre-gate does not
// execute, and the claim still succeeds with the result recorded `unmatched`.
//
// T14 is the "pre-gate as a bypass" threat: pre-gates run at claim, earlier in
// the lifecycle and on a different code path than the saga's gates, which is
// exactly the shape an implementation "simplifies" into a trusted path. The
// only differences the design permits are WHEN they run and WHERE the result
// goes.
func TestPreGatesUseTheSameTrustPath(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// An EMPTY trust store: the pre-gate is unmatched.
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t)
	e := testEngine()
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.
		// PG2: THE CLAIM STILL SUCCEEDS. Refusing it would let an untrusted
		// command block work — a denial of service an issue author should not have.
		Must(t, err, "an unmatched pre-gate must not refuse the claim: %v", err)

	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("bundle carries %d pre-gate results, want 1",
			len(claim.Context.PreGates))
	}
	got := claim.Context.PreGates[0]
	if got.Verdict != VerdictUnmatched {
		t.Errorf("verdict = %q, want %q", got.Verdict, VerdictUnmatched)
	}
	// The honest encoding: nothing ran, so there is no argv and no exit code.
	if got.Argv != nil {
		t.Errorf("argv = %v on an unmatched pre-gate, want nil", got.Argv)
	}
	if got.Exit != nil {
		t.Errorf("exit = %v on an unmatched pre-gate, want nil", got.Exit)
	}
	if got.Reason == "" {
		t.Error("an unmatched pre-gate carries no reason; the operator cannot " +
			"tell which of the four causes applies")
	}

	// It is recorded with `pre = 1`, which is what PG4's read-side filter needs.
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(rows) != 1 || !rows[0].Pre {
		t.Errorf("recorded rows = %+v, want exactly one marked pre", rows)
	}
}

// TestFailingPreGateDoesNotRefuseTheClaim is PG3: a pre-gate that RUNS and
// fails is data, not a refusal.
//
// §11.1 calls these "measure-then-judge steps" — the judging is the step's job,
// which is exactly why the failure rides in the bundle rather than blocking.
func TestFailingPreGateDoesNotRefuseTheClaim(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	argv := []string{"/usr/bin/false"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "a failing pre-gate must not refuse the claim: %v", err)
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("bundle carries %d pre-gate results, want 1", len(claim.Context.PreGates))
	}
	got := claim.Context.PreGates[0]
	if got.Verdict != VerdictFail {
		t.Errorf("verdict = %q, want %q", got.Verdict, VerdictFail)
	}
	// It RAN: a real exit code, unlike the unmatched case.
	if got.Exit == nil || *got.Exit == 0 {
		t.Errorf("exit = %v, want a non-zero code from a command that ran", got.Exit)
	}
}

// TestPreGateResultsAreExcludedFromTheSagaVerdict is PG4: pre-gate results are
// INPUTS to the step, not judgments of it.
//
// A failing pre-gate must not fail the step's completion — that decision
// belongs to the step's own artifact and threshold. S3 already excludes `pre`
// gates from completionGates; this is the read-side counterpart, and without it
// a measurement would silently become a verdict.
func TestPreGateResultsAreExcludedFromTheSagaVerdict(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")

	// The step is read BEFORE the transaction opens: internal/db caps the pool
	// at one connection, so a pool read while a transaction holds it deadlocks
	// permanently rather than failing.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	// A FAILING pre-gate row, recorded directly, alongside nothing else.
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	exit := 1
	err = db.InsertGateResultTx(tx, db.GateResultRow{
		RunID: step.RunID, StepID: stepID, Gate: "ac-commands", Ordinal: 0,
		Argv: []string{"/usr/bin/false"}, Exit: &exit,
		Verdict: db.GateVerdictFail, Pre: true, CreatedAtMS: nowMS,
	})
	testsupport.Must(t, err, "InsertGateResultTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	verdict, unmeasured, err := gateVerdict(conn, stepID)
	testsupport.Must(t, err, "gateVerdict: %v", err)
	if verdict != VerdictPass {
		t.Errorf("gateVerdict = %q with only a failing PRE-gate recorded, want %q — "+
			"a pre-gate is a measurement the step consumes, not a judgment of it",
			verdict, VerdictPass)
	}
	// PG4 applies to the unmeasured list too (DKT-254). A pre-gate that could
	// not bind its tree is data for the step's worker; parking the step on it
	// would be the engine judging the step by an input it was handed.
	if len(unmeasured) != 0 {
		t.Errorf("gateVerdict reported %v as unmeasured from PRE-gate rows; "+
			"PG4 excludes pre-gates from the step's own routing", unmeasured)
	}
}

// TestClaimRemainsSingleWinnerAcrossThePreGatePhase is §7.6.1's race: the
// phase split must not weaken mutual exclusion.
//
// Exclusion is decided in transaction A and only there. N goroutines claim one
// step with pre-gates; exactly one wins.
func TestClaimRemainsSingleWinnerAcrossThePreGatePhase(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	argv := []string{"/usr/bin/true"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	const claimants = 6
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	for i := range claimants {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
				Owner: "w", NowMS: nowMS,
			})
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d claimants won, want exactly 1 — the phase split must not "+
			"weaken mutual exclusion", winners)
	}
}

// TestPreGateWallTimeDoesNotConsumeTheLease is §7.6.1.1 LR1: the caller gets a
// FULL lease however long phase 2 took.
//
// Without the refresh, the pre-gate's wall time is deducted from a lease the
// caller has not yet received, and with a short TTL the caller is handed an
// already-expired one — its first `step complete` then fails on a lease it
// never had a chance to use.
func TestPreGateWallTimeDoesNotConsumeTheLease(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// A pre-gate that takes real wall time, under a TTL SHORTER than it.
	argv := []string{"/bin/sleep", "1"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	const shortTTL = 500 // ms — deliberately shorter than the pre-gate
	claimAt := nowMS
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", TTLOverride: shortTTL, NowMS: claimAt,
	})
	testsupport.Must(t, err, "claim: %v", err)

	// THE ASSERTION: the lease is a full TTL ahead of the claim's own clock,
	// not eaten by the pre-gate. Both halves fail without LR1.
	if claim.LeaseExpiresMS < claimAt+shortTTL {
		t.Errorf("lease expires at %d, want at least %d (a full TTL) — "+
			"the pre-gate's wall time was deducted from the caller's lease",
			claim.LeaseExpiresMS, claimAt+shortTTL)
	}

	// And the stored lease agrees, so the worker's next call sees the same.
	lease, err := db.GetStepLease(conn, stepID)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if lease.ExpiresMS < claimAt+shortTTL {
		t.Errorf("stored expires_ms = %d, want at least %d",
			lease.ExpiresMS, claimAt+shortTTL)
	}
}

// TestPreGateGoldenSplit is GD1-GD3 (§7.6.3): a bundle WITHOUT pre-gates omits
// the member entirely, and one WITH them carries a structurally-asserted array.
//
// GD3 is the argument a reviewer should check hardest, so the test states it:
// §9 item 5's determinism requires bundles immune to "mid-run issue edits and
// working-tree changes", and a pre-gate exists precisely to MEASURE the working
// tree at claim time. Requiring its output to be immune would require it to not
// measure anything. So the golden covers the bundle with `pre_gates` elided,
// plus a structural assertion that excludes duration and output.
func TestPreGateGoldenSplit(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// GD1: a step WITHOUT pre-gates. The member is ABSENT, not empty — the
	// rule the v6 `lease` object established.
	e := execEngineWithTrust(t, repoRoot, fixtureTrustEntries(repoRoot, true)...)
	plainID := stepIDByInstance(t, conn, "implement@0")
	plain, err := e.ClaimStepWithGates(conn, plainID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	if plain.Context.PreGates != nil {
		t.Errorf("a step with no pre-gates carries pre_gates = %v, want absent",
			plain.Context.PreGates)
	}
	encoded, err := json.Marshal(plain.Context)
	testsupport.Must(t, err, "encoding the bundle: %v", err)
	if strings.Contains(string(encoded), "pre_gates") {
		t.Errorf("pre_gates appears in a bundle for a step that declares none: %s", encoded)
	}
}

// TestPreGateRecordsBothTheRowAndTheEvent is §7.6.1 phase 2's "exactly as the
// saga's do", asserted on the half that was missing.
//
// A pre-gate announced `gate-started` and then recorded its row silently: the
// feed showed a gate that began and never finished, which is precisely the
// crash signature `gate-started` exists to make visible, produced here by a
// gate that ran fine. `ac-commands` in the live store held seven `gate-started`
// events, seven passing rows in `gate_results`, and zero `gate-recorded`.
//
// The ROW-AND-EVENT pairing is the assertion, not either half alone. A result
// with no closing event is the defect, and a test that checked only the count
// of one kind would have passed throughout.
func TestPreGateRecordsBothTheRowAndTheEvent(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// A pre-gate that MATCHES and PASSES — the case that emitted nothing.
	argv := []string{"/usr/bin/true"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "claim: %v", err)
	if len(claim.Context.PreGates) != 1 ||
		claim.Context.PreGates[0].Verdict != VerdictPass {
		t.Fatalf("pre-gate results = %+v, want one passing result",
			claim.Context.PreGates)
	}

	// The row landed.
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(rows) != 1 || !rows[0].Pre || rows[0].Verdict != db.GateVerdictPass {
		t.Fatalf("recorded rows = %+v, want exactly one passing pre-gate row", rows)
	}

	// AND the event. Without it the gate_results row is the only evidence the
	// gate ever finished, and `docket run report` disagrees with the feed.
	if !hasEventKind(t, conn, stepID, EventGateRecorded) {
		t.Error("a recorded pre-gate result wrote no gate-recorded event: the " +
			"feed shows a gate that started and never closed (§7.6.1 phase 2)")
	}

	// The ORDER holds here exactly as it does in the saga: the announcement
	// commits before the spawn, the record after it.
	started := eventSeqs(t, conn, EventGateStarted)
	recorded := eventSeqs(t, conn, EventGateRecorded)
	if len(started) != len(recorded) {
		t.Fatalf("%d gate-started and %d gate-recorded events, want them paired",
			len(started), len(recorded))
	}
	for i := range started {
		if started[i] >= recorded[i] {
			t.Errorf("gate %d: gate-started seq %d is not before gate-recorded seq %d",
				i, started[i], recorded[i])
		}
	}

	// A gate that ran is not a gate that was refused: `gate-unmatched` stays
	// separate (§6.4), so the added event must not have been bought by
	// emitting the wrong kind.
	if hasEventKind(t, conn, stepID, EventGateUnmatched) {
		t.Error("a matched, passing pre-gate wrote gate-unmatched")
	}
}

// TestUnmatchedPreGateKeepsBothItsEvents guards the kind §6.4 keeps separate.
//
// `gate-unmatched` and `gate-recorded` are not alternatives — the first says
// the command was refused, the second says the outcome is now recorded, and an
// operator following the feed needs both. Adding the second must not cost the
// first, which is the regression a shared writer could plausibly introduce.
func TestUnmatchedPreGateKeepsBothItsEvents(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// An EMPTY trust store: the pre-gate is unmatched and never executes.
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t)
	e := testEngine()
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)

	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "claim: %v", err)

	for _, kind := range []string{EventGateUnmatched, EventGateRecorded} {
		if !hasEventKind(t, conn, stepID, kind) {
			t.Errorf("an unmatched pre-gate wrote no %s event", kind)
		}
	}
}

// TestPreGateEventsMarkTheVerdictThatRoutedNothing is DKT-862's events half.
//
// `gate-recorded ... detail=ac-commands exit=2 verdict=fail` said the same
// thing whether the failure BLOCKED the step or was an advisory input to it —
// and a pre-gate never routes: §11.1 runs it at claim, PG4 keeps it out of the
// saga's verdict. On RUN-61 three pre-gate failures sat in the feed beside the
// `step-routed pass` that contradicted them, and a conductor nearly reported a
// fix round as burned on one.
//
// The assertion is on the STORED PAYLOAD, because that is what both readers
// see: `events list` renders `data` as sorted key=value pairs and interprets
// nothing, and `--json` hands the object straight to a program.
func TestPreGateEventsMarkTheVerdictThatRoutedNothing(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// A pre-gate that matches and FAILS — RUN-61's shape. `/usr/bin/false`
	// exits 1, so the row is a `fail` that routed nothing.
	argv := []string{"/usr/bin/false"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)
	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "an advisory pre-gate failure must not refuse the claim: %v", err)

	data := gateRecordedData(t, conn, stepID, "ac-commands")
	if data["verdict"] != VerdictFail {
		t.Fatalf("the pre-gate's gate-recorded verdict = %v, want %q — this "+
			"case is about a FAILING advisory gate", data["verdict"], VerdictFail)
	}
	if data["pre"] != true {
		t.Errorf("gate-recorded for a pre-gate carries pre = %v, want true; "+
			"payload %v renders as a blocking failure in `events list`",
			data["pre"], data)
	}

	// The marker is not bought by dropping what was already there (DKT-63).
	if data["detail"] != "ac-commands" {
		t.Errorf("the gate name left `detail`: %v", data)
	}

	// AND a BLOCKING gate stays unmarked. `implement@0`'s gates run through
	// advanceToVerify's pass-through runner and route the step for real, so
	// their absence of a `pre` key is what makes the pre-gate's presence mean
	// something rather than being a field every gate event carries.
	implementID := stepIDByInstance(t, conn, "implement@0")
	for _, blocking := range []string{"build", "tests"} {
		// gateRecordedData FATALS on a missing event, which is what keeps this
		// half from passing vacuously: a payload that carries no `pre` key
		// because it was never written proves nothing.
		if _, ok := gateRecordedData(t, conn, implementID, blocking)["pre"]; ok {
			t.Errorf("gate-recorded for the blocking gate %q carries a pre "+
				"marker; only a gate that routed nothing may be marked", blocking)
		}
	}
}

// TestGateRecordedEventCarriesStubMarker is DKT-983: a stub-trusted command's
// pass is already marked stub:true in the trust store, in `gate_results` rows
// (`step gates --json`), and in `run report` — but the `gate-recorded` EVENT
// carried none of it, so a stub pass was byte-identical to a real measurement
// on the event stream. RUN-63 / vorpal.git seq 13052 is this exactly:
// `gate-recorded ... detail=ac-commands exit=0 pre=true verdict=pass` beside a
// `step gates --json` that showed `stub=true` for the same record.
//
// The assertion is on the STORED PAYLOAD, exactly as
// TestPreGateEventsMarkTheVerdictThatRoutedNothing's is, because that is what
// both readers see: `events list` renders `data` as sorted key=value pairs,
// and `--json` hands the object straight to a program.
func TestGateRecordedEventCarriesStubMarker(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()

	// A trust entry that declares `stub = true` (DKT-265): the command that
	// runs is a placeholder, not the check its name implies.
	argv := []string{"/usr/bin/true"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot), Stub: true,
	})
	e.Gates = runner

	stepID := advanceToVerify(t, conn, e)
	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "claim: %v", err)

	// The row already carries it (this is not new — it is the baseline the
	// event is being brought up to).
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(rows) != 1 || !rows[0].StubEntry {
		t.Fatalf("recorded rows = %+v, want exactly one stub-marked row", rows)
	}

	data := gateRecordedData(t, conn, stepID, "ac-commands")
	if data["stub"] != true {
		t.Errorf("gate-recorded for a stub-trusted gate carries stub = %v, "+
			"want true; payload %v is indistinguishable from a real measurement "+
			"on the event stream", data["stub"], data)
	}
	// The marker is not bought by dropping what was already there (DKT-63).
	if data["verdict"] != VerdictPass {
		t.Errorf("verdict = %v, want %q — the stub marker must not change the "+
			"outcome it decorates", data["verdict"], VerdictPass)
	}
	if data["detail"] != "ac-commands" {
		t.Errorf("the gate name left `detail`: %v", data)
	}

	// AND a NON-stub gate stays unmarked: implement@0's gates run through
	// advanceToVerify's pass-through runner, whose stub marker is a different
	// field entirely (the legacy S3-migration `Stub`, never set by any live
	// path) — so their `gate-recorded` events must carry no `stub` key.
	implementID := stepIDByInstance(t, conn, "implement@0")
	for _, blocking := range []string{"build", "tests"} {
		if _, ok := gateRecordedData(t, conn, implementID, blocking)["stub"]; ok {
			t.Errorf("gate-recorded for the non-stub gate %q carries a stub "+
				"marker; only a gate whose trust entry declared `stub` may be "+
				"marked", blocking)
		}
	}
}

// gateRecordedData reads the `gate-recorded` payload for one step's gate, and
// fails the test when there is none.
func gateRecordedData(
	t *testing.T, conn *sql.DB, stepID int, gate string,
) map[string]any {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq`,
		stepID, EventGateRecorded)
	testsupport.Must(t, err, "reading gate-recorded events: %v", err)
	defer rows.Close()

	for rows.Next() {
		var raw string
		testsupport.Must(t, rows.Scan(&raw), "scanning a gate-recorded event: %v", nil)
		var fields map[string]any
		testsupport.Must(t, json.Unmarshal([]byte(raw), &fields),
			"the gate event payload is not an object: %v", nil)
		if fields["detail"] == gate {
			return fields
		}
	}
	t.Fatalf("no gate-recorded event for gate %q on step %d", gate, stepID)
	return nil
}

// advanceToVerify drives the fixture's run until `verify@0` — the step that
// declares the `ac-commands` pre-gate — is claimable, and returns its id.
//
// The intermediate steps complete through a PASS-THROUGH runner: this file's
// subject is the pre-gate at `verify`, and letting the earlier steps' own gates
// go unmatched would park the run before it ever gets there.
func advanceToVerify(t *testing.T, conn *sql.DB, e *Engine) int {
	t.Helper()

	pass := testEngine() // pass-through gates
	pass.DiffFn = e.DiffFn

	claimAndComplete(t, conn, pass, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, pass, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, pass, "synthesize@0", "synthesized", "")
	// `reconcile` is an action step: the engine runs it, no claim (§6.15).
	driveAction(t, conn, pass, "reconcile@0")

	return stepIDByInstance(t, conn, "verify@0")
}

// ---------------------------------------------------------------------------
// DKT-254 — a pre-gate binds the tree under review, or measures nothing
// ---------------------------------------------------------------------------

// seedGitRepo builds a repository holding ONE commit with one named file, and
// returns that commit's sha.
//
// The file is the witness: a gate that finds it is running in a tree at that
// commit, and a gate running anywhere else is not. That is what lets the
// reconstruction cases prove WHICH tree was measured rather than merely that
// something was.
func seedGitRepo(t *testing.T, dir, name, body string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitRun(t, dir, "init", "-q")
	err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644)
	testsupport.Must(t, err, "writing %s: %v", name, err)
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-q", "-m", "the change under review")
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// setRunExecRoot points the run at a checkout, which is the object database
// reconstructTarget reads from — the same resolution `runExecRoot` performs for
// the diff stage (DKT-11).
func setRunExecRoot(t *testing.T, conn *sql.DB, runID int, execRoot string) {
	t.Helper()
	_, err := conn.Exec(`UPDATE runs SET exec_root = ? WHERE id = ?`, execRoot, runID)
	testsupport.Must(t, err, "setting the run's exec root: %v", err)
}

// preGateStep seeds the fixture's `verify` step — the one that declares a
// `pre = true` gate — and returns it loaded, so a case can drive runPreGates
// directly with a target of its own choosing.
//
// Driving runPreGates rather than the whole claim is deliberate: what mode 1
// and mode 2 are about is which TREE the phase binds, and the claim's own
// resolution would substitute its answer for the one under test.
func preGateStep(t *testing.T, conn *sql.DB, e *Engine) *db.Step {
	t.Helper()
	stepID := advanceToVerify(t, conn, e)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep(%d): %v", stepID, err)
	return step
}

// TestUnbindableTargetSkipsRatherThanMeasuringTheSharedCheckout is mode 1.
//
// RUN-2's ac-commands pre-gate recorded PASS with commit 76f5d0c — the SHARED
// CHECKOUT's HEAD — while the sha actually under review was 2b9d9c8, living in
// a private worktree. The PASS was a recorded verdict with zero evidence value:
// nothing it measured was the work being gated, and nothing said so.
//
// The witness is a SENTINEL. "Nothing executed" is only provable from outside
// the engine: a `skipped` row is the engine's own account of itself, while a
// file that does not exist is the filesystem's.
func TestUnbindableTargetSkipsRatherThanMeasuringTheSharedCheckout(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "pregate-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)

	// A sha under review that is in no object database anywhere, so neither a
	// resolved worktree nor a reconstruction can serve it.
	const unreachable = "2b9d9c8000000000000000000000000000000000"

	got, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}}, unreachable, "", nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)

	if sentinelExists(t, sentinel) {
		t.Error("the pre-gate EXECUTED against the shared checkout; a verdict " +
			"measured in a tree other than the one under review is not evidence " +
			"about the change, however green it comes back")
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Verdict != VerdictSkipped {
		t.Errorf("verdict = %q, want %q — the trust entry matched and the "+
			"command would have run; what is missing is the SUBJECT",
			got[0].Verdict, VerdictSkipped)
	}
	// The reason must name the sha: that is what an operator goes and fetches.
	// A generic "could not measure" sends them to look at the gate instead.
	if !strings.Contains(got[0].Reason, unreachable) {
		t.Errorf("the reason does not name the tree it could not bind: %q",
			got[0].Reason)
	}
}

// TestNoTargetKeepsMeasuringTheSharedCheckout is the containment half, and it
// is what keeps mode 1's fix from being a regression.
//
// A step with NOTHING under review — a first implement step whose pre-gate asks
// "is the tree clean before we start" — is doing exactly its job when it
// measures the shared checkout. Skipping those would break every pre-gate that
// runs before any work exists, which is most of them.
func TestNoTargetKeepsMeasuringTheSharedCheckout(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "clean-check-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)

	got, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}}, "", "", nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)

	if !sentinelExists(t, sentinel) {
		t.Error("a pre-gate with no target under review did not run; nothing " +
			"is being reviewed yet, so the shared checkout IS the subject")
	}
	if len(got) != 1 || got[0].Verdict != VerdictPass {
		t.Errorf("results = %+v, want one pass", got)
	}
}

// TestSweptWorktreeIsReconstructedFromTheObjectDatabase is mode 2.
//
// A verify step's pre-gate resolves the IMPLEMENT wave's worktree, which
// integration sweeps before verify runs in a later wave — deterministic 2/2
// across RUN-22 STEP-380 and RUN-27 STEP-467, plus RUN-29 STEP-746.
//
// Parking every one of those is honest but useless: the commit is STILL IN THE
// OBJECT DATABASE. Sweeping a checkout does not delete the object, so the tree
// can be rebuilt exactly, measured, and thrown away. The gate then measures the
// real subject rather than a substitute or nothing at all.
func TestSweptWorktreeIsReconstructedFromTheObjectDatabase(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	// A real repository with a real commit, and a worktree that is then swept
	// exactly as integration sweeps one.
	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")
	swept := filepath.Join(t.TempDir(), "gone")

	// The gate's witness proves WHICH tree it ran in: it copies the reviewed
	// file out, so a run against the shared checkout (which does not have it)
	// would produce different bytes than a run against the reconstruction.
	witness := filepath.Join(repoRoot, "witness-out")
	argv := []string{"/bin/cp", "measured.txt", witness}

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)
	setRunExecRoot(t, conn, step.RunID, repoRoot)

	got, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}}, sha, swept, nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Verdict != VerdictPass {
		t.Fatalf("verdict = %q, want %q — the commit is in the object database, "+
			"so the swept tree is reconstructible and the gate has a real "+
			"subject to measure. Reason: %s",
			got[0].Verdict, VerdictPass, got[0].Reason)
	}
	// The gate ran IN the reconstruction: it found `measured.txt`, which only
	// exists at that commit.
	body, readErr := os.ReadFile(witness)
	testsupport.Must(t, readErr, "the gate did not run in a tree holding the "+
		"reviewed file: %v", readErr)
	if strings.TrimSpace(string(body)) != "under review" {
		t.Errorf("the gate measured %q, want %q", body, "under review")
	}
	// The row says the tree was rebuilt. A verdict from a reconstruction is as
	// good as one from the original, but a reader should not have to infer it —
	// and it explains why the tree the row names is not on disk.
	if !strings.Contains(got[0].Reason, "reconstruction") {
		t.Errorf("the row does not disclose that it measured a reconstruction: %q",
			got[0].Reason)
	}
}

// TestReconstructionIsReleased: the scratch tree does not outlive the phase.
//
// A worktree left behind is not merely litter — it stays in the parent's
// .git/worktrees and `git worktree list` reports it forever, so a run that
// reconstructed once per wave would accumulate them for the life of the
// checkout.
func TestReconstructionIsReleased(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	argv := []string{"/usr/bin/true"}
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)
	setRunExecRoot(t, conn, step.RunID, repoRoot)

	_, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}},
		sha, filepath.Join(t.TempDir(), "gone"), nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)

	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list").Output()
	testsupport.Must(t, err, "git worktree list: %v", err)
	if strings.Contains(string(out), "docket-pregate-") {
		t.Errorf("a reconstruction outlived the pre-gate phase:\n%s", out)
	}
}

// TestSkippedGateParksRatherThanRoutingOnFail is DKT-254's routing half.
//
// `skipped` counts as not-pass, and before this it was swallowed by the fail
// case and routed per `on_fail`. That is how RUN-22 STEP-380 and RUN-27
// STEP-467 sent a swept worktree into a 3-seat verify-tribunal: three seats
// deliberated over an infrastructure artifact instead of the change, because
// "we could not measure" arrived at the panel wearing the same word as "we
// measured and it failed".
//
// The two need OPPOSITE responses — reconstruct-and-remeasure versus judge the
// failure — so they must not share a destination.
func TestSkippedGateParksRatherThanRoutingOnFail(t *testing.T) {
	rows := []db.GateResultRow{
		{Gate: "ac-commands", Ordinal: 0, Verdict: db.GateVerdictSkipped},
		{Gate: "tests", Ordinal: 0, Verdict: db.GateVerdictPass},
	}
	verdict, unmeasured := verdictOverRows(rows)

	// The verdict is unchanged: "we couldn't check, so carry on" is what makes
	// a control decorative, and a skipped gate is still not a pass.
	if verdict != VerdictFail {
		t.Errorf("verdict = %q, want %q — a gate that measured nothing has not "+
			"passed", verdict, VerdictFail)
	}
	// What is new is that the CALLER can tell which kind of not-pass it was.
	if len(unmeasured) != 1 || unmeasured[0] != "ac-commands" {
		t.Errorf("unmeasured = %v, want [ac-commands] — the routing decision "+
			"needs to know a measurement was absent, not merely bad", unmeasured)
	}
}

// TestMeasuredFailureIsNotReportedAsUnmeasured is the other direction.
//
// If a plain failure were reported as unmeasured, every failing gate would park
// for an operator instead of entering its `on_fail` fix loop — which would
// break the ordinary path this change is supposed to leave alone.
func TestMeasuredFailureIsNotReportedAsUnmeasured(t *testing.T) {
	for _, verdict := range []string{
		db.GateVerdictFail, db.GateVerdictUnmatched, db.GateVerdictPass,
	} {
		t.Run(verdict, func(t *testing.T) {
			_, unmeasured := verdictOverRows([]db.GateResultRow{
				{Gate: "tests", Ordinal: 0, Verdict: verdict},
			})
			if len(unmeasured) != 0 {
				t.Errorf("a %q gate was reported unmeasured (%v); only `skipped` "+
					"means no evidence was collected", verdict, unmeasured)
			}
		})
	}
}

// TestFlakyReRunClearsAnEarlierSkip: the last attempt decides, here too.
//
// F4 makes the LAST attempt's verdict the one that routes. A gate that could
// not bind its tree on attempt 0 and measured it fine on attempt 1 has been
// measured, and parking it would strand a step whose evidence exists.
func TestFlakyReRunClearsAnEarlierSkip(t *testing.T) {
	verdict, unmeasured := verdictOverRows([]db.GateResultRow{
		{Gate: "ac-commands", Ordinal: 0, Verdict: db.GateVerdictSkipped},
		{Gate: "ac-commands", Ordinal: 1, Verdict: db.GateVerdictPass},
	})
	if verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q — F4 routes on the last attempt",
			verdict, VerdictPass)
	}
	if len(unmeasured) != 0 {
		t.Errorf("unmeasured = %v after a passing re-run; the earlier skip was "+
			"superseded by a real measurement", unmeasured)
	}
}
