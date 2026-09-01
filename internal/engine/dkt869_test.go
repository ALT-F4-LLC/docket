package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-869: the authorized mid-run widen, made executable.
//
// RUN-52 (VPL-434), 2026-08-26 retro: "scope snapshots per-step at creation and
// does not reach the already-created fix@2 step — no engine verb refreshes an
// existing step's scope". The panel had rejected the work as out of scope, the
// operator had AGREED and widened it, and the honest remedy still could not be
// rendered into the step that was to perform it, so the issue was abandoned
// mid-loop. DKT-741 pinned the freeze (dkt741_test.go) and named abandon +
// re-plan as the only disposition; this is the narrower one, for the case where
// the run's premise is intact and one declaration was corrected.
//
// What is asserted here is the whole bargain: the refresh reaches the pending
// step, it copies ONLY what the gated writer declared, it refuses every state
// where a step could straddle the change, it rewrites nothing else in the
// snapshot, and it leaves the discontinuity in the ledger. The regression guard
// for "a step never refreshed behaves exactly as before" is
// TestScopeEditDoesNotReachAnActivatedPacket in dkt741_test.go, which still
// runs unchanged — the freeze is still the default, and this verb is the
// exception to it.

// refresh drives the engine entry point with the fixture's reason.
func refresh(conn *sql.DB, runID, issueID int) (*RefreshedScope, error) {
	return RefreshIssueScopeInRun(conn, runID, issueID, "scope widened", nowMS)
}

// widen is the authorized act the refresh copies: the ONLY writer of
// `issues.scope_globs`, exactly as `issue edit --scope` calls it.
func widen(t *testing.T, conn *sql.DB, issueID int, globsJSON string) {
	t.Helper()
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issueID, globsJSON),
		"widening scope")
}

// TestRefreshReachesTheAlreadyCreatedStep is the acceptance criterion, in the
// shape RUN-52 met it: a step that already exists renders the widened scope
// after the refresh, and its diff will be recorded over the same paths.
func TestRefreshReachesTheAlreadyCreatedStep(t *testing.T) {
	conn := mustDB(t)
	runID, issue, stepID := scopedIssueInRun(t, conn, `["cli/src/command/start.rs"]`)

	before, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "rendering before the widen: %v", err)
	if !strings.Contains(before.Packet, "cli/src/command/start.rs") {
		t.Fatalf("premise: the packet does not carry the declared scope:\n%s",
			before.Packet)
	}

	widen(t, conn, issue, `["cli/src/command/start.rs","script/install.sh","makefile"]`)

	// The edit ALONE still does not reach it — DKT-741's freeze is the default
	// and this test must not pass because the freeze quietly stopped holding.
	stillFrozen, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "rendering after the widen: %v", err)
	if stillFrozen.Packet != before.Packet {
		t.Fatalf("the widen alone changed the packet; §9 item 5's edit "+
			"immunity requires the refresh to be a separate act:\n%s",
			stillFrozen.Packet)
	}

	outcome, err := refresh(conn, runID, issue)
	testsupport.Must(t, err, "refreshing: %v", err)

	after, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "rendering after the refresh: %v", err)
	for _, added := range []string{"script/install.sh", "makefile"} {
		if !strings.Contains(after.Packet, added) {
			t.Errorf("the packet does not carry %q after the refresh:\n%s",
				added, after.Packet)
		}
	}

	// The diff scope reads the same blob, so the packet and the recorded diff
	// still cannot disagree — the refresh moved BOTH or it moved neither.
	frozen, err := snapshotScope(conn, runID, issue)
	testsupport.Must(t, err, "reading the snapshot scope: %v", err)
	if len(frozen) != 3 || frozen[0] != "cli/src/command/start.rs" ||
		frozen[1] != "script/install.sh" || frozen[2] != "makefile" {
		t.Errorf("snapshotScope = %v, want the widened declaration in the "+
			"author's order", frozen)
	}

	if len(outcome.From) != 1 || len(outcome.To) != 3 {
		t.Errorf("outcome = %+v, want the one-path scope replaced by the three-path one",
			outcome)
	}
	if len(outcome.Steps) != 1 || outcome.Steps[0] != "flaky@0" {
		t.Errorf("outcome.Steps = %v, want the one live instance", outcome.Steps)
	}
}

