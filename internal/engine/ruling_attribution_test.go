package engine

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-2450: a step ruling names who made it and from where.
//
// `step approve`, `step reject`, `step resolve` and `step reap` are the four
// `human` kinds an auditor most needs a name on, and each recorded only a
// note — a ruling a harness relayed was indistinguishable in the feed from one
// a person typed. Trust events had already closed the same gap for grants
// (DKT-263/DKT-595); this pins the same two claim-level fields, the same
// unconditional keys, and the same refusal of an unattributed write, on the
// rulings — and the run report carrying them beside the routing.

// auditBy is a ruling identity distinct from the suite's testBy, so a test
// proves the fields came from ITS options rather than from any default.
var auditBy = Attribution{Actor: "Ada Relay", Cwd: "/work/session-b"}

// rulingPayload decodes the newest event of `kind` recorded on the step.
func rulingPayload(t *testing.T, conn *sql.DB, stepID int, kind string) map[string]any {
	t.Helper()
	var raw string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		stepID, kind).Scan(&raw)
	testsupport.Must(t, err, "reading the %s event: %v", kind, err)
	data := map[string]any{}
	testsupport.Must(t, json.Unmarshal([]byte(raw), &data), "a %s event's data is not an object: %s", kind, raw)
	return data
}

// assertAttributed checks the two fields and that the payload's other keys
// are exactly the ones the kind always carried.
func assertAttributed(t *testing.T, data map[string]any, kind string, want map[string]any) {
	t.Helper()
	if data["actor"] != auditBy.Actor || data["cwd"] != auditBy.Cwd {
		t.Errorf("%s event actor=%#v cwd=%#v, want %q from %q — the ruling must "+
			"say who made it and from where", kind, data["actor"], data["cwd"],
			auditBy.Actor, auditBy.Cwd)
	}
	for key, value := range want {
		if data[key] != value {
			t.Errorf("%s event %s = %#v, want %#v — the attribution rides beside "+
				"the payload the kind always had, replacing nothing", kind, key, data[key], value)
		}
	}
	// The instance key is eventData's own; every other key must be accounted
	// for, or a reader diffing shapes sees a key nothing documents.
	for key := range data {
		// `instance` is eventData's own; `authority` rides on every ruling
		// under DKT-1899 and is asserted by that criterion's own test.
		if key == "instance" || key == "actor" || key == "cwd" || key == "authority" {
			continue
		}
		if _, expected := want[key]; !expected {
			t.Errorf("%s event carries an unexpected key %q = %#v", kind, key, data[key])
		}
	}
}

// readyHumanGate drives the fixture to the point where commit-gate@0 awaits an
// operator, and returns its id.
func readyHumanGate(t *testing.T, conn *sql.DB, e *Engine) int {
	t.Helper()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")
	claimAndComplete(t, conn, e, "verify@0", "report", `[{"status":"met"}]`)
	return stepIDByInstance(t, conn, "commit-gate@0")
}

// parkedExecutor registers a one-step workflow whose executor parks on its
// first failure, drives it there, and returns the parked step's id.
func parkedExecutor(t *testing.T, conn *sql.DB, e *Engine) int {
	t.Helper()
	const src = `
[pipeline]
name = "parks"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"
`
	registerSource(t, conn, []byte(src), "parks.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	id := stepIDByInstance(t, conn, "flaky@0")

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, id, claim.Token, "gave up", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)
	if got := stepStatus(t, conn, "flaky@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: flaky@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	return id
}

// TestRulingEventsCarryActorAndCwd is the acceptance criterion: each of the
// four rulings emits an event carrying `actor` and `cwd`, beside exactly the
// payload it always carried.
func TestRulingEventsCarryActorAndCwd(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()
		gateID := readyHumanGate(t, conn, e)

		err := e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
			Approve: true, Note: "looks right", By: auditBy, Under: testUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "approve: %v", err)

		assertAttributed(t, rulingPayload(t, conn, gateID, EventStepApproved),
			EventStepApproved, map[string]any{"detail": "looks right"})
	})

	t.Run("reject", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()
		gateID := readyHumanGate(t, conn, e)

		err := e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
			Approve: false, Note: "not yet", By: auditBy, Under: testUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "reject: %v", err)

		// The fixture's commit-gate rejects into a fix loop, which appends its
		// own reason to the note (DKT-168); the detail is the recorded note,
		// whatever the loop added.
		data := rulingPayload(t, conn, gateID, EventStepRejected)
		detail, _ := data["detail"].(string)
		if !strings.HasPrefix(detail, "not yet") {
			t.Errorf("step-rejected detail = %q, want the operator's note first", detail)
		}
		assertAttributed(t, data, EventStepRejected, map[string]any{"detail": detail})
	})

	t.Run("resolve", func(t *testing.T) {
		conn := mustDB(t)
		e := testEngine()
		id := parkedExecutor(t, conn, e)

		_, err := e.ResolveStepWith(conn, id, ResolveOptions{Token: testConductorToken,
			As: ResolveSkip, Note: "not needed", By: auditBy, Under: testUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "resolve: %v", err)

		assertAttributed(t, rulingPayload(t, conn, id, EventStepResolved),
			EventStepResolved, map[string]any{"detail": ResolveSkip})
	})

	t.Run("force reap", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		id := stepIDByInstance(t, conn, "implement@0")
		_, err := ClaimStep(conn, id, ClaimOptions{Owner: "doomed", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)

		err = ForceReapStepWith(conn, id, ForceReapOptions{Token: testConductorToken,
			Reason: "spawn died at startup", By: auditBy, NowMS: nowMS,
		})
		testsupport.Must(t, err, "reap: %v", err)

		assertAttributed(t, rulingPayload(t, conn, id, EventLeaseReaped),
			EventLeaseReaped, map[string]any{"forced": true, "reason": "spawn died at startup"})
	})

	t.Run("approve of a held cluster with a value", func(t *testing.T) {
		// The held-cluster path writes its own payload (`note` + `value`),
		// and it must carry the attribution too — a materialized hold is
		// decided by the same verbs.
		conn := mustDB(t)
		e := testEngine()
		driveMirrorReconcile(t, conn, e)
		held := heldStep(t, conn, "reconcile-held@0#0")

		err := e.DecideStepWith(conn, held.ID, DecideOptions{Token: testConductorToken,
			Approve: true, Note: "call it high", Value: "high", By: auditBy,
			Under: testUnder, NowMS: nowMS,
		})
		testsupport.Must(t, err, "approve --value: %v", err)

		assertAttributed(t, rulingPayload(t, conn, held.ID, EventStepApproved),
			EventStepApproved, map[string]any{"note": "call it high", "value": "high"})
	})
}

