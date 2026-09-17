package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-2291: the authorized mid-run AMENDMENT, made executable.
//
// RUN-98 / HRN-830, step STEP-9184 (verify@3): the rendered packet was
// internally inconsistent about one operator ruling. The body sections carried
// AC1's PRE-amendment wording while the packet header's scope line already
// carried the same ruling's second half, because DKT-869's scope refresh
// existed and its body counterpart did not. A verify-ac seat judging
// AC-as-written from that packet judges the superseded criterion.
//
// The bargain asserted here is DKT-869's, applied to the other frozen premise:
// the refresh copies only what the issue's description declares, refuses every
// state where a step could straddle it, rewrites no history, and leaves the
// discontinuity in the ledger.

// bodyInputWorkflow declares `issue.body` on both of its steps, so a rendered
// packet carries the description in BOTH places AC3 names: the `== REQUEST`
// every packet has, and the `== INPUT issue.body` section a declared input
// adds. The shared parking fixture declares no inputs and would assert only
// half of the criterion.
const bodyInputWorkflow = `
[pipeline]
name = "bodies"
version = 1

[match]
kind = ["task"]

[[step]]
name = "first"
after = []
executor = "w"
inputs = ["issue.body"]
emits = "out"

[[step]]
name = "second"
after = ["first"]
executor = "w"
inputs = ["issue.body"]
emits = "out2"
`

// refreshBody drives the engine entry point with the fixture's reason.
func refreshBody(conn *sql.DB, runID, issueID int) (*RefreshedBody, error) {
	return RefreshIssueBodyInRun(conn, runID, issueID, "operator amended AC1", nowMS+1)
}

// amend is the authorized act the refresh copies, as issue edit writes it.
func amend(t *testing.T, conn *sql.DB, issueID int, body string) {
	t.Helper()
	mustExec(t, conn, "UPDATE issues SET description = ? WHERE id = ?", body, issueID)
}

// runIssueBody reads the two columns the refresh is allowed to move.
func runIssueBody(t *testing.T, conn *sql.DB, runID, issueID int) (string, string) {
	t.Helper()
	var body, sha sql.NullString
	err := conn.QueryRow(
		"SELECT body_snapshot, body_sha256 FROM run_issues"+
			" WHERE run_id = ? AND issue_id = ?", runID, issueID).Scan(&body, &sha)
	testsupport.Must(t, err, "reading the body snapshot: %v", err)
	return body.String, sha.String
}

// recordedBodyOf is the read-back AC3 asserts: the body a handed-out step's
// own context carries, through the seam `step context` reads it by.
func recordedBodyOf(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	bundle, err := ReadContext(conn, stepID, nowMS+2)
	testsupport.Must(t, err, "reading the recorded context: %v", err)
	return bundle.Issue.BodySnapshot
}

// recordClaim puts a step in the state a recorded attempt leaves behind: one
// claim counted, a terminal status, and the `started_ms` that claim stamped.
// The read-back is reconstructed against that stamp, so a fixture that sets
// only `attempt` describes a step no claim ever handed out.
func recordClaim(t *testing.T, conn *sql.DB, stepID int, claimedAtMS int64) {
	t.Helper()
	mustExec(t, conn,
		`UPDATE steps SET status = ?, attempt = 1, started_ms = ? WHERE id = ?`,
		db.StepDone, claimedAtMS, stepID)
}

func assertNoBodyRefreshEvent(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	var n int
	err := conn.QueryRow(
		"SELECT COUNT(*) FROM events WHERE kind = ? AND run_id = ?",
		EventIssueBodyRefreshed, runID).Scan(&n)
	testsupport.Must(t, err, "counting body-refresh events: %v", err)
	if n != 0 {
		t.Errorf("a refused refresh wrote %d event(s); it must write none", n)
	}
}