// TestRefreshRefusesWithoutTheWiden is the GATE, and the reason this verb takes
// no `--scope` of its own: it can only copy what `issue create|edit --scope`
// declared, so a refresh nobody authorized a widen for has nothing to make
// real. Without this refusal the verb would be a second, ungated writer of what
// a live run renders.
func TestRefreshRefusesWithoutTheWiden(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)

	_, err := refresh(conn, runID, issue)
	if err == nil {
		t.Fatal("a refresh with no declared widen behind it was accepted")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	// It must name the gated verb: an operator here has typed the second act
	// without the first, and the fix is the first.
	if !strings.Contains(err.Error(), "issue edit") {
		t.Errorf("the refusal does not name the gated writer:\n%s", err)
	}
	assertNoRefreshEvent(t, conn, runID)

	// A RE-DECLARATION of the same globs is the same non-authorization: the
	// operator wrote the column, but nothing about the run changed, and an
	// event for it would be a ruling that ruled nothing.
	widen(t, conn, issue, `["internal/a/**"]`)
	if _, err := refresh(conn, runID, issue); err == nil {
		t.Error("a refresh over an unchanged declaration was accepted")
	}
	assertNoRefreshEvent(t, conn, runID)
}

// TestRefreshRefusesAStraddlingStep is the quiescence half — repin's rule
// (repin.go) applied to the other frozen premise. A step holding a packet
// rendered under the old scope must not record its diff under the new one.
func TestRefreshRefusesAStraddlingStep(t *testing.T) {
	for _, status := range []string{db.StepClaimed, db.StepRunning, db.StepGated} {
		t.Run(status, func(t *testing.T) {
			conn := mustDB(t)
			runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
			widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
			mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
				status, stepID)

			_, err := refresh(conn, runID, issue)
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
			assertFrozenScope(t, conn, runID, issue, "internal/a/**")
			assertNoRefreshEvent(t, conn, runID)
		})
	}
}

// TestRefreshRefusesAnOpenDispatch: a manifest is a frozen offer, and its relay
// must see the world before or after the refresh, never both.
func TestRefreshRefusesAnOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
	mustExec(t, conn,
		`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
		 VALUES (?, 'open', 0, ?, ?)`, runID, nowMS+60_000, nowMS)

	_, err := refresh(conn, runID, issue)
	if err == nil {
		t.Fatal("a refresh under an open dispatch was accepted")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	if !strings.Contains(err.Error(), "DISPATCH-") {
		t.Errorf("the refusal does not name the open dispatch:\n%s", err)
	}
	assertFrozenScope(t, conn, runID, issue, "internal/a/**")
}

