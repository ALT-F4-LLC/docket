package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-582: a pinned ref that no longer RESOLVES had exactly one disposition —
// refuse the whole repin — and corpus commits that delete a contract are as
// ordinary as commits that edit one. RUN-42 died on cc92e38/93ed1e9 deleting
// `contracts/test-infra.md` and `contracts/pr-comment-author.md`; RUN-36 died
// the same way after surviving two earlier repins. Neither had a step left that
// would ever open either file.
//
// These tests pin the new disposition and its guard: a NOT_FOUND file pin no
// NON-TERMINAL step's packet closure reaches can be retired on the operator's
// say-so, and one a pending step still reads cannot be, however it is asked for.

// dropWorkflowSrc declares one packet file per step, so "referenced by a
// pending step" and "referenced only by a completed step" are two different
// files rather than two readings of one.
const dropWorkflowSrc = `
[pipeline]
name = "drop-dev"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
after = []
packet = ["contracts/implement.md"]

[[step]]
name = "review"
after = ["implement"]
executor = "review"
emits = "findings"
inputs = ["implement.change-summary"]
packet = ["contracts/review.md"]
`

// dropRun activates drop-dev and completes `implement`, leaving `review`
// pending. From here `contracts/implement.md` is read only by history and
// `contracts/review.md` only by work still to come.
func dropRun(t *testing.T) (conn *sql.DB, configDir string, runID int) {
	t.Helper()
	conn, configDir = configRepo(t)
	writeConfigFile(t, configDir, "workflows/drop-dev.toml", dropWorkflowSrc)
	writeConfigFile(t, configDir, "contracts/implement.md", "the implement contract\n")
	writeConfigFile(t, configDir, "contracts/review.md", "the review contract\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "drop subject", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// implement records, with the artifact review consumes — the completed
	// provenance that must survive every drop below.
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

	return conn, configDir, run.ID
}

// pinCount counts a run's pin rows naming a ref — 0 after a drop, 1 otherwise.
func pinCount(t *testing.T, conn *sql.DB, runID int, ref string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM pins WHERE run_id = ? AND ref = ?`, runID, ref).Scan(&n)
	testsupport.Must(t, err, "counting pin %s: %v", ref, err)
	return n
}

// deleteConfigFile is the corpus commit these tests are about.
func deleteConfigFile(t *testing.T, configDir, rel string) {
	t.Helper()
	testsupport.Must(t, os.Remove(filepath.Join(configDir, rel)), "deleting %s", rel)
}

// TestRepinDropsADeletedRefNoPendingStepReads is the acceptance criterion
// verbatim: a run whose only drifted pins are deleted refs unreferenced by
// pending steps can be repinned and resumed. Both spellings of the ask —
// --drop-unresolvable and an explicit --drop — reach it.
func TestRepinDropsADeletedRefNoPendingStepReads(t *testing.T) {
	cases := []struct {
		name string
		opts RepinOptions
	}{
		{"--drop-unresolvable", RepinOptions{
			Reason: "cc92e38 deleted it", DropUnresolvable: true}},
		{"--drop by name", RepinOptions{
			Reason: "cc92e38 deleted it", Drop: []string{"contracts/implement.md"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, configDir, runID := dropRun(t)
			pinned := pinSHA(t, conn, runID, "contracts/implement.md")

			// The corpus commit: the completed step's contract is deleted. It is
			// the run's ONLY drift, and no pending step's packet names it.
			deleteConfigFile(t, configDir, "contracts/implement.md")
			before, err := VerifyPins(conn, runID)
			testsupport.Must(t, err, "VerifyPins: %v", err)
			if before.Missing != 1 || before.Changed != 0 {
				t.Fatalf("premise: want exactly one missing pin and no changed one, "+
					"got %d missing / %d changed: %s",
					before.Missing, before.Changed, PinReportReason(before))
			}

			outcome, err := RepinRunWith(conn, runID, tc.opts, nowMS)
			testsupport.Must(t, err, "RepinRunWith: %v", err)

			if len(outcome.Repinned) != 0 {
				t.Errorf("repinned %+v; a deleted ref has no bytes to adopt",
					outcome.Repinned)
			}
			if len(outcome.Dropped) != 1 {
				t.Fatalf("dropped %d pin(s), want 1: %+v", len(outcome.Dropped),
					outcome.Dropped)
			}
			d := outcome.Dropped[0]
			if d.Ref != "contracts/implement.md" || d.OldSHA256 != pinned ||
				d.NewSHA256 != "" || !d.Dropped {
				t.Errorf("drop = %+v, want contracts/implement.md %s -> (nothing), "+
					"marked dropped", d, pinned)
			}

			// THE PIN IS RETIRED, not carried as perpetual drift: the row is gone,
			// so verify-pins has nothing left to fail on and the run is sound.
			if n := pinCount(t, conn, runID, "contracts/implement.md"); n != 0 {
				t.Errorf("%d pin row(s) still name the dropped ref; verify-pins would "+
					"report it forever", n)
			}
			after, err := VerifyPins(conn, runID)
			testsupport.Must(t, err, "VerifyPins after the drop: %v", err)
			if !after.Sound() {
				t.Errorf("the run is still unsound after the drop: %s",
					PinReportReason(after))
			}

			// THE OLD SHA SURVIVES IN THE TRAIL, with a NULL new_sha256 saying
			// there are no current bytes rather than naming some.
			events := repinEvents(t, conn, runID)
			if len(events) != 1 {
				t.Fatalf("recorded %d run-repinned event(s), want 1", len(events))
			}
			e := events[0]
			if e["ref"] != "contracts/implement.md" || e["old_sha256"] != pinned {
				t.Errorf("event = %v, want contracts/implement.md at %s", e, pinned)
			}
			if e["new_sha256"] != nil {
				t.Errorf("event new_sha256 = %v, want null — a drop has no new bytes",
					e["new_sha256"])
			}
			if e["dropped"] != true {
				t.Errorf("event dropped = %v, want true; a reader must not have to "+
					"infer the disposition from an absence", e["dropped"])
			}
			if e["reason"] != tc.opts.Reason {
				t.Errorf("event reason = %v, want the operator's --reason verbatim",
					e["reason"])
			}

			// AND THE RUN RESUMES. The pending step's packet still resolves, and
			// the next wave's manifest offers it with no drift advisory.
			steps, err := db.ListRunSteps(conn, runID)
			testsupport.Must(t, err, "ListRunSteps: %v", err)
			defs, err := StepDefinitions(conn, runID)
			testsupport.Must(t, err, "StepDefinitions: %v", err)
			for _, s := range steps {
				if s.Instance != "review@0" {
					continue
				}
				files, ferr := stepPacketFiles(conn, s, stepSpec(defs, s, holdTally{}), "")
				testsupport.Must(t, ferr, "rendering review@0's packet: %v", ferr)
				if len(files) != 1 || files[0].Path != "contracts/review.md" {
					t.Errorf("review@0's packet = %+v, want its own contract", files)
				}
			}
			m := openDispatch(t, conn, runID, 0, nowMS)
			if len(m.PinDrift) != 0 {
				t.Errorf("the manifest still advises drift after the drop: %+v",
					m.PinDrift)
			}
			offered := false
			for _, r := range m.Rows {
				if r.Instance == "review@0" {
					offered = true
				}
			}
			if !offered {
				t.Errorf("the next wave offers %+v; the pending step must be "+
					"claimable again", m.Rows)
			}
		})
	}
}

// TestRepinRefusesToDropARefAPendingStepReads is the guard: dropping is opt-in,
// and opting in never buys the removal of something still needed. Both flag
// spellings refuse, and the refusal names the step.
func TestRepinRefusesToDropARefAPendingStepReads(t *testing.T) {
	cases := []struct {
		name string
		opts RepinOptions
	}{
		{"--drop-unresolvable", RepinOptions{
			Reason: "the install", DropUnresolvable: true}},
		{"--drop by name", RepinOptions{
			Reason: "the install", Drop: []string{"contracts/review.md"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, configDir, runID := dropRun(t)
			pinned := pinSHA(t, conn, runID, "contracts/review.md")

			// The deletion that WOULD wedge the run: review@0 is pending and its
			// packet declares exactly this file.
			deleteConfigFile(t, configDir, "contracts/review.md")

			_, err := RepinRunWith(conn, runID, tc.opts, nowMS)
			if err == nil {
				t.Fatal("the repin dropped a ref a pending step still reads")
			}
			if code, ok := CodeOf(err); !ok || code != CodeConflict {
				t.Errorf("error code = %v, want CONFLICT: %v", code, err)
			}
			for _, want := range []string{"contracts/review.md", "review@0"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q", err, want)
				}
			}
			if got := pinSHA(t, conn, runID, "contracts/review.md"); got != pinned {
				t.Errorf("the pin moved to %q under a refusal; nothing may change", got)
			}
			if events := repinEvents(t, conn, runID); len(events) != 0 {
				t.Errorf("%d run-repinned event(s) recorded by a refused repin",
					len(events))
			}
		})
	}
}

// TestRepinDropsAndUpdatesInOneCall is the mixed case: one drifted ref resolves
// to new bytes and one no longer resolves at all. The first is adopted, the
// second retired, and the run comes out sound — a NOT_FOUND ref must not cost
// the changed ones their recovery.
func TestRepinDropsAndUpdatesInOneCall(t *testing.T) {
	conn, configDir, runID := dropRun(t)
	deletedPin := pinSHA(t, conn, runID, "contracts/implement.md")
	changedPin := pinSHA(t, conn, runID, "contracts/review.md")
	policyPin := pinSHA(t, conn, runID, policyPinRef)

	deleteConfigFile(t, configDir, "contracts/implement.md")
	rewriteConfigFile(t, configDir, "contracts/review.md", "the install's new contract\n")

	outcome, err := RepinRunWith(conn, runID, RepinOptions{
		Reason: "corpus install: one edit, one deletion", DropUnresolvable: true,
	}, nowMS)
	testsupport.Must(t, err, "RepinRunWith: %v", err)

	if len(outcome.Repinned) != 1 || outcome.Repinned[0].Ref != "contracts/review.md" {
		t.Fatalf("repinned %+v, want the one ref that resolves to new bytes",
			outcome.Repinned)
	}
	if outcome.Repinned[0].OldSHA256 != changedPin ||
		outcome.Repinned[0].NewSHA256 == changedPin {
		t.Errorf("the changed ref's pin did not move: %+v", outcome.Repinned[0])
	}
	if len(outcome.Dropped) != 1 || outcome.Dropped[0].Ref != "contracts/implement.md" {
		t.Fatalf("dropped %+v, want the one deleted ref", outcome.Dropped)
	}
	if outcome.Dropped[0].OldSHA256 != deletedPin {
		t.Errorf("the drop event's old sha is %q, want %q — the completed step's "+
			"agreement rides on it", outcome.Dropped[0].OldSHA256, deletedPin)
	}

	// The sound pin was not touched by either disposition.
	if got := pinSHA(t, conn, runID, policyPinRef); got != policyPin {
		t.Errorf("the sound pin moved %s -> %s", policyPin, got)
	}
	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Errorf("the run is still unsound after the mixed repin: %s",
			PinReportReason(report))
	}

	// One event per ref, each self-contained, and the two dispositions are
	// distinguishable in the trail.
	events := repinEvents(t, conn, runID)
	if len(events) != 2 {
		t.Fatalf("recorded %d run-repinned event(s), want one per ref", len(events))
	}
	seen := map[string]any{}
	for _, e := range events {
		ref, _ := e["ref"].(string)
		seen[ref] = e["new_sha256"]
	}
	if seen["contracts/implement.md"] != nil {
		t.Errorf("the dropped ref's event carries new_sha256 %v, want null",
			seen["contracts/implement.md"])
	}
	if seen["contracts/review.md"] == nil {
		t.Errorf("the updated ref's event carries a null new_sha256; only a drop does")
	}
}

// TestRepinRefusesADropThatDescribesTheWrongThing: --drop is a claim about the
// run's state, and a claim the run contradicts is a refusal rather than a
// silent no-op — the operator and the run disagree about what is broken.
func TestRepinRefusesADropThatDescribesTheWrongThing(t *testing.T) {
	t.Run("the named ref still resolves", func(t *testing.T) {
		conn, configDir, runID := dropRun(t)
		rewriteConfigFile(t, configDir, "contracts/review.md", "edited, not deleted\n")

		_, err := RepinRunWith(conn, runID, RepinOptions{
			Reason: "r", Drop: []string{"contracts/review.md"}}, nowMS)
		if err == nil {
			t.Fatal("--drop retired a ref that still resolves")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
		if pinCount(t, conn, runID, "contracts/review.md") != 1 {
			t.Error("the pin was retired under a refusal")
		}
	})

	t.Run("the named ref is there but unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a 0000 file, so the unreadable case cannot be staged")
		}
		conn, configDir, runID := dropRun(t)
		path := filepath.Join(configDir, "contracts/implement.md")
		testsupport.Must(t, os.Chmod(path, 0o000), "chmod")
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

		_, err := RepinRunWith(conn, runID, RepinOptions{
			Reason: "r", DropUnresolvable: true}, nowMS)
		if err == nil {
			t.Fatal("a file that exists but cannot be read was retired as deleted")
		}
		if code, ok := CodeOf(err); !ok || code != CodeConflict {
			t.Errorf("error code = %v, want CONFLICT: %v", code, err)
		}
		if pinCount(t, conn, runID, "contracts/implement.md") != 1 {
			t.Error("the pin was retired for a file still sitting on disk")
		}
	})

	t.Run("the named ref is not pinned at all", func(t *testing.T) {
		conn, configDir, runID := dropRun(t)
		deleteConfigFile(t, configDir, "contracts/implement.md")

		_, err := RepinRunWith(conn, runID, RepinOptions{
			Reason: "r", Drop: []string{"contracts/never-pinned.md"}}, nowMS)
		if err == nil {
			t.Fatal("--drop accepted a ref this run does not pin")
		}
		if code, ok := CodeOf(err); !ok || code != CodeNotFound {
			t.Errorf("error code = %v, want NOT_FOUND: %v", code, err)
		}
		if !strings.Contains(err.Error(), "contracts/never-pinned.md") {
			t.Errorf("refusal %q does not name the ref", err)
		}
	})
}

// TestRepinNeverDropsARegisteredObjectPin: --drop-unresolvable is about FILES.
// A workflow or schema pin names a registered object every step is expanded
// from or validated against, and no packet closure can call one unread — so a
// missing one still refuses, and naming it explicitly is a validation error
// rather than a wider reading of the flag.
func TestRepinNeverDropsARegisteredObjectPin(t *testing.T) {
	conn, _, runID := dropRun(t)

	// A schema pin whose registry row does not exist: verify-pins reports it
	// missing, exactly as a de-registered schema would.
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertPinTx(tx, db.Pin{
		RunID: runID, Kind: db.PinKindSchema, Ref: "gone@1", SHA256: "deadbeef",
	}), "InsertPinTx")
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	_, err = RepinRunWith(conn, runID, RepinOptions{
		Reason: "r", DropUnresolvable: true}, nowMS)
	if err == nil {
		t.Fatal("--drop-unresolvable retired a schema pin")
	}
	if code, ok := CodeOf(err); !ok || code != CodeNotFound {
		t.Errorf("error code = %v, want NOT_FOUND: %v", code, err)
	}
	if !strings.Contains(err.Error(), "gone@1") {
		t.Errorf("refusal %q does not name the missing schema", err)
	}

	_, err = RepinRunWith(conn, runID, RepinOptions{
		Reason: "r", Drop: []string{"gone@1"}}, nowMS)
	if err == nil {
		t.Fatal("--drop retired a schema pin")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("error code = %v, want VALIDATION_ERROR: %v", code, err)
	}
	if pinCount(t, conn, runID, "gone@1") != 1 {
		t.Error("the schema pin was retired under a refusal")
	}
}

// TestPendingClosureCoversIncludesAndSpares completed-only refs: the walk that
// decides a drop must follow `packet_includes` from a PENDING step (or the
// fragment under a still-needed contract would look unread), and must not
// follow them from a terminal one (or nothing would ever be droppable).
func TestPendingClosureCoversIncludesAndSparesCompletedOnlyRefs(t *testing.T) {
	conn, configDir, runID := dropRun(t)
	rewriteConfigFile(t, configDir, "contracts/review.md",
		"---\npacket_includes:\n  - fragments/style.md\n---\nthe review contract\n")
	writeConfigFile(t, configDir, "fragments/style.md", "the included fragment\n")

	closure, err := pendingPacketClosure(conn, runID, instanceConfigRoots())
	testsupport.Must(t, err, "pendingPacketClosure: %v", err)

	if by := closure.referencedBy("contracts/review.md"); len(by) == 0 ||
		by[0] != "review@0" {
		t.Errorf("contracts/review.md is reached by %v, want review@0", by)
	}
	if by := closure.referencedBy("fragments/style.md"); len(by) == 0 {
		t.Error("a fragment reachable only through a pending step's " +
			"packet_includes reads as unreferenced; dropping it would wedge the render")
	}
	if by := closure.referencedBy("contracts/implement.md"); len(by) != 0 {
		t.Errorf("contracts/implement.md is reached by %v, but only a terminal "+
			"step declares it", by)
	}
	// policy.toml is the harness's, not any step's, and no step-sourced walk
	// could find it — so it is never droppable.
	if by := closure.referencedBy(policyPinRef); len(by) == 0 {
		t.Error("policy.toml reads as unreferenced; the harness resolves policy from it")
	}
}
