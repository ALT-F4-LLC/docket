package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-408: verify-pins made drift visible; nothing made it survivable. Four
// runs across three projects were abandoned because a corpus install replaced
// pinned files mid-run and the only disposition was a funeral. RepinRun is the
// recovery half, and these tests pin its two contracts: the remaining steps'
// agreement moves, and completed steps' recorded provenance never does.

// dumpRows renders every row a query returns, every column, as one string —
// the byte-identity oracle for "this table's record of that step did not
// change". Generic scanning rather than named columns, so a column added later
// is covered without anyone remembering to add it here.
func dumpRows(t *testing.T, conn *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(query, args...)
	testsupport.Must(t, err, "query %q: %v", query, err)
	defer rows.Close()

	cols, err := rows.Columns()
	testsupport.Must(t, err, "columns: %v", err)

	var b strings.Builder
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		testsupport.Must(t, rows.Scan(ptrs...), "scan")
		for i, c := range cols {
			fmt.Fprintf(&b, "%s=%q|", c, string(raw[i]))
		}
		b.WriteString("\n")
	}
	testsupport.Must(t, rows.Err(), "rows: %v", err)
	return b.String()
}

// repinEvents reads the run's run-repinned events' data payloads, oldest first.
func repinEvents(t *testing.T, conn *sql.DB, runID int) []map[string]any {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq`,
		runID, EventRunRepinned)
	testsupport.Must(t, err, "reading repin events: %v", err)
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var data string
		testsupport.Must(t, rows.Scan(&data), "scan")
		var m map[string]any
		testsupport.Must(t, json.Unmarshal([]byte(data), &m), "decode %q", data)
		out = append(out, m)
	}
	testsupport.Must(t, rows.Err(), "rows: %v", err)
	return out
}

// pinSHA reads one pin row's stored hash.
func pinSHA(t *testing.T, conn *sql.DB, runID int, ref string) string {
	t.Helper()
	var sha string
	err := conn.QueryRow(
		`SELECT sha256 FROM pins WHERE run_id = ? AND ref = ?`, runID, ref).Scan(&sha)
	testsupport.Must(t, err, "reading pin %s: %v", ref, err)
	return sha
}

// stepID resolves a step row id by instance.
func stepID(t *testing.T, conn *sql.DB, runID int, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND instance = ?`, runID, instance).Scan(&id)
	testsupport.Must(t, err, "resolving %s: %v", instance, err)
	return id
}

