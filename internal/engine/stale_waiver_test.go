package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-742, both halves.
//
// HALF ONE — detection completeness. IsAncestorFn's `known = false` conflated
// "git could not answer" with "the object is not in the shared store at all",
// and staleTargets skipped both silently. RUN-52's DKT-V253 was the second
// state: a three-seat vote panel each ran `git cat-file -t` on the packet's
// target, found no object anywhere, and no warning had fired. The absence
// probe (ObjectExistsFn) closes exactly that gap, and ONLY that gap: a
// genuinely unanswerable existence question stays as silent as it ever was.
//
// HALF TWO — waiver memory. A stale-target warning an operator investigated
// and ruled acceptable re-fired unchanged at every subsequent dispatch
// open/verify of the same (step, target) pair — four times in RUN-52 — with
// the standing ruling living only in session memory. A run-scoped waiver
// (`dispatch waive-target`) makes it engine-visible; the signature is the
// pair alone, so a different sha or an unnamed row still warns.

// TestDispatchWarnsWhenTargetObjectIsAbsent: ancestry unanswerable, existence
// DEFINITIVELY no — the DKT-V253 shape. Every consuming row warns, marked
// `absent`, with the reason naming the cat-file probe rather than a
// divergence nothing measured.
func TestDispatchWarnsWhenTargetObjectIsAbsent(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, false }
	e.ObjectExistsFn = func(_, sha string) (exists, known bool) {
		if sha != "cafe1234cafe1234" {
			t.Errorf("existence asked about %q, want the recorded head", sha)
		}
		return false, true
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("stale targets = %d, want the four review siblings — an absent "+
			"object must warn, not be skipped as unanswerable: %+v",
			len(m.StaleTargets), m.StaleTargets)
	}
	for _, s := range m.StaleTargets {
		if !s.Absent {
			t.Errorf("%s is not marked absent: %+v", s.Instance, s)
		}
		if !strings.Contains(s.Reason, "does not resolve as a commit") ||
			!strings.Contains(s.Reason, "cat-file") {
			t.Errorf("%s reason %q does not name the absence or the probe",
				s.Instance, s.Reason)
		}
		// The divergence wording must NOT appear: nothing measured a
		// divergence, and the two advisory shapes may not blur (DKT-415's
		// discipline applied to the third shape).
		if strings.Contains(s.Reason, "not an ancestor") {
			t.Errorf("%s reason %q claims an ancestry fact that was unanswerable",
				s.Instance, s.Reason)
		}
		// DKT-415: the claim-time semantics still ride every shape.
		if !strings.Contains(s.Reason, "does not re-derive the target from HEAD") ||
			!strings.Contains(s.Reason, "resolves at claim time") {
			t.Errorf("%s reason %q dropped the claim-time semantics",
				s.Instance, s.Reason)
		}
	}
}

// An object that EXISTS while ancestry is unanswerable stays silent: the
// probe accuses on definitive absence only, never on the ancestry question it
// could not answer.
func TestDispatchStaysQuietWhenObjectExistsButAncestryUnanswerable(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, false }
	e.ObjectExistsFn = func(_, _ string) (exists, known bool) { return true, true }

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("a present object with unanswerable ancestry was flagged: %+v",
			m.StaleTargets)
	}
}

// An engine with no existence probe wired keeps the pre-DKT-742 behavior
// exactly: unanswerable ancestry stays silent.
func TestMissingExistenceProbeKeepsTheUnansweredCaseSilent(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, false }
	e.ObjectExistsFn = nil

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("an unanswerable question warned with no probe wired: %+v",
			m.StaleTargets)
	}
}

// TestGitCommitResolvable drives the real implementation across its
// three-valued contract: present commit, definitively absent object, an
// object that exists but is not a commit, and the two unanswerable shapes.
func TestGitCommitResolvable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "a.txt", "content\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	commit := gitRun(t, repo, "rev-parse", "HEAD")
	blob := gitRun(t, repo, "rev-parse", "HEAD:a.txt")

	cases := []struct {
		name          string
		execRoot, sha string
		exists, known bool
	}{
		{"a present commit", repo, commit, true, true},
		{"an absent object", repo, "0123456789abcdef0123456789abcdef01234567", false, true},
		{"a blob, not a commit", repo, blob, false, true},
		{"no repository", t.TempDir(), commit, false, false},
		{"empty inputs", "", "", false, false},
	}
	for _, c := range cases {
		exists, known := gitCommitResolvable(c.execRoot, c.sha)
		if exists != c.exists || known != c.known {
			t.Errorf("%s: = (%v, %v), want (%v, %v)",
				c.name, exists, known, c.exists, c.known)
		}
	}
}