// TestRefreshBodyCopiesLiveDescription is AC1: the copy is verbatim, the sha is
// recomputed from it, and a refresh with no amendment behind it is refused.
//
// The refusal is the gate, and the reason this verb takes no body of its own:
// it can only copy what the issue's description already declares, so a refresh
// nobody amended for has nothing to make real. Refusing rather than no-opping
// keeps the ledger honest — an event that refreshed nothing is a ruling that
// ruled nothing, and a reader auditing the discontinuities would have to open
// each one to find out which were real.
func TestRefreshBodyCopiesLiveDescription(t *testing.T) {
	t.Run("an amended description is copied and the sha recomputed", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)

		before, beforeSHA := runIssueBody(t, conn, runID, issue)
		if before != "body" {
			t.Fatalf("premise: the snapshot carries %q, want the activated body", before)
		}

		amended := "body\n\nAC1, as the operator amended it."
		amend(t, conn, issue, amended)

		outcome, err := refreshBody(conn, runID, issue)
		testsupport.Must(t, err, "refreshing: %v", err)

		got, gotSHA := runIssueBody(t, conn, runID, issue)
		if got != amended {
			t.Errorf("body_snapshot = %q, want the live description verbatim", got)
		}
		if want := workflow.SHA256([]byte(amended)); gotSHA != want {
			t.Errorf("body_sha256 = %q, want %q recomputed from the new body", gotSHA, want)
		}
		if gotSHA == beforeSHA {
			t.Error("body_sha256 did not move; it must follow the body it names")
		}
		if outcome.FromSHA256 != beforeSHA || outcome.ToSHA256 != gotSHA {
			t.Errorf("outcome = %+v, want the old and new shas", outcome)
		}
	})

	t.Run("an unchanged description is refused", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)

		_, err := refreshBody(conn, runID, issue)
		if err == nil {
			t.Fatal("a refresh with no amendment behind it was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeValidation {
			t.Errorf("error code = %v, want VALIDATION_ERROR: %v", code, err)
		}
		if !strings.Contains(err.Error(), "issue edit") {
			t.Errorf("the refusal does not name the gated writer:\n%s", err)
		}
		assertNoBodyRefreshEvent(t, conn, runID)

		// A re-declaration of the same text is the same non-authorization.
		amend(t, conn, issue, "body")
		if _, err := refreshBody(conn, runID, issue); err == nil {
			t.Error("a refresh over an unchanged description was accepted")
		}
		assertNoBodyRefreshEvent(t, conn, runID)
	})
}

// TestRefreshBodyRefusesWhileStepsStraddle is AC2 — DKT-869's quiescence rule
// (D2) applied to the body. A step holding a packet rendered under the frozen
// description must not record its artifact under the amended one: that is the
// one combination that would falsify a step's own provenance.
func TestRefreshBodyRefusesWhileStepsStraddle(t *testing.T) {
	for _, status := range []string{db.StepClaimed, db.StepRunning, db.StepGated} {
		t.Run(status, func(t *testing.T) {
			conn := mustDB(t)
			runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
			amend(t, conn, issue, "amended mid-flight")
			mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
				status, stepID)

			_, err := refreshBody(conn, runID, issue)
			if err == nil {
				t.Fatalf("a refresh under a %s step was accepted", status)
			}
			if code, ok := CodeOf(err); !ok || code != CodeConflict {
				t.Errorf("error code = %v, want CONFLICT: %v", code, err)
			}
			for _, want := range []string{"flaky@0", status} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q:\n%s", want, err)
				}
			}
			if got, _ := runIssueBody(t, conn, runID, issue); got != "body" {
				t.Errorf("body_snapshot = %q; a refused refresh must write nothing", got)
			}
			assertNoBodyRefreshEvent(t, conn, runID)
		})
	}

	// The open dispatch is the per-run half of the same rule: a manifest is a
	// frozen offer, and a relay spawning from it would claim rows whose packets
	// no longer say what the manifest's reader saw.
	t.Run("open dispatch", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		amend(t, conn, issue, "amended under an open manifest")
		mustExec(t, conn,
			`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
			 VALUES (?, 'open', 0, ?, ?)`, runID, nowMS+60_000, nowMS)

		_, err := refreshBody(conn, runID, issue)
		if err == nil {
			t.Fatal("a refresh under an open dispatch was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
		if !strings.Contains(err.Error(), "DISPATCH-") {
			t.Errorf("the refusal does not name the open dispatch:\n%s", err)
		}
		if got, _ := runIssueBody(t, conn, runID, issue); got != "body" {
			t.Errorf("body_snapshot = %q; a refused refresh must write nothing", got)
		}
		assertNoBodyRefreshEvent(t, conn, runID)
	})
}