// TestRepinUpdatesPendingAgreementAndPreservesCompletedProvenance is the
// demanded mix case: one pinned file, referenced by a COMPLETED step and by
// steps still pending, drifts. The repin must move the run's current agreement
// (so the pending steps proceed under the new bytes), record old sha -> new
// sha per changed ref, and leave every table that records the completed step's
// work byte-identical.
func TestRepinUpdatesPendingAgreementAndPreservesCompletedProvenance(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	// Two file pins: one that will drift (shared by the whole run — pins are
	// run-scoped, so the completed implement@0 and the pending review both
	// reference it), one steady control.
	oldSHA := pinAFile(t, conn, run.ID, root, "contracts/shared.md", "BEFORE\n")
	steady := pinAFile(t, conn, run.ID, root, "contracts/steady.md", "STEADY\n")

	// implement@0 completes with an artifact — the provenance the acceptance
	// criterion protects.
	doneID := stepID(t, conn, run.ID, "implement@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, doneID)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = db.InsertArtifactTx(tx, db.Artifact{
		RunID: run.ID, StepID: doneID, Kind: "change-summary",
		Body: "did the thing", SHA256: "abc123",
	}, nowMS)
	testsupport.Must(t, err, "InsertArtifactTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	// The corpus install: the shared contract's bytes change on disk.
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/shared.md"), []byte("AFTER\n"), 0o644),
		"replacing the contract")

	// Snapshot every table that records the completed step's work.
	stepBefore := dumpRows(t, conn, `SELECT * FROM steps WHERE id = ?`, doneID)
	artifactsBefore := dumpRows(t, conn,
		`SELECT * FROM artifacts WHERE step_id = ? ORDER BY id`, doneID)
	eventsBefore := dumpRows(t, conn,
		`SELECT * FROM events WHERE step_id = ? ORDER BY seq`, doneID)

	outcome, err := repinRunIn(conn, run.ID, "corpus install 2026-08-20", nowMS,
		[]string{root})
	testsupport.Must(t, err, "repinRunIn: %v", err)

	// The current agreement moved — this is what un-wedges the pending steps:
	// every claim and render verifies against the pins table, so the new hash
	// here IS "pending steps' pins update".
	newSHA := pinSHA(t, conn, run.ID, "contracts/shared.md")
	if newSHA == oldSHA {
		t.Fatalf("the pin still records %s; the run would stay wedged", oldSHA)
	}
	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn after repin: %v", err)
	if !report.Sound() {
		t.Errorf("the run is still unsound after repin: %s", PinReportReason(report))
	}

	// The steady pin was not touched.
	if got := pinSHA(t, conn, run.ID, "contracts/steady.md"); got != steady {
		t.Errorf("the unchanged pin moved %s -> %s; repin must touch only drifted pins",
			steady, got)
	}

	// The outcome names exactly the drifted ref with both hashes.
	if len(outcome.Repinned) != 1 {
		t.Fatalf("repinned %d pin(s), want 1: %+v", len(outcome.Repinned), outcome.Repinned)
	}
	change := outcome.Repinned[0]
	if change.Ref != "contracts/shared.md" ||
		change.OldSHA256 != oldSHA || change.NewSHA256 != newSHA {
		t.Errorf("change = %+v, want contracts/shared.md %s -> %s", change, oldSHA, newSHA)
	}

	// COMPLETED PROVENANCE IS BYTE-IDENTICAL. The steps row, the artifacts,
	// and the step's own events are exactly what they were — the repin
	// transaction touches the pins table and appends run-repinned events, and
	// nothing else.
	if got := dumpRows(t, conn, `SELECT * FROM steps WHERE id = ?`, doneID); got != stepBefore {
		t.Errorf("the completed step's row changed:\nbefore: %s\nafter:  %s", stepBefore, got)
	}
	if got := dumpRows(t, conn,
		`SELECT * FROM artifacts WHERE step_id = ? ORDER BY id`, doneID); got != artifactsBefore {
		t.Errorf("the completed step's artifacts changed:\nbefore: %s\nafter:  %s",
			artifactsBefore, got)
	}
	if got := dumpRows(t, conn,
		`SELECT * FROM events WHERE step_id = ? ORDER BY seq`, doneID); got != eventsBefore {
		t.Errorf("the completed step's events changed:\nbefore: %s\nafter:  %s",
			eventsBefore, got)
	}

	// ONE EVENT PER CHANGED REF, carrying old -> new. This is where the
	// completed step's original pin survives as history: it recorded before
	// this event's seq, so the agreement it worked under is the event's
	// old_sha256.
	events := repinEvents(t, conn, run.ID)
	if len(events) != 1 {
		t.Fatalf("recorded %d run-repinned event(s), want 1 (one per changed ref)",
			len(events))
	}
	e := events[0]
	if e["ref"] != "contracts/shared.md" || e["kind"] != db.PinKindFile {
		t.Errorf("event names %v %v, want file contracts/shared.md", e["kind"], e["ref"])
	}
	if e["old_sha256"] != oldSHA {
		t.Errorf("event old_sha256 = %v, want %s — without it the completed step's "+
			"agreement is unrecoverable", e["old_sha256"], oldSHA)
	}
	if e["new_sha256"] != newSHA {
		t.Errorf("event new_sha256 = %v, want %s", e["new_sha256"], newSHA)
	}
	if e["reason"] != "corpus install 2026-08-20" {
		t.Errorf("event reason = %v, want the operator's --reason verbatim", e["reason"])
	}
}