// TestDispatchWarnsAbsentTargetRecordedOutsideTheSharedStore is DKT-V253's
// shape end to end with real git: the executor commits in a checkout whose
// object store the shared checkout does NOT share (a separate clone — the
// same absence a pruned-then-GC'd linked worktree leaves), the step records
// that head, and dispatch open must warn that the target resolves from
// nowhere the consumers can reach — the case that previously produced NO
// warning at all.
func TestDispatchWarnsAbsentTargetRecordedOutsideTheSharedStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	shared := t.TempDir()
	gitRun(t, shared, "init", "-q")
	writeFile(t, shared, "internal/work.txt", "original\n")
	gitRun(t, shared, "add", "-A")
	gitRun(t, shared, "commit", "-q", "-m", "base")

	// The executor's checkout: a clone, so its new commit's object never
	// enters the shared store.
	clone := t.TempDir()
	gitRun(t, clone, "clone", "-q", shared, ".")
	writeFile(t, clone, "internal/work.txt", "the executor's change\n")
	gitRun(t, clone, "add", "-A")
	gitRun(t, clone, "commit", "-q", "-m", "implement the issue")
	target := gitRun(t, clone, "rev-parse", "HEAD")

	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, shared, run.ID)
	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		WorkDir:  clone,
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	// THE PREMISE, ASSERTED: the recorded target really is absent from the
	// shared store (the acceptance criterion's own probe), and ancestry really
	// is unanswerable — the exact state that used to skip silently.
	if exists, known := gitCommitResolvable(shared, target); exists || !known {
		t.Fatalf("premise broken: existence = (%v, %v), want a definitive NO — "+
			"the clone's commit must not be in the shared store", exists, known)
	}
	if _, known := gitAncestorOfHead(shared, target); known {
		t.Fatal("premise broken: ancestry answered about an absent object, so " +
			"this case no longer covers the silent-skip gap")
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("stale targets = %d, want the four review siblings — the "+
			"absent-object case fired no warning: %+v",
			len(m.StaleTargets), m.StaleTargets)
	}
	for _, s := range m.StaleTargets {
		if !s.Absent || s.TargetSHA != target {
			t.Errorf("%s: absent=%v target=%q, want absent with the recorded head %q",
				s.Instance, s.Absent, s.TargetSHA, target)
		}
	}
}