// TestRefreshBodyReachesRemainingStepsOnly is AC3, the division of labour
// DKT-869 draws between an agreement and the history under it.
//
// The done step's answer to "what did this step see" must not move, and the
// pending step must render the amendment in BOTH places the packet states the
// request — `== REQUEST` and `== INPUT issue.body` both read
// `Issue.BodySnapshot` (packets/default.tmpl), so one substitution governs
// both and this asserts them together.
func TestRefreshBodyReachesRemainingStepsOnly(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(bodyInputWorkflow), "bodies.toml")
	issue := createIssue(t, conn, "amend me", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	runID := run.ID

	doneID := stepIDOf(t, conn, runID, issue, "first@0")
	pendingID := stepIDOf(t, conn, runID, issue, "second@0")

	// `attempt > 0` plus a terminal status is what makes the first step's
	// read-back the RECORDED assembly (recordedClaim), which is the state AC3's
	// first assertion is about. `started_ms` is the claim that assembly is
	// reconstructed against, so the fixture stamps it as the claim would.
	recordClaim(t, conn, doneID, nowMS)

	recordedBefore := recordedBodyOf(t, conn, doneID)

	amended := "body\n\nAC1, as the operator amended it."
	amend(t, conn, issue, amended)

	outcome, err := refreshBody(conn, runID, issue)
	testsupport.Must(t, err, "refreshing: %v", err)
	if len(outcome.Steps) != 1 || outcome.Steps[0] != "second@0" {
		t.Errorf("outcome.Steps = %v, want only the non-terminal instance", outcome.Steps)
	}

	// It rewrites no history: the recorded context is byte-identical.
	recordedAfter := recordedBodyOf(t, conn, doneID)
	if recordedAfter != recordedBefore {
		t.Errorf("the recorded context of a done step moved:\nbefore: %s\nafter:  %s",
			recordedBefore, recordedAfter)
	}

	// And the remaining step renders the refreshed body in both places.
	packet, err := RenderStep(conn, pendingID, "", nowMS+2)
	testsupport.Must(t, err, "rendering the remaining step: %v", err)
	request := strings.Index(packet.Packet, "== REQUEST")
	input := strings.Index(packet.Packet, "== INPUT issue.body")
	if request < 0 || input < 0 {
		t.Fatalf("the packet lacks one of the two body sections:\n%s", packet.Packet)
	}
	for _, section := range []struct {
		name string
		at   int
	}{{"== REQUEST", request}, {"== INPUT issue.body", input}} {
		if !strings.Contains(packet.Packet[section.at:], amended) {
			t.Errorf("%s does not carry the refreshed body:\n%s",
				section.name, packet.Packet)
		}
	}
}