// TestRepinRecordsOneEventPerChangedRef pins the per-ref granularity with two
// drifted files: two events, each self-contained.
func TestRepinRecordsOneEventPerChangedRef(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	oldA := pinAFile(t, conn, run.ID, root, "contracts/a.md", "A1\n")
	oldB := pinAFile(t, conn, run.ID, root, "policy.toml", "B1\n")
	for ref, body := range map[string]string{
		"contracts/a.md": "A2\n", "policy.toml": "B2\n",
	} {
		testsupport.Must(t, os.WriteFile(
			filepath.Join(root, ref), []byte(body), 0o644), "rewriting %s", ref)
	}

	outcome, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	testsupport.Must(t, err, "repinRunIn: %v", err)
	if len(outcome.Repinned) != 2 {
		t.Fatalf("repinned %d, want 2", len(outcome.Repinned))
	}

	events := repinEvents(t, conn, run.ID)
	if len(events) != 2 {
		t.Fatalf("recorded %d run-repinned event(s), want one per changed ref", len(events))
	}
	old := map[string]string{"contracts/a.md": oldA, "policy.toml": oldB}
	for _, e := range events {
		ref, _ := e["ref"].(string)
		want, known := old[ref]
		if !known {
			t.Errorf("event names unexpected ref %q", ref)
			continue
		}
		if e["old_sha256"] != want {
			t.Errorf("event for %s carries old %v, want %s", ref, e["old_sha256"], want)
		}
		delete(old, ref)
	}
	if len(old) != 0 {
		t.Errorf("no event recorded for %v", old)
	}
}