// TestWaiverSuppressesAdjudicatedStaleTarget: the AC's companion half. A
// warning acknowledged once for a (step, target) pair does not re-fire
// unchanged on subsequent open/verify of the same pair — and the waiver's
// sha may be the 12-character prefix the warning itself renders.
func TestWaiverSuppressesAdjudicatedStaleTarget(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)
	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, true }

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("premise: stale targets = %d, want 4", len(m.StaleTargets))
	}

	// The operator adjudicates three of the four rows, copying the sha at the
	// advisory's own 12-character rendering.
	waived, err := e.WaiveStaleTargets(conn, run.ID,
		[]string{"review@0#0", "review@0#1", "review@0#2"},
		"cafe1234cafe", "the divergence is the later format pass", testBy, testConductorToken, nowMS)
	testsupport.Must(t, err, "waive: %v", err)
	if len(waived) != 3 {
		t.Fatalf("waivers minted = %d, want 3: %+v", len(waived), waived)
	}

	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify mismatch: %+v", mismatch)
	}
	if len(result.StaleTargets) != 1 || result.StaleTargets[0].Instance != "review@0#3" {
		t.Fatalf("post-waiver stale targets = %+v, want exactly the unwaived "+
			"review@0#3", result.StaleTargets)
	}

	// The waivers are event-logged: standing precedent must be findable in
	// the feed, or a warning that stopped appearing is indistinguishable from
	// a warning that stopped being true.
	var events int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = 'stale-target-waived'`,
		run.ID).Scan(&events)
	testsupport.Must(t, err, "counting waiver events: %v", err)
	if events != 3 {
		t.Errorf("stale-target-waived events = %d, want one per waiver", events)
	}
}

// TestWaiverDoesNotCoverADifferentSignature: a different sha on the waived
// row, and the waived sha on an unnamed row, both still warn — a new
// divergence never rides an old ruling.
func TestWaiverDoesNotCoverADifferentSignature(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)
	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, true }

	// A waiver for a DIFFERENT sha on every row: nothing may be suppressed.
	_, err := e.WaiveStaleTargets(conn, run.ID,
		[]string{"review@0#0", "review@0#1", "review@0#2", "review@0#3"},
		"beefbeefbeef", "ruled on some other target", testBy, testConductorToken, nowMS)
	testsupport.Must(t, err, "waive: %v", err)

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Errorf("stale targets = %d, want all 4 — a waiver for another sha "+
			"suppressed a warning it never ruled on: %+v",
			len(m.StaleTargets), m.StaleTargets)
	}
}

// A waiver covers the ABSENT advisory shape too: the adjudication is about
// the (step, target) pair, whichever reason the pair warned with.
func TestWaiverSuppressesAbsentTargetWarning(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)
	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, false }
	e.ObjectExistsFn = func(_, _ string) (exists, known bool) { return false, true }

	_, err := e.WaiveStaleTargets(conn, run.ID,
		[]string{"review@0#0", "review@0#1", "review@0#2", "review@0#3"},
		"cafe1234cafe1234", "seats judge the integrated successor instead", testBy, testConductorToken, nowMS)
	testsupport.Must(t, err, "waive: %v", err)

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("a waived absent-target warning re-fired: %+v", m.StaleTargets)
	}
}

// The verb's own refusals: a sha that is not hex (or too short to be an
// unambiguous prefix), an empty instance list, and a run that does not exist.
func TestWaiveStaleTargetsRefusesBadInputs(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	for _, c := range []struct {
		name      string
		instances []string
		sha       string
	}{
		{"no instances", nil, "cafe1234cafe"},
		{"an empty instance", []string{""}, "cafe1234cafe"},
		{"a non-hex sha", []string{"review@0#0"}, "not-a-sha!!"},
		{"a too-short prefix", []string{"review@0#0"}, "cafe12"},
	} {
		if _, err := e.WaiveStaleTargets(conn, run.ID, c.instances, c.sha, "", testBy, testConductorToken, nowMS); err == nil {
			t.Errorf("%s: the waiver was recorded", c.name)
		}
	}

	if _, err := e.WaiveStaleTargets(conn, 999999,
		[]string{"review@0#0"}, "cafe1234cafe", "", testBy, testConductorToken, nowMS); err == nil {
		t.Error("a waiver was recorded against a run that does not exist")
	}
}

// TestWaiveStaleTargetsRecordsRulingAttribution is DKT-2654 criterion 1: each
// stale-target-waived event is a JSON object carrying the ruling's actor and
// cwd beside the target sha and the waiver id it minted.
func TestWaiveStaleTargetsRecordsRulingAttribution(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	by := Attribution{Actor: "the conductor", Cwd: "/work/shared"}
	waived, err := e.WaiveStaleTargets(conn, run.ID, []string{"review@0#0"},
		"cafe1234cafe", "adjudicated", by, testConductorToken, nowMS)
	testsupport.Must(t, err, "waive: %v", err)

	page, err := ListEvents(conn, EventQuery{RunID: run.ID, Kind: EventStaleTargetWaived})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(page.Events) != 1 {
		t.Fatalf("%d stale-target-waived events, want 1", len(page.Events))
	}
	var data map[string]any
	testsupport.Must(t, json.Unmarshal(page.Events[0].Data, &data), "decoding %s: %v", page.Events[0].Data, nil)
	if data["actor"] != by.Actor || data["cwd"] != by.Cwd {
		t.Errorf("actor/cwd = %v/%v, want %q/%q", data["actor"], data["cwd"], by.Actor, by.Cwd)
	}
	if data["target_sha"] != waived[0].Target || data["waiver"] != float64(waived[0].ID) {
		t.Errorf("event data %v does not match the waiver %+v", data, waived[0])
	}
}

// TestWaiveStaleTargetsRefusesEmptyAttribution is DKT-2654 criterion 2: a zero
// attribution is refused before any waiver row or event is written.
func TestWaiveStaleTargetsRefusesEmptyAttribution(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	if _, err := e.WaiveStaleTargets(conn, run.ID, []string{"review@0#0"},
		"cafe1234cafe", "adjudicated", Attribution{}, testConductorToken, nowMS); err == nil {
		t.Fatal("an unattributed waiver was recorded")
	}
	var rows int
	testsupport.Must(t, conn.QueryRow(`SELECT COUNT(*) FROM stale_target_waivers`).Scan(&rows),
		"counting waivers: %v", nil)
	page, err := ListEvents(conn, EventQuery{RunID: run.ID, Kind: EventStaleTargetWaived})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if rows != 0 || len(page.Events) != 0 {
		t.Errorf("%d waiver rows and %d events after a refusal, want 0 and 0", rows, len(page.Events))
	}
}

// waiverWrites counts what a waive-target call left behind on a run: the
// waiver rows and the stale-target-waived events.
func waiverWrites(t *testing.T, conn *sql.DB, runID int) (rows, events int) {
	t.Helper()
	testsupport.Must(t, conn.QueryRow(
		`SELECT COUNT(*) FROM stale_target_waivers WHERE run_id = ?`, runID).Scan(&rows),
		"counting waivers: %v", nil)
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: EventStaleTargetWaived})
	testsupport.Must(t, err, "ListEvents: %v", err)
	return rows, len(page.Events)
}

var waiveInstances = []string{"review@0#0", "review@0#1"}

// On a run bound to a conductor capability, a waiver presented with no token
// is refused before any row or event is written.
func TestWaiveStaleTargetsRefusesMissingToken(t *testing.T) {
	conn := mustDB(t)
	run, _ := boundRun(t, conn)

	_, err := testEngine().WaiveStaleTargets(conn, run.ID, waiveInstances,
		"cafe1234cafe", "adjudicated", testBy, "", nowMS)
	assertConductorCode(t, err, CodeValidation, "dispatch waive-target with no token")
	if rows, events := waiverWrites(t, conn, run.ID); rows != 0 || events != 0 {
		t.Errorf("%d waiver rows and %d events after a refusal, want 0 and 0", rows, events)
	}
}

// A token that is not the run's current capability, including the one a
// `run conduct` rotation retired, is an AUTH_ERROR and writes nothing.
func TestWaiveStaleTargetsRefusesWrongToken(t *testing.T) {
	conn := mustDB(t)
	run, activationToken := boundRun(t, conn)
	_, err := ConductRun(conn, run.ID, ConductOptions{By: testBy, NowMS: nowMS})
	testsupport.Must(t, err, "run conduct: %v", err)

	for name, token := range map[string]string{
		"a guessed token":   "deadbeef",
		"the retired token": activationToken,
	} {
		_, err := testEngine().WaiveStaleTargets(conn, run.ID, waiveInstances,
			"cafe1234cafe", "adjudicated", testBy, token, nowMS)
		assertConductorCode(t, err, CodeAuth, "dispatch waive-target with "+name)
		if !errors.Is(err, ErrNotConductor) {
			t.Errorf("%s: err = %v, want it to wrap ErrNotConductor", name, err)
		}
		if rows, events := waiverWrites(t, conn, run.ID); rows != 0 || events != 0 {
			t.Errorf("%s: %d waiver rows and %d events after a refusal, want 0 and 0",
				name, rows, events)
		}
	}
}

// The capability Activate returned records one waiver row and one
// stale-target-waived event per named instance, and no event carries it.
func TestWaiveStaleTargetsRecordsWithTheRunsCapability(t *testing.T) {
	conn := mustDB(t)
	run, token := boundRun(t, conn)

	waived, err := testEngine().WaiveStaleTargets(conn, run.ID, waiveInstances,
		"cafe1234cafe", "adjudicated", testBy, token, nowMS)
	testsupport.Must(t, err, "waive with the run's capability: %v", err)
	if len(waived) != len(waiveInstances) {
		t.Fatalf("waivers minted = %d, want %d: %+v", len(waived), len(waiveInstances), waived)
	}
	rows, events := waiverWrites(t, conn, run.ID)
	if rows != len(waiveInstances) || events != len(waiveInstances) {
		t.Errorf("%d waiver rows and %d events, want %d of each",
			rows, events, len(waiveInstances))
	}
	page, err := ListEvents(conn, EventQuery{RunID: run.ID, Kind: EventStaleTargetWaived})
	testsupport.Must(t, err, "ListEvents: %v", err)
	for _, ev := range page.Events {
		if strings.Contains(string(ev.Data), token) {
			t.Errorf("a stale-target-waived event carries the capability: %s", ev.Data)
		}
	}
}

// A run with no capability minted (activated before the capability existed)
// takes a waiver with no token, as every operator verb does on such a run.
func TestWaiveStaleTargetsAllowsAnUnboundRunWithoutToken(t *testing.T) {
	conn := mustDB(t)
	run, _ := boundRun(t, conn)
	unbind(t, conn, run.ID)

	waived, err := testEngine().WaiveStaleTargets(conn, run.ID, waiveInstances,
		"cafe1234cafe", "adjudicated", testBy, "", nowMS)
	testsupport.Must(t, err, "waive on an unbound run: %v", err)
	rows, events := waiverWrites(t, conn, run.ID)
	if len(waived) != 2 || rows != 2 || events != 2 {
		t.Errorf("waived %d, %d rows, %d events on an unbound run, want 2 of each",
			len(waived), rows, events)
	}
}
