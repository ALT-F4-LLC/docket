package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-2465: the seven operator verbs bind to the run's conductor capability.
//
// The claims-leases §4.0 rows, proven at the engine seam so they hold for
// every writer: R9 (bound run, no token) is VALIDATION_ERROR, R10 (bound run,
// wrong token) is AUTH_ERROR, R11 (unbound run) is allowed exactly as before,
// R12 (`run conduct` on a terminal run) is CONFLICT — and every refusal writes
// nothing: no event, no row_version bump.

// boundRun activates the fixture and returns the capability the activation
// minted, which is the whole of what a conductor session holds.
func boundRun(t *testing.T, conn *sql.DB) (*model.Run, string) {
	t.Helper()
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)
	if len(result.ConductorToken) != 2*model.TokenBytes {
		t.Fatalf("activation returned a %d-char conductor token, want %d hex chars",
			len(result.ConductorToken), 2*model.TokenBytes)
	}
	hash, err := db.RunConductorHash(conn, run.ID)
	testsupport.Must(t, err, "RunConductorHash: %v", err)
	if !model.TokenMatches(result.ConductorToken, hash) {
		t.Fatal("the stored hash is not the returned token's")
	}
	return run, result.ConductorToken
}

// unbind makes a run read as activated before v29: no capability minted.
func unbind(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	execSQL(t, conn, `UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, runID)
}

// snapshot is the "nothing was written" probe: the event count and the run's
// and step's CAS versions.
type snapshot struct {
	events, runVersion, stepVersion int
}

func snap(t *testing.T, conn *sql.DB, runID, stepID int) snapshot {
	t.Helper()
	s := snapshot{events: eventCount(t, conn)}
	err := conn.QueryRow(`SELECT row_version FROM runs WHERE id = ?`, runID).Scan(&s.runVersion)
	testsupport.Must(t, err, "run row_version: %v", err)
	if stepID != 0 {
		err = conn.QueryRow(`SELECT row_version FROM steps WHERE id = ?`, stepID).Scan(&s.stepVersion)
		testsupport.Must(t, err, "step row_version: %v", err)
	}
	return s
}

func assertConductorCode(t *testing.T, err error, want ErrorCode, verb string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want %s", verb, want)
	}
	code, ok := CodeOf(err)
	if !ok || code != want {
		t.Fatalf("%s: code = %q (%v), want %s", verb, code, err, want)
	}
	if strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("%s: the refusal echoes the presented token: %v", verb, err)
	}
	if !strings.Contains(err.Error(), "run conduct") {
		t.Errorf("%s: the refusal does not name the recovery verb: %v", verb, err)
	}
}

// parkedImplement claims implement@0 and parks it, the way a fix that gave up
// would leave it.
func parkedImplement(t *testing.T, conn *sql.DB) int {
	t.Helper()
	id := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	execSQL(t, conn, `UPDATE steps SET status = ?, saga_stage = ? WHERE id = ?`,
		db.StepWaitingHuman, db.SagaRecorded, id)
	return id
}

// conductorVerb is one of the seven, as a call taking the token to present.
type conductorVerb struct {
	name string
	// setup readies the fixture and returns the step the verb addresses (0
	// for a run verb).
	setup func(t *testing.T, conn *sql.DB, e *Engine, run *model.Run) int
	call  func(conn *sql.DB, e *Engine, run *model.Run, stepID int, token string) error
}

var conductorVerbs = []conductorVerb{
	{
		name:  "step approve",
		setup: func(t *testing.T, conn *sql.DB, e *Engine, _ *model.Run) int { return readyHumanGate(t, conn, e) },
		call: func(conn *sql.DB, e *Engine, _ *model.Run, stepID int, token string) error {
			return e.DecideStepWith(conn, stepID, DecideOptions{
				Approve: true, By: auditBy, Token: token, NowMS: nowMS})
		},
	},
	{
		name:  "step reject",
		setup: func(t *testing.T, conn *sql.DB, e *Engine, _ *model.Run) int { return readyHumanGate(t, conn, e) },
		call: func(conn *sql.DB, e *Engine, _ *model.Run, stepID int, token string) error {
			return e.DecideStepWith(conn, stepID, DecideOptions{
				Approve: false, By: auditBy, Token: token, NowMS: nowMS})
		},
	},
	{
		name:  "step resolve",
		setup: func(t *testing.T, conn *sql.DB, _ *Engine, _ *model.Run) int { return parkedImplement(t, conn) },
		call: func(conn *sql.DB, e *Engine, _ *model.Run, stepID int, token string) error {
			_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
				As: ResolveSkip, By: auditBy, Token: token, NowMS: nowMS})
			return err
		},
	},
	{
		name: "step reap",
		setup: func(t *testing.T, conn *sql.DB, _ *Engine, _ *model.Run) int {
			id := stepIDByInstance(t, conn, "implement@0")
			_, err := ClaimStep(conn, id, ClaimOptions{Owner: "doomed", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)
			return id
		},
		call: func(conn *sql.DB, _ *Engine, _ *model.Run, stepID int, token string) error {
			return ForceReapStepWith(conn, stepID, ForceReapOptions{
				Reason: "spawn died", By: auditBy, Token: token, NowMS: nowMS})
		},
	},
	{
		name:  "run pause",
		setup: func(*testing.T, *sql.DB, *Engine, *model.Run) int { return 0 },
		call: func(conn *sql.DB, _ *Engine, run *model.Run, _ int, token string) error {
			_, _, err := MoveRunWith(conn, MoveRunOptions{
				RunID: run.ID, Verb: "pause", To: model.RunWaitingHuman,
				From: []model.RunStatus{model.RunActive}, Token: token, NowMS: nowMS})
			return err
		},
	},
	{
		name: "run resume",
		setup: func(t *testing.T, conn *sql.DB, _ *Engine, run *model.Run) int {
			execSQL(t, conn, `UPDATE runs SET status = ?, pause_origin = ? WHERE id = ?`,
				model.RunWaitingHuman, model.RunPauseOriginOperator, run.ID)
			return 0
		},
		call: func(conn *sql.DB, _ *Engine, run *model.Run, _ int, token string) error {
			_, _, err := MoveRunWith(conn, MoveRunOptions{
				RunID: run.ID, Verb: "resume", To: model.RunActive,
				From: []model.RunStatus{model.RunWaitingHuman}, Token: token, NowMS: nowMS})
			return err
		},
	},
	{
		name:  "run abandon",
		setup: func(*testing.T, *sql.DB, *Engine, *model.Run) int { return 0 },
		call: func(conn *sql.DB, _ *Engine, run *model.Run, _ int, token string) error {
			_, _, err := MoveRunWith(conn, MoveRunOptions{
				RunID: run.ID, Verb: "abandon", To: model.RunAbandoned, From: abandonFrom,
				Reason: "scope changed", Token: token, NowMS: nowMS})
			return err
		},
	},
	{
		name:  "run abandon --issue",
		setup: func(*testing.T, *sql.DB, *Engine, *model.Run) int { return 0 },
		call: func(conn *sql.DB, _ *Engine, run *model.Run, _ int, token string) error {
			var issueID int
			if err := conn.QueryRow(
				`SELECT issue_id FROM run_issues WHERE run_id = ?`, run.ID).Scan(&issueID); err != nil {
				return err
			}
			_, err := AbandonIssueInRunWith(conn, AbandonIssueOptions{
				RunID: run.ID, IssueID: issueID, Reason: "mis-routed", Token: token, NowMS: nowMS})
			return err
		},
	},
}

// TestOperatorVerbsRequireTheConductorCapability: on a bound run each verb
// refuses a missing token (R9) and a wrong one (R10) without writing, then
// succeeds with the token activation returned.
func TestOperatorVerbsRequireTheConductorCapability(t *testing.T) {
	for _, verb := range conductorVerbs {
		t.Run(verb.name, func(t *testing.T) {
			conn := mustDB(t)
			run, token := boundRun(t, conn)
			e := testEngine()
			stepID := verb.setup(t, conn, e, run)

			before := snap(t, conn, run.ID, stepID)
			assertConductorCode(t, verb.call(conn, e, run, stepID, ""), CodeValidation, verb.name+" without a token")
			err := verb.call(conn, e, run, stepID, "deadbeef")
			assertConductorCode(t, err, CodeAuth, verb.name+" with a wrong token")
			if !errors.Is(err, ErrNotConductor) {
				t.Errorf("%s: a wrong token is not ErrNotConductor: %v", verb.name, err)
			}
			if after := snap(t, conn, run.ID, stepID); after != before {
				t.Errorf("%s: a refusal wrote something: before %+v, after %+v",
					verb.name, before, after)
			}

			testsupport.Must(t, verb.call(conn, e, run, stepID, token),
				"%s with the run's capability: %v", verb.name, nil)
		})
	}
}

// TestUnboundRunStaysOpenToTheOperatorVerbs is R11: a run activated before
// the capability existed asks for nothing, so a migrated store's runs in
// flight keep working exactly as they did.
func TestUnboundRunStaysOpenToTheOperatorVerbs(t *testing.T) {
	for _, verb := range conductorVerbs {
		t.Run(verb.name, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := boundRun(t, conn)
			unbind(t, conn, run.ID)
			e := testEngine()
			stepID := verb.setup(t, conn, e, run)

			testsupport.Must(t, verb.call(conn, e, run, stepID, ""),
				"%s on an unbound run without a token: %v", verb.name, nil)
		})
	}
}

// TestConductRunRotatesTheCapability: `run conduct` mints a fresh token,
// retires the standing one, and records who took the seat.
func TestConductRunRotatesTheCapability(t *testing.T) {
	conn := mustDB(t)
	run, first := boundRun(t, conn)

	result, err := ConductRun(conn, run.ID, ConductOptions{By: auditBy, NowMS: nowMS})
	testsupport.Must(t, err, "ConductRun: %v", err)
	if !result.Rotated {
		t.Error("rotated = false on a run that already held a capability")
	}
	if result.Token == first || len(result.Token) != 2*model.TokenBytes {
		t.Errorf("token = %q, want a fresh %d-char token", result.Token, 2*model.TokenBytes)
	}

	pause := func(token string) error {
		_, _, err := MoveRunWith(conn, MoveRunOptions{
			RunID: run.ID, Verb: "pause", To: model.RunWaitingHuman,
			From: []model.RunStatus{model.RunActive}, Token: token, NowMS: nowMS})
		return err
	}
	assertConductorCode(t, pause(first), CodeAuth, "run pause with the retired token")
	testsupport.Must(t, pause(result.Token), "run pause with the new token: %v", nil)

	// The seat's event: attributed, and stating that it displaced someone.
	var raw string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		run.ID, EventConductorSeated).Scan(&raw)
	testsupport.Must(t, err, "reading the conductor-seated event: %v", err)
	for _, want := range []string{`"actor":"Ada Relay"`, `"cwd":"/work/session-b"`, `"rotated":true`} {
		if !strings.Contains(raw, want) {
			t.Errorf("conductor-seated data %s lacks %s", raw, want)
		}
	}
	// And the token itself is in no event and no row: hash-only storage.
	var leaked int
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE data LIKE ?`, "%"+result.Token+"%").Scan(&leaked)
	testsupport.Must(t, err, "scanning events: %v", err)
	if leaked != 0 {
		t.Error("the minted token appears in an event payload")
	}
}

