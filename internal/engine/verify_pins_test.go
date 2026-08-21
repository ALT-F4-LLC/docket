package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-297: no verb answered "is this run's pin state sound". `step render`
// checks only the pins its own step reads, so it returned exit 0 and a full
// packet while a contract another step depended on had already drifted — and
// an hour later every step that DID read it was unclaimable.

// pinAFile records a file pin on a run, exactly as activation does.
func pinAFile(t *testing.T, conn *sql.DB, runID int, root, ref, body string) string {
	t.Helper()
	path := filepath.Join(root, ref)
	testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	testsupport.Must(t, os.WriteFile(path, []byte(body), 0o644), "writing the fixture")

	hash := workflow.SHA256([]byte(body))
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertPinTx(tx, db.Pin{
		RunID: runID, Kind: db.PinKindFile, Ref: ref, SHA256: hash,
	}), "InsertPinTx")
	testsupport.Must(t, tx.Commit(), "Commit")
	return hash
}

// TestVerifyPinsReportsDriftAcrossTheWholeRun is the verb's whole point: it
// finds a changed pin whether or not any particular step reads it.
func TestVerifyPinsReportsDriftAcrossTheWholeRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	steady := pinAFile(t, conn, run.ID, root, "contracts/implement.md", "STEADY\n")
	drifted := pinAFile(t, conn, run.ID, root, "contracts/synthesize-findings.md", "BEFORE\n")

	// The `just activate` that replaced the installed copy.
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/synthesize-findings.md"), []byte("AFTER\n"), 0o644),
		"rewriting the contract")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)

	if report.Sound() {
		t.Fatal("the report reads sound with a contract already replaced on disk")
	}
	if report.Changed != 1 {
		t.Errorf("changed = %d, want 1", report.Changed)
	}

	byRef := map[string]PinVerdict{}
	for _, v := range report.Pins {
		byRef[v.Ref] = v
	}
	if got := byRef["contracts/implement.md"]; got.Status != PinOK || got.Found != steady {
		t.Errorf("the unchanged contract reads %+v, want ok at %s", got, steady)
	}
	drift := byRef["contracts/synthesize-findings.md"]
	if drift.Status != PinChanged {
		t.Errorf("the replaced contract reads %q, want %q", drift.Status, PinChanged)
	}
	if drift.Pinned != drifted || drift.Found == drifted {
		t.Errorf("verdict = %+v, want the pinned hash %s beside a different one "+
			"on disk — an operator needs both to choose between restoring the "+
			"file and starting a new run", drift, drifted)
	}
	if drift.Path == "" {
		t.Error("the verdict names no path; an operator restoring the file " +
			"should not have to guess which config root won")
	}
}

// TestVerifyPinsReportsAMissingPin separates "changed" from "gone": they have
// different remedies and different exit codes.
func TestVerifyPinsReportsAMissingPin(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/gone.md", "BODY\n")
	testsupport.Must(t, os.Remove(filepath.Join(root, "contracts/gone.md")),
		"removing the contract")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)

	if report.Missing != 1 || report.Changed != 0 {
		t.Errorf("missing = %d, changed = %d, want 1 and 0",
			report.Missing, report.Changed)
	}
	if report.Sound() {
		t.Error("a run missing a file it depends on reads sound")
	}
}

// TestVerifyPinsIsSoundOnAnUntouchedRun is the lower bound: the verb must not
// cry drift over a run nobody touched, or nobody will believe it when it does.
func TestVerifyPinsIsSoundOnAnUntouchedRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/a.md", "A\n")
	pinAFile(t, conn, run.ID, root, "contracts/b.md", "B\n")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)
	if !report.Sound() {
		t.Errorf("an untouched run reads unsound: %s", PinReportReason(report))
	}
	if len(report.Pins) < 2 {
		t.Errorf("checked %d pins, want at least the two file pins", len(report.Pins))
	}
}

// TestVerifyPinsWritesNothing pins the read-only contract. A verb that re-pinned
// on drift would silently rewrite the agreement instead of reporting it broke —
// which is the failure `step render`'s own "never a silent re-pin" rule names.
func TestVerifyPinsWritesNothing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()
	pinned := pinAFile(t, conn, run.ID, root, "contracts/a.md", "A\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/a.md"), []byte("EDITED\n"), 0o644),
		"editing the contract")

	for range 3 {
		if _, err := verifyPinsIn(conn, run.ID, []string{root}); err != nil {
			t.Fatalf("verifyPinsIn: %v", err)
		}
	}

	var stored string
	err := conn.QueryRow(
		`SELECT sha256 FROM pins WHERE run_id = ? AND ref = ?`,
		run.ID, "contracts/a.md").Scan(&stored)
	testsupport.Must(t, err, "reading the pin back: %v", err)
	if stored != pinned {
		t.Errorf("the pin was rewritten to %s; it must still record %s",
			stored, pinned)
	}
}

// TestVerifyPinsIsDeterministic keeps the report golden-stable: two checks of
// one unchanged run produce the same rows in the same order.
func TestVerifyPinsIsDeterministic(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()
	for _, ref := range []string{"c/z.md", "c/a.md", "c/m.md"} {
		pinAFile(t, conn, run.ID, root, ref, ref+"\n")
	}

	var first []PinVerdict
	for range 8 {
		report, err := verifyPinsIn(conn, run.ID, []string{root})
		testsupport.Must(t, err, "verifyPinsIn: %v", err)
		if first == nil {
			first = report.Pins
			continue
		}
		if len(report.Pins) != len(first) {
			t.Fatalf("pin count moved %d -> %d", len(first), len(report.Pins))
		}
		for i := range first {
			if report.Pins[i] != first[i] {
				t.Fatalf("row %d moved: %+v -> %+v", i, first[i], report.Pins[i])
			}
		}
	}
}
