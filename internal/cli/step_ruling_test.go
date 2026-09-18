package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-2450 at the CLI boundary: the four ruling verbs WIRE the attribution.
//
// The engine records whatever it is handed and refuses nothing but emptiness,
// so an engine test passing the fields itself proves nothing about `docket
// step approve`. Modelled on TestTrustEventNamesWhoGrantedIt: the actor is the
// identity resolver's answer and the cwd is the process's real working
// directory, which is what proves both are read from the invocation rather
// than defaulted — and, as there, an unreadable cwd refuses the ruling rather
// than recording it unattributed.

// stepIDNamed reads a step's id by its rendered instance.
func stepIDNamed(t *testing.T, conn *sql.DB, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(`SELECT id FROM steps WHERE instance = ?`, instance).Scan(&id)
	testsupport.Must(t, err, "finding %s: %v", instance, err)
	return id
}

// rulingEvent decodes the newest event of `kind` on the step, or nil when
// there is none.
func rulingEvent(t *testing.T, conn *sql.DB, stepID int, kind string) map[string]any {
	t.Helper()
	var raw string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		stepID, kind).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	testsupport.Must(t, err, "reading the %s event: %v", kind, err)
	data := map[string]any{}
	testsupport.Must(t, json.Unmarshal([]byte(raw), &data), "a %s event's data is not an object: %s", kind, raw)
	return data
}

// assertRuledByThisInvocation is the wiring check: the fields name THIS
// process's identity and working directory.
func assertRuledByThisInvocation(t *testing.T, data map[string]any, kind string) {
	t.Helper()
	if data == nil {
		t.Fatalf("no %s event was recorded", kind)
	}
	actor, _ := data["actor"].(string)
	if actor == "" || actor != config.DefaultAuthor() {
		t.Errorf("%s event actor = %#v, want the identity resolver's %q — a ruling "+
			"is attributed the way every other authored row is", kind, data["actor"], config.DefaultAuthor())
	}
	wd, err := os.Getwd()
	testsupport.Must(t, err, "Getwd: %v", err)
	if data["cwd"] != wd {
		t.Errorf("%s event cwd = %#v, want %q — the field must name where the verb "+
			"actually ran, since that is what tells two concurrent sessions apart",
			kind, data["cwd"], wd)
	}
}

// readyGate drives the unit-run fixture's executor to done, so its human gate
// `second@0` is ready, and returns the gate's id.
func readyGate(t *testing.T, conn *sql.DB) int {
	t.Helper()
	activatedRunForNext(t, conn)
	first := stepIDNamed(t, conn, "first@0")
	claim, err := engine.ClaimStep(conn, first, engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
	testsupport.Must(t, err, "claim: %v", err)
	err = engine.NewEngine().CompleteStep(conn, first, engine.CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "complete: %v", err)
	return stepIDNamed(t, conn, "second@0")
}

// parkedStep registers a one-step workflow whose executor parks on its first
// failure, drives it there, and returns the parked step's id.
func parkedStep(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerForRun(t, conn, `
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
`)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "park me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue: %v", nil)
	activateHolding(t, conn, run.ID, model.NowMS())

	id := stepIDNamed(t, conn, "flaky@0")
	claim, err := engine.ClaimStep(conn, id, engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
	testsupport.Must(t, err, "claim: %v", err)
	err = engine.NewEngine().FailStep(conn, id, claim.Token, "gave up", "", model.NowMS())
	testsupport.Must(t, err, "fail: %v", err)
	if got := stepStatusByInstance(t, conn, "flaky@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: flaky@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	return id
}

func decideCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("note", "", "")
	cmd.Flags().String("value", "", "")
	withOperatorAuthority(cmd)
	return cmd
}

// withOperatorAuthority registers the DKT-1899 flags and answers them
// `operator`, the authority a suite standing in for a person asserts. A test
// about the authority itself sets its own value, or registers the flags
// without one.
func withOperatorAuthority(cmd *cobra.Command) *cobra.Command {
	addAuthorityFlags(cmd)
	_ = cmd.Flags().Set("authority", engine.AuthorityOperator)
	return cmd
}

func reapCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("reason", "", "")
	return cmd
}