// TestConductRunBindsAnUnboundRunAndRefusesATerminalOne covers the two ends:
// a legacy run is bound by its first conduct (rotated = false), and a run
// that has ended has nothing left to conduct (R12).
func TestConductRunBindsAnUnboundRunAndRefusesATerminalOne(t *testing.T) {
	conn := mustDB(t)
	run, _ := boundRun(t, conn)
	unbind(t, conn, run.ID)

	result, err := ConductRun(conn, run.ID, ConductOptions{By: auditBy, NowMS: nowMS})
	testsupport.Must(t, err, "ConductRun on an unbound run: %v", err)
	if result.Rotated {
		t.Error("rotated = true on a run that held no capability")
	}
	hash, err := db.RunConductorHash(conn, run.ID)
	testsupport.Must(t, err, "RunConductorHash: %v", err)
	if !model.TokenMatches(result.Token, hash) {
		t.Error("the conduct did not bind the run to the token it returned")
	}

	_, _, err = MoveRunWith(conn, MoveRunOptions{
		RunID: run.ID, Verb: "abandon", To: model.RunAbandoned, From: abandonFrom,
		Reason: "done with it", Token: result.Token, NowMS: nowMS})
	testsupport.Must(t, err, "abandon: %v", err)

	_, err = ConductRun(conn, run.ID, ConductOptions{By: auditBy, NowMS: nowMS})
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("conduct on an abandoned run: code = %q (%v), want CONFLICT", code, err)
	}

	// An unattributed seat is refused before anything is written, as a
	// ruling is.
	if _, err := ConductRun(conn, run.ID, ConductOptions{NowMS: nowMS}); err == nil ||
		!strings.Contains(err.Error(), "unattributed") {
		t.Errorf("an unattributed conduct was not refused: %v", err)
	}
}

