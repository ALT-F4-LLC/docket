package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1899: every resolution event says under WHAT AUTHORITY it was made.
//
// Attribution (DKT-2450) answers who ruled and from where; it cannot answer
// whether they were entitled to. The conductor policy routes an operator's own
// decision, an applied standing authorization, and the conductor's own
// reproduction differently, and all three used to be tellable only by reading
// the note — so `run report` could not count them.

// grantUnder is a standing-grant authority, the one value that carries a
// reference. Distinct from testUnder so a test proves the fields came from ITS
// options rather than from the suite's default.
var grantUnder = Authority{Kind: AuthorityStandingGrant, Ref: "RUN NOTE 54"}

// runPayload decodes the newest run-scoped event of `kind`.
func runPayload(t *testing.T, conn *sql.DB, runID int, kind string) map[string]any {
	t.Helper()
	var raw string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, kind).Scan(&raw)
	testsupport.Must(t, err, "reading the %s event: %v", kind, err)
	data := map[string]any{}
	testsupport.Must(t, json.Unmarshal([]byte(raw), &data),
		"a %s event's data is not an object: %s", kind, raw)
	return data
}

// assertAuthority checks the pair on one event's payload. An empty `wantRef`
// asserts the key is ABSENT, because a reference means something only on a
// standing grant and an empty one would be a key a reader must interpret.
func assertAuthority(t *testing.T, data map[string]any, kind, wantKind, wantRef string) {
	t.Helper()
	if data["authority"] != wantKind {
		t.Errorf("%s event authority = %#v, want %q — the event must say under "+
			"what authority the resolution was made, not only who made it",
			kind, data["authority"], wantKind)
	}
	if wantRef == "" {
		if _, present := data["authority_ref"]; present {
			t.Errorf("%s event carries authority_ref = %#v under %q, which names "+
				"nothing there", kind, data["authority_ref"], wantKind)
		}
		return
	}
	if data["authority_ref"] != wantRef {
		t.Errorf("%s event authority_ref = %#v, want %q — a standing grant must "+
			"name the authorization it applies", kind, data["authority_ref"], wantRef)
	}
}