// TestRefreshRefusesWhereItCouldOnlyRewriteHistory covers the states in which a
// refresh means nothing: nothing left to render, a run that froze nothing yet,
// a terminal run, an issue this run does not hold.
func TestRefreshRefusesWhereItCouldOnlyRewriteHistory(t *testing.T) {
	t.Run("every step of the issue is terminal", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
		mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
			db.StepDone, stepID)

		_, err := refresh(conn, runID, issue)
		if err == nil {
			t.Fatal("a refresh over a fully-recorded issue was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
		assertFrozenScope(t, conn, runID, issue, "internal/a/**")
	})

	t.Run("a run that never activated", func(t *testing.T) {
		conn := mustDB(t)
		registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
		issue := createIssue(t, conn, "not yet", "body", "task", nil)
		widen(t, conn, issue, `["internal/a/**"]`)
		run := startRun(t, conn, issue)

		_, err := refresh(conn, run.ID, issue)
		if err == nil {
			t.Fatal("a refresh on a planning run was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
	})

	t.Run("a terminal run", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
		mustExec(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
			string(model.RunAbandoned), runID)

		_, err := refresh(conn, runID, issue)
		if err == nil {
			t.Fatal("a refresh on an abandoned run was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
	})

	t.Run("an issue the run does not hold", func(t *testing.T) {
		conn := mustDB(t)
		runID, _, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		other := createIssue(t, conn, "elsewhere", "body", "task", nil)

		_, err := refresh(conn, runID, other)
		if err == nil {
			t.Fatal("a refresh named an issue the run does not hold")
		}
		if code, ok := CodeOf(err); !ok || code != CodeNotFound {
			t.Errorf("error code = %v, want NOT_FOUND: %v", code, err)
		}
	})

	t.Run("no reason", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)

		_, err := RefreshIssueScopeInRun(conn, runID, issue, "  ", nowMS)
		if err == nil {
			t.Fatal("a refresh with no reason was accepted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeValidation {
			t.Errorf("error code = %v, want VALIDATION_ERROR: %v", code, err)
		}
		assertFrozenScope(t, conn, runID, issue, "internal/a/**")
	})
}

// TestRefreshRewritesTheScopeAndNothingElse is the immunity that SURVIVES the
// exception. The snapshot also carries the title, kind and labels a mid-run
// edit must never reach — labels decide how a step routes — so the refresh has
// to be surgical, not a re-snapshot.
func TestRefreshRewritesTheScopeAndNothingElse(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
	issue := createIssue(t, conn, "widen me", "body", "task", []string{"alpha", "beta"})
	widen(t, conn, issue, `["internal/a/**"]`)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	before := snapshotBlob(t, conn, run.ID, issue)
	for _, want := range []string{`"title":"widen me"`, `"kind":"task"`,
		`"labels":["alpha","beta"]`} {
		if !strings.Contains(before, want) {
			t.Fatalf("premise: the snapshot does not carry %s — this test "+
				"would pass vacuously:\n%s", want, before)
		}
	}

	// Everything a mid-run edit could touch, edited mid-run.
	mustExec(t, conn,
		`UPDATE issues SET title = ?, kind = ? WHERE id = ?`,
		"renamed after activation", "bug", issue)
	mustExec(t, conn, `DELETE FROM issue_labels WHERE issue_id = ?`, issue)
	widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)

	if _, err := refresh(conn, run.ID, issue); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	after := snapshotBlob(t, conn, run.ID, issue)
	var was, now map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(before), &was), "decoding before")
	testsupport.Must(t, json.Unmarshal([]byte(after), &now), "decoding after")

	for _, key := range []string{"title", "kind", "labels"} {
		if fmtJSON(was[key]) != fmtJSON(now[key]) {
			t.Errorf("%s moved from %s to %s; only scope may move",
				key, fmtJSON(was[key]), fmtJSON(now[key]))
		}
	}
	if fmtJSON(now["scope"]) != `["internal/a/**","internal/b/**"]` {
		t.Errorf("scope = %s, want the widened declaration", fmtJSON(now["scope"]))
	}
}

// TestRefreshedSnapshotIsByteIdenticalApartFromScope pins the round trip the
// rewrite depends on. reScopedSnapshot decodes and re-encodes through
// `issueSnapshotFields`, so a snapshot whose scope is replaced with the SAME
// value must come back byte for byte — anything else means a key was dropped
// or the canonical order moved, and either would corrupt a live run's snapshot
// while reporting success.
func TestRefreshedSnapshotIsByteIdenticalApartFromScope(t *testing.T) {
	for _, blob := range []string{
		`{"title":"t","kind":"task","labels":[],"scope":[]}`,
		`{"title":"t","kind":"bug","labels":["a","b"],"scope":["x/**","y/**"]}`,
		`{"title":"t","kind":"task","labels":["a"],"scope":["x"],"linked":{"blocks.diff":[7,9]}}`,
	} {
		scope, err := decodeSnapshotScope(blob)
		testsupport.Must(t, err, "decoding %s: %v", blob, err)
		out, err := reScopedSnapshot(blob, scope)
		testsupport.Must(t, err, "re-encoding %s: %v", blob, err)
		if out != blob {
			t.Errorf("re-encoding with the same scope changed the blob:\n"+
				"before: %s\nafter:  %s", blob, out)
		}
	}
}