// TestActivationMintsOnceAndReactivationRotatesNothing: the first activation
// binds the run; a dry run mints nothing; a re-activation leaves the standing
// capability alone; and a run conducted while planning keeps its token.
func TestActivationMintsOnceAndReactivationRotatesNothing(t *testing.T) {
	t.Run("dry run mints nothing", func(t *testing.T) {
		conn := mustDB(t)
		registerFixture(t, conn)
		issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
		run := startRun(t, conn, issue)
		result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
		testsupport.Must(t, err, "dry run: %v", err)
		if result.ConductorToken != "" {
			t.Error("a dry run returned a conductor token for a run it did not activate")
		}
		if hash, _ := db.RunConductorHash(conn, run.ID); hash != "" {
			t.Error("a dry run bound the run")
		}
	})

	t.Run("re-activation keeps the standing capability", func(t *testing.T) {
		conn := mustDB(t)
		run, token := boundRun(t, conn)
		result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
		testsupport.Must(t, err, "re-activate: %v", err)
		if !result.Reactivation {
			t.Fatal("premise: the second activation was not a re-activation")
		}
		if result.ConductorToken != "" {
			t.Error("a re-activation returned a conductor token")
		}
		hash, _ := db.RunConductorHash(conn, run.ID)
		if !model.TokenMatches(token, hash) {
			t.Error("a re-activation rotated the capability the conductor holds")
		}
	})

	t.Run("a run conducted while planning keeps its token", func(t *testing.T) {
		conn := mustDB(t)
		registerFixture(t, conn)
		issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
		run := startRun(t, conn, issue)
		seat, err := ConductRun(conn, run.ID, ConductOptions{By: auditBy, NowMS: nowMS})
		testsupport.Must(t, err, "conduct while planning: %v", err)

		result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
		testsupport.Must(t, err, "activate: %v", err)
		if result.ConductorToken != "" {
			t.Error("activation minted over a capability the conductor already holds")
		}
		hash, _ := db.RunConductorHash(conn, run.ID)
		if !model.TokenMatches(seat.Token, hash) {
			t.Error("activation rotated the capability minted while planning")
		}
	})
}
