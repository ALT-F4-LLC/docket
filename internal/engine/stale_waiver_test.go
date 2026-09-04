package engine

import (
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
		"cafe1234cafe", "the divergence is the later format pass", nowMS)
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
		"beefbeefbeef", "ruled on some other target", nowMS)
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
		"cafe1234cafe1234", "seats judge the integrated successor instead", nowMS)
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
		if _, err := e.WaiveStaleTargets(conn, run.ID, c.instances, c.sha, "", nowMS); err == nil {
			t.Errorf("%s: the waiver was recorded", c.name)
		}
	}

	if _, err := e.WaiveStaleTargets(conn, 999999,
		[]string{"review@0#0"}, "cafe1234cafe", "", nowMS); err == nil {
		t.Error("a waiver was recorded against a run that does not exist")
	}
}