// TestUnattributedRulingIsRefused is DKT-595's rule on the rulings: a writer
// that cannot say who and from where must not write one, and the refusal is
// at the engine seam so it holds for every writer, not only the CLI.
func TestUnattributedRulingIsRefused(t *testing.T) {
	partial := []Attribution{{}, {Actor: "someone"}, {Cwd: "/somewhere"}}

	t.Run("approve and reject", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		e := testEngine()
		gateID := readyHumanGate(t, conn, e)
		for _, by := range partial {
			for _, approve := range []bool{true, false} {
				err := e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
					Approve: approve, By: by, NowMS: nowMS,
				})
				if err == nil || !strings.Contains(err.Error(), "unattributed") {
					t.Fatalf("approve=%v by %+v: err = %v, want the unattributed refusal", approve, by, err)
				}
			}
		}
		if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepPending {
			t.Errorf("commit-gate@0 = %q after refused decisions; nothing may have been written", got)
		}
		if hasEventKind(t, conn, gateID, EventStepApproved) || hasEventKind(t, conn, gateID, EventStepRejected) {
			t.Error("a refused decision left an event behind")
		}
	})

	t.Run("resolve", func(t *testing.T) {
		conn := mustDB(t)
		e := testEngine()
		id := parkedExecutor(t, conn, e)
		for _, by := range partial {
			_, err := e.ResolveStepWith(conn, id, ResolveOptions{Token: testConductorToken, As: ResolveSkip, By: by, NowMS: nowMS})
			if err == nil || !strings.Contains(err.Error(), "unattributed") {
				t.Fatalf("resolve by %+v: err = %v, want the unattributed refusal", by, err)
			}
		}
		if got := stepStatus(t, conn, "flaky@0"); got != db.StepWaitingHuman {
			t.Errorf("flaky@0 = %q after refused resolutions, want still parked", got)
		}
	})

	t.Run("force reap", func(t *testing.T) {
		conn := mustDB(t)
		activatedRun(t, conn)
		id := stepIDByInstance(t, conn, "implement@0")
		_, err := ClaimStep(conn, id, ClaimOptions{Owner: "doomed", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		for _, by := range partial {
			err := ForceReapStepWith(conn, id, ForceReapOptions{Token: testConductorToken, Reason: "dead", By: by, NowMS: nowMS})
			if err == nil || !strings.Contains(err.Error(), "unattributed") {
				t.Fatalf("reap by %+v: err = %v, want the unattributed refusal", by, err)
			}
		}
		if got := stepStatus(t, conn, "implement@0"); got != db.StepClaimed {
			t.Errorf("implement@0 = %q after refused reaps, want still claimed", got)
		}
	})
}

// TestRunReportAttributesRulings: the report's step rows carry the last
// ruling's attribution beside the routing, so a reader learns not only what
// was decided but by whom — and a forced reap by a relay is told apart from an
// operator's approval.
func TestRunReportAttributesRulings(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// A relay reaps a dead implement attempt from its own session...
	relay := Attribution{Actor: "wave-relay", Cwd: "/work/session-a"}
	implementID := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "doomed", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = ForceReapStepWith(conn, implementID, ForceReapOptions{Token: testConductorToken,
		Reason: "spawn died", By: relay, NowMS: nowMS,
	})
	testsupport.Must(t, err, "reap: %v", err)

	// ...the work completes on a second attempt, and an operator approves the
	// gate from another.
	gateID := readyHumanGate(t, conn, e)
	err = e.DecideStepWith(conn, gateID, DecideOptions{Token: testConductorToken,
		Approve: true, Note: "looks right", By: auditBy, Under: testUnder, NowMS: nowMS,
	})
	testsupport.Must(t, err, "approve: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	rulings := make(map[string]*StepRuling)
	for _, a := range report.Attempts {
		rulings[a.Instance] = a.Ruling
	}

	want := map[string]StepRuling{
		"implement@0":   {Event: EventLeaseReaped, Actor: relay.Actor, Cwd: relay.Cwd},
		"commit-gate@0": {Event: EventStepApproved, Actor: auditBy.Actor, Cwd: auditBy.Cwd},
	}
	for instance, expected := range want {
		got := rulings[instance]
		if got == nil || *got != expected {
			t.Errorf("%s ruling = %+v, want %+v", instance, got, expected)
		}
	}
	// A step nobody ruled on carries nothing: a completed executor step's
	// record is its own, and annotating it would invent a ruling.
	if got := rulings["verify@0"]; got != nil {
		t.Errorf("verify@0 ruling = %+v, want none — nobody ruled on it", got)
	}
	if _, ok := rulings[model.FormatStepID(0)]; ok {
		t.Error("the report carries a row for no step")
	}
}