// TestStepRulingVerbsNameWhoRuled: approve, reject, resolve and reap each
// record this invocation's actor and cwd on the event they write.
func TestStepRulingVerbsNameWhoRuled(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		conn := newTestDB(t)
		gate := readyGate(t, conn)
		cmd := decideCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("note", "looks right"), "set --note: %v", nil)
		w, buf := bufWriter(true)
		err := runDecide(cmd, []string{model.FormatStepID(gate)}, true, w)
		testsupport.Must(t, err, "step approve: %v\n%s", err, buf.String())

		data := rulingEvent(t, conn, gate, engine.EventStepApproved)
		assertRuledByThisInvocation(t, data, engine.EventStepApproved)
		if data["detail"] != "looks right" {
			t.Errorf("detail = %#v, want the note the verb has always recorded", data["detail"])
		}
	})

	t.Run("reject", func(t *testing.T) {
		conn := newTestDB(t)
		gate := readyGate(t, conn)
		w, buf := bufWriter(true)
		err := runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, false, w)
		testsupport.Must(t, err, "step reject: %v\n%s", err, buf.String())

		assertRuledByThisInvocation(t, rulingEvent(t, conn, gate, engine.EventStepRejected),
			engine.EventStepRejected)
	})

	t.Run("resolve", func(t *testing.T) {
		conn := newTestDB(t)
		id := parkedStep(t, conn)
		cmd := resolveCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
		w, buf := bufWriter(true)
		err := runStepResolve(cmd, []string{model.FormatStepID(id)}, w)
		testsupport.Must(t, err, "step resolve: %v\n%s", err, buf.String())

		data := rulingEvent(t, conn, id, engine.EventStepResolved)
		assertRuledByThisInvocation(t, data, engine.EventStepResolved)
		if data["detail"] != engine.ResolveSkip {
			t.Errorf("detail = %#v, want the resolution the verb has always recorded", data["detail"])
		}
	})

	t.Run("reap", func(t *testing.T) {
		conn := newTestDB(t)
		activatedRunForNext(t, conn)
		first := stepIDNamed(t, conn, "first@0")
		_, err := engine.ClaimStep(conn, first, engine.ClaimOptions{Owner: "doomed", NowMS: model.NowMS()})
		testsupport.Must(t, err, "claim: %v", err)
		cmd := reapCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("reason", "spawn died"), "set --reason: %v", nil)
		w, buf := bufWriter(true)
		err = runStepReap(cmd, []string{model.FormatStepID(first)}, w)
		testsupport.Must(t, err, "step reap: %v\n%s", err, buf.String())

		data := rulingEvent(t, conn, first, engine.EventLeaseReaped)
		assertRuledByThisInvocation(t, data, engine.EventLeaseReaped)
		if data["forced"] != true || data["reason"] != "spawn died" {
			t.Errorf("forced=%#v reason=%#v; the reap's own payload must be untouched", data["forced"], data["reason"])
		}
	})
}

// TestStepRulingRefusesWhenCwdIsUnreadable: a cwd that cannot be resolved
// refuses the ruling before the engine runs, the same choice trust events made
// (DKT-595) — the ruling is retryable from a readable directory, the
// attribution can never be backfilled.
func TestStepRulingRefusesWhenCwdIsUnreadable(t *testing.T) {
	conn := newTestDB(t)
	gate := readyGate(t, conn)
	stubGetwdFailure(t)

	w, _ := bufWriter(true)
	err := runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, true, w)
	if err == nil {
		t.Fatal("an approve with an unreadable cwd was accepted")
	}
	if !strings.Contains(err.Error(), "readable directory") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
	if rulingEvent(t, conn, gate, engine.EventStepApproved) != nil {
		t.Error("the refused approve left an event behind")
	}
	if got := stepStatusByInstance(t, conn, "second@0"); got != db.StepPending {
		t.Errorf("second@0 = %q after the refusal, want it untouched", got)
	}
}