// TestRefreshRecordsTheDiscontinuity is what keeps this an exception rather
// than a hole: two steps of one run rendering two different scopes is legible
// only because one dated, attributable event says when the premise moved and
// why.
func TestRefreshRecordsTheDiscontinuity(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)

	if _, err := refresh(conn, runID, issue); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE kind = ? AND run_id = ? AND issue_id = ?`,
		EventIssueScopeRefreshed, runID, issue).Scan(&data)
	testsupport.Must(t, err, "reading the refresh event: %v", err)

	var payload struct {
		Issue  string   `json:"issue"`
		Reason string   `json:"reason"`
		From   []string `json:"from"`
		To     []string `json:"to"`
		Steps  []string `json:"steps"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(data), &payload), "decoding the event")

	if payload.Issue != model.FormatID(issue) || payload.Reason != "scope widened" {
		t.Errorf("event payload = %+v, want the issue and the operator's reason", payload)
	}
	if len(payload.From) != 1 || len(payload.To) != 2 {
		t.Errorf("event payload = %+v, want BOTH scopes — the old one is what "+
			"the recorded diffs were computed over", payload)
	}
	if len(payload.Steps) != 1 || payload.Steps[0] != "flaky@0" {
		t.Errorf("event payload steps = %v, want the reached instance", payload.Steps)
	}

	// §9 item 2: the kind is attributable, and it is a person's act.
	actor, ok := ActorFor(EventIssueScopeRefreshed)
	if !ok || actor != ActorHuman {
		t.Errorf("ActorFor(%s) = %v/%v, want ActorHuman",
			EventIssueScopeRefreshed, actor, ok)
	}
}

// TestScopeEditAdvisoryNamesTheRefresh: DKT-741's disclosure fires at the
// moment an operator spends a widen on a live run, and it is the only place
// this verb is discoverable from. An advisory still naming only abandon +
// re-plan would leave RUN-52's conductor exactly where it was.
func TestScopeEditAdvisoryNamesTheRefresh(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)

	warnings := ScopeEditFrozenForActiveRuns(conn, issue)
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	want := "run refresh-scope " + model.FormatRunID(runID) +
		" --issue " + model.FormatID(issue)
	if !strings.Contains(warnings[0], want) {
		t.Errorf("the advisory does not name %q:\n%s", want, warnings[0])
	}
}

func snapshotBlob(t *testing.T, conn *sql.DB, runID, issueID int) string {
	t.Helper()
	var blob string
	err := conn.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&blob)
	testsupport.Must(t, err, "reading the snapshot: %v", err)
	return blob
}

func assertFrozenScope(t *testing.T, conn *sql.DB, runID, issueID int, want ...string) {
	t.Helper()
	got, err := snapshotScope(conn, runID, issueID)
	testsupport.Must(t, err, "reading the snapshot scope: %v", err)
	if len(got) != len(want) {
		t.Fatalf("snapshotScope = %v, want %v — a refused refresh must write "+
			"nothing", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshotScope = %v, want %v", got, want)
		}
	}
}

func assertNoRefreshEvent(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND run_id = ?`,
		EventIssueScopeRefreshed, runID).Scan(&n)
	testsupport.Must(t, err, "counting refresh events: %v", err)
	if n != 0 {
		t.Errorf("%d %s event(s) recorded by a refused refresh",
			n, EventIssueScopeRefreshed)
	}
}

func fmtJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return "<unencodable>"
	}
	return string(out)
}