// TestResolutionAuthorityIsRecorded is AC2: each of the five kinds carries
// `authority`, and `authority_ref` on a standing grant.
func TestResolutionAuthorityIsRecorded(t *testing.T) {
	t.Run("step-approved", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()
		gateID := readyHumanGate(t, conn, e)

		err := e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
			Approve: true, Note: "looks right", By: auditBy, Under: grantUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "approve: %v", err)

		assertAuthority(t, rulingPayload(t, conn, gateID, EventStepApproved),
			EventStepApproved, AuthorityStandingGrant, grantUnder.Ref)
	})

	t.Run("step-rejected", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()
		gateID := readyHumanGate(t, conn, e)

		err := e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
			Approve: false, Note: "not yet", By: auditBy,
			Under: Authority{Kind: AuthorityConductor}, NowMS: nowMS,
		})
		testsupport.Must(t, err, "reject: %v", err)

		// `conductor` carries no reference, and the key must be absent rather
		// than empty.
		assertAuthority(t, rulingPayload(t, conn, gateID, EventStepRejected),
			EventStepRejected, AuthorityConductor, "")
	})

	t.Run("step-resolved", func(t *testing.T) {
		conn := mustDB(t)
		e := testEngine()
		id := parkedExecutor(t, conn, e)

		_, err := e.ResolveStepWith(conn, id, ResolveOptions{Token: testConductorToken,
			As: ResolveSkip, Note: "not needed", By: auditBy, Under: grantUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "resolve: %v", err)

		assertAuthority(t, rulingPayload(t, conn, id, EventStepResolved),
			EventStepResolved, AuthorityStandingGrant, grantUnder.Ref)
	})

	t.Run("step-approved on a held cluster", func(t *testing.T) {
		// The held-cluster path writes its own payload and must carry the
		// authority too — a materialized hold is decided by the same verbs, so
		// a resolution made there is as countable as one made anywhere else.
		conn := mustDB(t)
		e := testEngine()
		driveMirrorReconcile(t, conn, e)
		held := heldStep(t, conn, "reconcile-held@0#0")

		err := e.DecideStepWith(conn, held.ID, DecideOptions{Token: testConductorToken,
			Approve: true, Note: "call it high", Value: "high", By: auditBy,
			Under: grantUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "approve --value: %v", err)

		assertAuthority(t, rulingPayload(t, conn, held.ID, EventStepApproved),
			EventStepApproved, AuthorityStandingGrant, grantUnder.Ref)
	})

	t.Run("run-paused", func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)
		testsupport.Must(t, seatTestConductor(conn, run.ID), "seating: %v", nil)

		_, _, err := MoveRunWith(conn, MoveRunOptions{
			RunID: run.ID, Verb: "pause", To: model.RunWaitingHuman,
			From: []model.RunStatus{model.RunActive}, Reason: "operator review",
			Under: grantUnder, Token: testConductorToken, NowMS: nowMS,
		})
		testsupport.Must(t, err, "pause: %v", err)

		assertAuthority(t, runPayload(t, conn, run.ID, EventRunPaused),
			EventRunPaused, AuthorityStandingGrant, grantUnder.Ref)
	})

	t.Run("run-abandoned", func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)
		testsupport.Must(t, seatTestConductor(conn, run.ID), "seating: %v", nil)

		_, _, err := MoveRunWith(conn, MoveRunOptions{
			RunID: run.ID, Verb: "abandon", To: model.RunAbandoned,
			From:  []model.RunStatus{model.RunActive},
			Under: Authority{Kind: AuthorityOperator}, Reason: "not worth finishing",
			Token: testConductorToken, NowMS: nowMS,
		})
		testsupport.Must(t, err, "abandon: %v", err)

		assertAuthority(t, runPayload(t, conn, run.ID, EventRunAbandoned),
			EventRunAbandoned, AuthorityOperator, "")
	})

	// A RESUME is exempt: it returns a run to work rather than disposing of
	// anything, so it asserts no authority and records no key.
	t.Run("run-resumed carries none", func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)
		testsupport.Must(t, seatTestConductor(conn, run.ID), "seating: %v", nil)

		_, _, err := MoveRunWith(conn, MoveRunOptions{
			RunID: run.ID, Verb: "pause", To: model.RunWaitingHuman,
			From: []model.RunStatus{model.RunActive}, Under: grantUnder,
			Token: testConductorToken, NowMS: nowMS,
		})
		testsupport.Must(t, err, "pause: %v", err)

		_, _, err = MoveRunWith(conn, MoveRunOptions{
			RunID: run.ID, Verb: "resume", To: model.RunActive,
			From:  []model.RunStatus{model.RunWaitingHuman},
			Token: testConductorToken, NowMS: nowMS,
		})
		testsupport.Must(t, err, "resume without an authority: %v", err)

		data := runPayload(t, conn, run.ID, EventRunResumed)
		if _, present := data["authority"]; present {
			t.Errorf("run-resumed carries authority = %#v; a resume disposes of "+
				"nothing and asserts no authority", data["authority"])
		}
	})
}

// TestResolutionUnderNoAuthorityIsRefused: the seam refuses an unanswered
// authority before anything is written, so the rule binds every writer rather
// than only the CLI that happens to parse flags.
func TestResolutionUnderNoAuthorityIsRefused(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	id := parkedExecutor(t, conn, e)

	_, err := e.ResolveStepWith(conn, id, ResolveOptions{Token: testConductorToken,
		As: ResolveSkip, By: auditBy, NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("a resolution under no stated authority was accepted")
	}
	if hasEventKind(t, conn, id, EventStepResolved) {
		t.Error("the refused resolution left an event behind")
	}
}