// TestRefreshBodyKeepsRecordedBodyAcrossLaterRefreshes extends AC3 past the
// single-refresh case its named check exercises.
//
// A second refresh must not move what the first one's steps were handed. Each
// case below is one way the answer can only come from the attempt's own claim:
// a step the first refresh REACHED and a step it did not, a step parked at
// `waiting-human` with its artifact already recorded, and a step whose row
// predates a refresh its claim followed.
func TestRefreshBodyKeepsRecordedBodyAcrossLaterRefreshes(t *testing.T) {
	// setup activates the two-step fixture and returns the run and its steps.
	setup := func(t *testing.T) (*sql.DB, int, int, int, int) {
		t.Helper()
		conn := mustDB(t)
		registerSource(t, conn, []byte(bodyInputWorkflow), "bodies.toml")
		issue := createIssue(t, conn, "amend me", "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)
		return conn, run.ID, issue,
			stepIDOf(t, conn, run.ID, issue, "first@0"),
			stepIDOf(t, conn, run.ID, issue, "second@0")
	}

	refreshTo := func(t *testing.T, conn *sql.DB, runID, issue int, body string, atMS int64) {
		t.Helper()
		amend(t, conn, issue, body)
		_, err := RefreshIssueBodyInRun(conn, runID, issue, "operator amended", atMS)
		testsupport.Must(t, err, "refreshing: %v", err)
	}

	t.Run("a step claimed between two refreshes keeps the middle body", func(t *testing.T) {
		conn, runID, issue, first, _ := setup(t)

		// R1 reaches the still-pending step; the step is then claimed and
		// recorded under the body R1 installed; R2 supersedes it afterwards.
		v1 := "body v1"
		refreshTo(t, conn, runID, issue, v1, nowMS+1)
		recordClaim(t, conn, first, nowMS+2)
		refreshTo(t, conn, runID, issue, "body v2", nowMS+3)

		if got := recordedBodyOf(t, conn, first); got != v1 {
			t.Errorf("recorded body = %q, want %q — the body its claim was handed", got, v1)
		}
	})

	t.Run("a step claimed before any refresh keeps the original body", func(t *testing.T) {
		conn, runID, issue, first, _ := setup(t)

		recordClaim(t, conn, first, nowMS)
		refreshTo(t, conn, runID, issue, "body v1", nowMS+1)
		refreshTo(t, conn, runID, issue, "body v2", nowMS+2)

		if got := recordedBodyOf(t, conn, first); got != "body" {
			t.Errorf("recorded body = %q, want the activated body", got)
		}
	})

	t.Run("a parked step that already recorded keeps its body", func(t *testing.T) {
		conn, runID, issue, first, _ := setup(t)

		// `waiting-human` is refreshable, so the refresh REACHES this step and
		// names it in the event — but its artifact was recorded under the body
		// its claim was handed, and that is what its read-back must state.
		mustExec(t, conn,
			`UPDATE steps SET status = ?, attempt = 1, started_ms = ? WHERE id = ?`,
			db.StepWaitingHuman, nowMS, first)

		refreshTo(t, conn, runID, issue, "body amended under the park", nowMS+1)

		if got := recordedBodyOf(t, conn, first); got != "body" {
			t.Errorf("recorded body = %q, want the body its claim was handed", got)
		}
	})

	t.Run("a fix-round claim after a refresh reads the refreshed body", func(t *testing.T) {
		conn, runID, issue, first, _ := setup(t)

		// The step ROW predates the refresh; its current attempt does not. A
		// read-back anchored on the row would hand it the superseded text.
		amended := "body, as the operator amended it"
		refreshTo(t, conn, runID, issue, amended, nowMS+1)
		recordClaim(t, conn, first, nowMS+2)

		if got := recordedBodyOf(t, conn, first); got != amended {
			t.Errorf("recorded body = %q, want the live refreshed body %q", got, amended)
		}
	})
}

// TestRefreshBodyRecordsTheDiscontinuity is AC4: one event, in the same
// transaction, carrying both shas, the steps it reaches and the reason.
//
// This is what keeps the refresh an exception rather than a hole. Two steps of
// one run rendering two different requests is legible only because one dated,
// attributable event says when the premise moved and why.
func TestRefreshBodyRecordsTheDiscontinuity(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)

	_, beforeSHA := runIssueBody(t, conn, runID, issue)
	amended := "body, amended"
	amend(t, conn, issue, amended)

	if _, err := refreshBody(conn, runID, issue); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE kind = ? AND run_id = ? AND issue_id = ?`,
		EventIssueBodyRefreshed, runID, issue).Scan(&data)
	testsupport.Must(t, err, "reading the refresh event: %v", err)

	var payload struct {
		Issue      string   `json:"issue"`
		Reason     string   `json:"reason"`
		FromSHA256 string   `json:"from_sha256"`
		ToSHA256   string   `json:"to_sha256"`
		Steps      []string `json:"steps"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(data), &payload), "decoding the event")

	if payload.Issue != model.FormatID(issue) {
		t.Errorf("event issue = %q, want %q", payload.Issue, model.FormatID(issue))
	}
	if payload.Reason != "operator amended AC1" {
		t.Errorf("event reason = %q, want the operator's reason", payload.Reason)
	}
	if payload.FromSHA256 != beforeSHA {
		t.Errorf("event from_sha256 = %q, want the superseded sha %q",
			payload.FromSHA256, beforeSHA)
	}
	if want := workflow.SHA256([]byte(amended)); payload.ToSHA256 != want {
		t.Errorf("event to_sha256 = %q, want %q", payload.ToSHA256, want)
	}
	if len(payload.Steps) != 1 || payload.Steps[0] != "flaky@0" {
		t.Errorf("event steps = %v, want the reached instance", payload.Steps)
	}
}