// TestRepinRefusesWhileAStepIsInFlight: a claimed executor holds a packet
// rendered under the old agreement; completing under the new one would be the
// straddle that falsifies its provenance the moment it records.
func TestRepinRefusesWhileAStepIsInFlight(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	oldSHA := pinAFile(t, conn, run.ID, root, "contracts/c.md", "OLD\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/c.md"), []byte("NEW\n"), 0o644), "rewrite")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE run_id = ? AND instance = ?`,
		db.StepClaimed, run.ID, "implement@0")

	_, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	if err == nil {
		t.Fatal("repin proceeded under a claimed step")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	if !strings.Contains(err.Error(), "implement@0") {
		t.Errorf("refusal %q does not name the in-flight step", err)
	}
	if got := pinSHA(t, conn, run.ID, "contracts/c.md"); got != oldSHA {
		t.Errorf("the pin moved to %s under a refusal; nothing may change", got)
	}
	if events := repinEvents(t, conn, run.ID); len(events) != 0 {
		t.Errorf("%d run-repinned event(s) recorded by a refused repin", len(events))
	}
}

// TestRepinRefusesWhenOnlyCompletedStepsRemain is the demanded refusal: a
// repin that would touch pins referenced only by completed steps. Three
// shapes of the same fact — a done run, an abandoned run, and an active run
// whose steps are all terminal (the rollup is lazy, so the steps are asked
// directly) — and each refuses with the pins untouched.
func TestRepinRefusesWhenOnlyCompletedStepsRemain(t *testing.T) {
	cases := []struct {
		name  string
		wedge func(t *testing.T, conn *sql.DB, runID int)
	}{
		{"run done", func(t *testing.T, conn *sql.DB, runID int) {
			execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)
		}},
		{"run abandoned", func(t *testing.T, conn *sql.DB, runID int) {
			execSQL(t, conn, `UPDATE runs SET status = 'abandoned' WHERE id = ?`, runID)
		}},
		{"every step terminal on an active run", func(t *testing.T, conn *sql.DB, runID int) {
			execSQL(t, conn, `UPDATE steps SET status = ? WHERE run_id = ?`,
				db.StepDone, runID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)
			root := t.TempDir()

			oldSHA := pinAFile(t, conn, run.ID, root, "contracts/d.md", "OLD\n")
			testsupport.Must(t, os.WriteFile(
				filepath.Join(root, "contracts/d.md"), []byte("NEW\n"), 0o644), "rewrite")
			tc.wedge(t, conn, run.ID)

			_, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
			if err == nil {
				t.Fatal("repin rewrote pins that only completed steps reference")
			}
			if code, ok := CodeOf(err); !ok || code != CodeConflict {
				t.Errorf("error code = %v, want CONFLICT: %v", code, err)
			}
			if got := pinSHA(t, conn, run.ID, "contracts/d.md"); got != oldSHA {
				t.Errorf("the completed steps' pin was rewritten to %s; it must "+
					"still record %s", got, oldSHA)
			}
			if events := repinEvents(t, conn, run.ID); len(events) != 0 {
				t.Errorf("%d run-repinned event(s) recorded by a refused repin",
					len(events))
			}
		})
	}
}

// TestRepinRefusesUnderAnOpenDispatch: a manifest is a frozen offer made under
// the current pins; its relay must see the world before or after the repin,
// never both.
func TestRepinRefusesUnderAnOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/e.md", "OLD\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/e.md"), []byte("NEW\n"), 0o644), "rewrite")
	execSQL(t, conn,
		`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
		 VALUES (?, 'open', 0, ?, ?)`, run.ID, nowMS+60_000, nowMS)

	_, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	if err == nil {
		t.Fatal("repin proceeded under an open dispatch")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("error code = %v, want CONFLICT: %v", code, err)
	}
	if !strings.Contains(err.Error(), "DISPATCH-") {
		t.Errorf("refusal %q does not name the open dispatch", err)
	}
}

// TestRepinRefusesOnAMissingPin: repin adopts current disk bytes, and a
// missing ref has none — partial recovery would report success over a run
// still wedged on the rest.
func TestRepinRefusesOnAMissingPin(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	changed := pinAFile(t, conn, run.ID, root, "contracts/f.md", "OLD\n")
	pinAFile(t, conn, run.ID, root, "contracts/gone.md", "BODY\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/f.md"), []byte("NEW\n"), 0o644), "rewrite")
	testsupport.Must(t, os.Remove(filepath.Join(root, "contracts/gone.md")), "remove")

	_, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	if err == nil {
		t.Fatal("repin proceeded with a pinned ref missing from disk")
	}
	if code, ok := CodeOf(err); !ok || code != CodeNotFound {
		t.Errorf("error code = %v, want NOT_FOUND: %v", code, err)
	}
	if !strings.Contains(err.Error(), "contracts/gone.md") {
		t.Errorf("refusal %q does not name the missing ref", err)
	}
	// ALL-OR-NOTHING: the changed pin was not repinned around the refusal.
	if got := pinSHA(t, conn, run.ID, "contracts/f.md"); got != changed {
		t.Errorf("the changed pin moved to %s despite the refusal", got)
	}
}

// TestRepinIsANoOpOnASoundRun: nothing drifted means nothing moves and no
// event is invented, so running it twice — or defensively — is safe.
func TestRepinIsANoOpOnASoundRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()
	pinAFile(t, conn, run.ID, root, "contracts/g.md", "SAME\n")

	for range 2 {
		outcome, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
		testsupport.Must(t, err, "repinRunIn: %v", err)
		if len(outcome.Repinned) != 0 {
			t.Fatalf("repinned %d pin(s) on a sound run", len(outcome.Repinned))
		}
	}
	if events := repinEvents(t, conn, run.ID); len(events) != 0 {
		t.Errorf("%d run-repinned event(s) recorded by a no-op", len(events))
	}
}

// TestRepinThenRepinIsIdempotent: after a real repin, a second call finds the
// run sound and changes nothing further.
func TestRepinThenRepinIsIdempotent(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/h.md", "V1\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/h.md"), []byte("V2\n"), 0o644), "rewrite")

	first, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	testsupport.Must(t, err, "first repin: %v", err)
	if len(first.Repinned) != 1 {
		t.Fatalf("first repin moved %d pin(s), want 1", len(first.Repinned))
	}
	second, err := repinRunIn(conn, run.ID, "install", nowMS, []string{root})
	testsupport.Must(t, err, "second repin: %v", err)
	if len(second.Repinned) != 0 {
		t.Fatalf("second repin moved %d pin(s), want 0", len(second.Repinned))
	}
	if events := repinEvents(t, conn, run.ID); len(events) != 1 {
		t.Errorf("%d run-repinned event(s), want exactly the first repin's 1", len(events))
	}
}

// TestDispatchOpenNamesPinDrift: the manifest a conductor reads before
// spending a wave carries the run's unsound pins beside stale_targets
// (DKT-408, the surfacing point issue comment 1 asked about). RUN-14's 3.5M
// tokens were dispatched against a drifted pin set that only render-time
// CONFLICTs reported, step by step, after the spend.
//
// This test goes through the REAL root resolution — DOCKET_PATH, not the
// injected-roots seam — because the advisory rides a verb that takes no roots
// parameter. The temp dirs come first, before t.Setenv (the autoregister
// tests' ordering note: t.TempDir reads TMPDIR).
func TestDispatchOpenNamesPinDrift(t *testing.T) {
	store := t.TempDir()
	configRoot := filepath.Join(store, "config")
	testsupport.Must(t, os.MkdirAll(filepath.Join(configRoot, "contracts"), 0o755), "mkdir")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configRoot, "contracts", "x.md"), []byte("OLD\n"), 0o644), "write")
	t.Setenv("DOCKET_PATH", store)

	conn := mustDB(t)
	// Activation scans the config root and pins contracts/x.md itself — the
	// same path a real run's pin takes.
	run, _ := activatedRun(t, conn)
	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Fatalf("the fixture run is unsound before the install: %s",
			PinReportReason(report))
	}

	// The corpus install, then the next wave's dispatch open.
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configRoot, "contracts", "x.md"), []byte("NEW\n"), 0o644), "rewrite")
	m := openDispatch(t, conn, run.ID, 0, nowMS)

	found := false
	for _, v := range m.PinDrift {
		if v.Ref == "contracts/x.md" && v.Status == PinChanged {
			found = true
		}
		if v.Status == PinOK {
			t.Errorf("the manifest advisory lists a sound pin: %+v", v)
		}
	}
	if !found {
		t.Errorf("manifest pin_drift = %+v; the drifted contract is not named "+
			"where the conductor decides to spend the wave", m.PinDrift)
	}
}

// TestPinDriftNamesOnlyUnsoundPins pins the surfacing helper the read verbs
// share: sound pins are omitted, drifted and missing ones are named, and a
// sound run yields nil so callers gate on emptiness.
func TestPinDriftNamesOnlyUnsoundPins(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/sound.md", "OK\n")
	pinAFile(t, conn, run.ID, root, "contracts/drifted.md", "OLD\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/drifted.md"), []byte("NEW\n"), 0o644), "rewrite")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)
	var drift []PinVerdict
	for _, v := range report.Pins {
		if v.Status != PinOK {
			drift = append(drift, v)
		}
	}

	found := false
	for _, v := range drift {
		if v.Ref == "contracts/sound.md" {
			t.Errorf("the sound pin is in the drift set: %+v", v)
		}
		if v.Ref == "contracts/drifted.md" && v.Status == PinChanged {
			found = true
		}
	}
	if !found {
		t.Errorf("the drifted pin is not in the drift set: %+v", drift)
	}

	notice := PinDriftNotice(drift, "RUN-1")
	for _, want := range []string{"contracts/drifted.md", "run repin RUN-1"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
	if PinDriftNotice(nil, "RUN-1") != "" {
		t.Error("a sound run renders a drift notice")
	}
}
