package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1034 — no resolution re-records a step's issue.diff after an
// out-of-band patch, so downstream review packets bind to the pre-patch tree.
//
// RUN-67 STEP-3248 (implement@0) failed self-hygiene on its recorded commit.
// The operator ruled a conductor patch; the conductor committed on top in the
// step's worktree, integrated both, and override-passed the step. review@0's
// packets rendered from the STALE record, three judges re-found a lint defect
// the branch no longer had, and a full fix round ran to report that nothing
// needed fixing. These tests run REAL GIT, because the fixture's whole point
// is what a worktree, a patch commit, and a cherry-pick integration do to the
// object graph the diff and the target sha are computed over.

// repinGates is a GateRunner whose verdict is decided per run and which records
// the WorkRoot every spawn measured, so a rerun can prove WHERE it measured.
type repinGates struct {
	mu    sync.Mutex
	fail  bool
	roots []string
}

func (g *repinGates) Run(_ context.Context, spec GateSpec, sc StepContext) (GateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roots = append(g.roots, sc.WorkRoot)
	if g.fail {
		return GateResult{Gate: spec.Name, Exit: 1, Verdict: VerdictFail}, nil
	}
	return GateResult{Gate: spec.Name, Exit: 0, Verdict: VerdictPass}, nil
}

func (g *repinGates) measured() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.roots...)
}

// diffRepinWorld is the RUN-67 shape in real git: the run's exec root (the shared
// checkout), a linked worktree the executor worked in, and implement@0 recorded
// from that worktree and parked by a failing gate.
type diffRepinWorld struct {
	execRoot, worktree, shared string
	executorSHA                string
	implementID                int
	gates                      *repinGates
}

// diffRepinFixture builds the world up to the park — the state every re-pin starts
// from — with the REAL diff and head resolution wired, since a stub would prove
// nothing about the sha a re-pin binds.
func diffRepinFixture(t *testing.T, conn *sql.DB, e *Engine, runID int) *diffRepinWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	w := &diffRepinWorld{execRoot: t.TempDir(), gates: &repinGates{fail: true}}
	gitRun(t, w.execRoot, "init", "-q")
	writeFile(t, w.execRoot, "internal/work.txt", "original\n")
	gitRun(t, w.execRoot, "add", "-A")
	gitRun(t, w.execRoot, "commit", "-q", "-m", "base")
	w.shared = gitRun(t, w.execRoot, "rev-parse", "--abbrev-ref", "HEAD")
	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, w.execRoot, runID)

	// The executor's isolated worktree — a real `git worktree`, sharing the
	// object database exactly as production's does.
	w.worktree = filepath.Join(t.TempDir(), "wt")
	gitRun(t, w.execRoot, "worktree", "add", "-q", "-b", "executor", w.worktree)
	writeFile(t, w.worktree, "internal/work.txt",
		"the executor's change\nlint defect: a line the gate rejects\n")
	gitRun(t, w.worktree, "add", "-A")
	gitRun(t, w.worktree, "commit", "-q", "-m", "implement the issue")
	w.executorSHA = gitRun(t, w.worktree, "rev-parse", "HEAD")

	e.DiffFn = GitDiff
	e.Gates = w.gates
	w.implementID = stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, w.implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, w.implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: w.worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 = %q after a failing gate, want %q",
			got, db.StepWaitingHuman)
	}
	return w
}

// conductorPatch is the operator's ruling made real: a fix committed ON TOP of
// the executor's commit in the step's own worktree, then both integrated onto
// the shared branch the conduct protocol's way — `git cherry-pick -x`, which
// mints new shas for both (the `-x` trailer guarantees it even inside one
// second of a fixed identity, where a plain pick reproduces the executor's
// objects byte for byte and reads as a fast-forward). Returns the patch
// commit's sha in the worktree.
func conductorPatch(t *testing.T, w *diffRepinWorld) string {
	t.Helper()
	writeFile(t, w.worktree, "internal/work.txt", "the executor's change\nlint fixed\n")
	gitRun(t, w.worktree, "add", "-A")
	gitRun(t, w.worktree, "commit", "-q", "-m", "conductor patch")
	patch := gitRun(t, w.worktree, "rev-parse", "HEAD")
	gitRun(t, w.execRoot, "cherry-pick", "-x", w.executorSHA, patch)
	if got := gitRun(t, w.execRoot, "rev-parse", "HEAD"); got == patch {
		t.Fatal("premise: the integration reproduced the worktree's commit verbatim, " +
			"so the fixture would read as a fast-forward rather than a cherry-pick")
	}
	return patch
}

// issueDiffArtifacts is the step's issue.diff records, oldest first.
func issueDiffArtifacts(t *testing.T, conn *sql.DB, stepID int) []StepArtifact {
	t.Helper()
	all, err := ListStepArtifacts(conn, stepID)
	testsupport.Must(t, err, "ListStepArtifacts: %v", err)
	var out []StepArtifact
	for _, a := range all {
		if a.Kind == ArtifactKindIssueDiff {
			out = append(out, a)
		}
	}
	return out
}

// diffRepinEvents reads every issue-diff-repinned event's data.
func diffRepinEvents(t *testing.T, conn *sql.DB) []map[string]any {
	t.Helper()
	rows, err := conn.Query(`SELECT data FROM events WHERE kind = ? ORDER BY seq`, EventIssueDiffRepinned)
	testsupport.Must(t, err, "reading events: %v", err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		testsupport.Must(t, rows.Scan(&raw), "scanning event")
		var data map[string]any
		testsupport.Must(t, json.Unmarshal([]byte(raw), &data), "decoding event data %q", raw)
		out = append(out, data)
	}
	return out
}

// TestOverridePassWorktreeRepinsTheReviewedObject is the issue's acceptance
// case end to end: after `--as override-pass --worktree`, the step's newest
// issue.diff describes the patched tree, its target is the patch commit, the
// original record survives in the supersession chain, the move is
// event-logged with both shas, and every downstream review packet — the
// bundle, the render, and the dispatch manifest's own target resolution —
// binds to the patched commit with no stale_targets row.
func TestOverridePassWorktreeRepinsTheReviewedObject(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()
	w := diffRepinFixture(t, conn, e, run.ID)

	// THE PREMISE: the recorded target is the pre-patch commit, and it is
	// what every consumer would render from.
	if got := latestIssueDiffHead(conn, run.ID, issue); got != w.executorSHA {
		t.Fatalf("premise: recorded head = %q, want the executor's commit %q", got, w.executorSHA)
	}
	before := issueDiffArtifacts(t, conn, w.implementID)
	if len(before) != 1 {
		t.Fatalf("premise: implement@0 recorded %d issue.diff artifacts, want 1", len(before))
	}

	patch := conductorPatch(t, w)

	out, err := e.ResolveStepWith(conn, w.implementID, ResolveOptions{
		As: ResolveOverridePass, Worktree: w.worktree, Note: "conductor patch ruled",
		NowMS: nowMS + 1,
	})
	testsupport.Must(t, err, "resolve --as override-pass --worktree: %v", err)

	// THE OUTCOME names both shas and both artifacts.
	if out == nil || out.Repin == nil {
		t.Fatal("the resolution reported no re-pin")
	}
	r := out.Repin
	if r.FromSHA != w.executorSHA || r.ToSHA != patch {
		t.Errorf("repin = %s -> %s, want %s -> %s", r.FromSHA, r.ToSHA, w.executorSHA, patch)
	}
	if r.Unchanged {
		t.Error("the re-pin reports Unchanged over a tree that changed")
	}
	if r.Supersedes != before[0].Artifact {
		t.Errorf("repin.Supersedes = %q, want the step's previous record %s",
			r.Supersedes, before[0].Artifact)
	}
	if r.Worktree != w.worktree {
		t.Errorf("repin.Worktree = %q, want %q", r.Worktree, w.worktree)
	}
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Errorf("implement@0 = %q after override-pass, want %q", got, db.StepDone)
	}

	// THE CHAIN: the original stays readable, the new record supersedes it,
	// and the two describe different trees.
	after := issueDiffArtifacts(t, conn, w.implementID)
	if len(after) != 2 {
		t.Fatalf("implement@0 holds %d issue.diff artifacts after the re-pin, want 2: %+v",
			len(after), after)
	}
	if after[1].Artifact != r.Artifact || after[1].Supersedes != before[0].Artifact {
		t.Errorf("newest record %s supersedes %q, want %s superseding %s",
			after[1].Artifact, after[1].Supersedes, r.Artifact, before[0].Artifact)
	}
	original, err := ReadArtifact(conn, diffArtifactID(t, before[0].Artifact))
	testsupport.Must(t, err, "ReadArtifact(original): %v", err)
	if !strings.Contains(original.Body, "+lint defect") {
		t.Errorf("the original record no longer carries the pre-patch hunk:\n%s", original.Body)
	}
	if handBackHead(original.Payload) != w.executorSHA {
		t.Errorf("the original record's head moved to %q", handBackHead(original.Payload))
	}
	repinned, err := ReadArtifact(conn, diffArtifactID(t, r.Artifact))
	testsupport.Must(t, err, "ReadArtifact(repinned): %v", err)
	if !strings.Contains(repinned.Body, "+lint fixed") || strings.Contains(repinned.Body, "lint defect") {
		t.Errorf("the re-pinned record does not describe the patched tree:\n%s", repinned.Body)
	}
	if handBackHead(repinned.Payload) != patch {
		t.Errorf("the re-pinned record's head = %q, want the patch commit %q",
			handBackHead(repinned.Payload), patch)
	}

	// THE EVENT carries both shas.
	events := diffRepinEvents(t, conn)
	if len(events) != 1 {
		t.Fatalf("issue-diff-repinned events = %d, want 1", len(events))
	}
	if events[0]["from_sha"] != w.executorSHA || events[0]["to_sha"] != patch {
		t.Errorf("event = %+v, want from %s to %s", events[0], w.executorSHA, patch)
	}
	if events[0]["artifact"] != r.Artifact || events[0]["supersedes"] != before[0].Artifact {
		t.Errorf("event = %+v, want artifact %s superseding %s", events[0], r.Artifact, before[0].Artifact)
	}

	// EVERY DOWNSTREAM PACKET binds to the patched commit: the bundle, and the
	// render a judge would receive.
	for _, instance := range []string{"review@0#0", "review@0#1", "review@0#2", "review@0#3"} {
		id := stepIDByInstance(t, conn, instance)
		bundle, err := ReadContext(conn, id, nowMS+2)
		testsupport.Must(t, err, "ReadContext(%s): %v", instance, err)
		if bundle.TargetSHA != patch {
			t.Errorf("%s target_sha = %q, want the patch commit %q", instance, bundle.TargetSHA, patch)
		}
		for _, in := range bundle.Inputs {
			if in.Kind == ArtifactKindIssueDiff && !strings.Contains(in.Body, "+lint fixed") {
				t.Errorf("%s's issue.diff input still renders the stale tree:\n%s", instance, in.Body)
			}
		}
	}
	rendered, err := RenderStep(conn, stepIDByInstance(t, conn, "review@0#0"), "", nowMS+2)
	testsupport.Must(t, err, "RenderStep(review@0#0): %v", err)
	if !strings.Contains(rendered.Packet, "target_sha: "+patch) {
		t.Errorf("the rendered packet does not name the patch commit as its target:\n%s", rendered.Packet)
	}
	if !strings.Contains(rendered.Packet, "+lint fixed") || strings.Contains(rendered.Packet, "lint defect") {
		t.Errorf("the rendered packet does not show the patched diff:\n%s", rendered.Packet)
	}

	// THE MANIFEST: the review fanout is offered, and with the real git probes
	// no row is stale — the branch carries the patch (cherry-picked, so by
	// patch containment rather than ancestry).
	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS+2)
	testsupport.Must(t, err, "dispatch open: %v", err)
	offered := map[string]bool{}
	for _, row := range m.Rows {
		offered[row.Instance] = true
	}
	for _, instance := range []string{"review@0#0", "review@0#1", "review@0#2", "review@0#3"} {
		if !offered[instance] {
			t.Errorf("dispatch open does not offer %s: %v", instance, offered)
		}
	}
	if len(m.StaleTargets) != 0 {
		t.Errorf("dispatch open flagged stale targets after the re-pin: %+v", m.StaleTargets)
	}

	// And the target the dispatch verbs THEMSELVES resolve for those rows is
	// the patched commit: with ancestry faked as disproved and every acquittal
	// probe faked unanswerable, the advisory names the sha it resolved.
	e.IsAncestorFn = func(string, string) (ancestor, known bool) { return false, true }
	e.PatchContainedFn = func(string, string) (contained, known bool) { return false, false }
	e.TreeMatchFn = func(string, string) (match, known bool) { return false, false }
	e.ObjectExistsFn = func(string, string) (exists, known bool) { return false, false }
	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS+3)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify mismatch: %+v", mismatch)
	}
	if len(result.StaleTargets) != 4 {
		t.Fatalf("with ancestry disproved, stale targets = %d, want the four review rows: %+v",
			len(result.StaleTargets), result.StaleTargets)
	}
	for _, s := range result.StaleTargets {
		if s.TargetSHA != patch {
			t.Errorf("%s resolves its target to %q, want the patch commit %q — the "+
				"dispatch verbs still bind the pre-patch record", s.Instance, s.TargetSHA, patch)
		}
	}
}

// TestRerunGatesWorktreeRepinsAndMeasuresThere: with rerun-gates the re-pin
// also re-points the step's recorded worktree, so the gates re-run over the
// patched tree — here a SECOND checkout, distinct from the one the executor
// used — and the routing stage's own recompute records no duplicate.
func TestRerunGatesWorktreeRepinsAndMeasuresThere(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	w := diffRepinFixture(t, conn, e, run.ID)

	// The conductor patches in a fresh checkout of the executor's commit.
	patched := filepath.Join(t.TempDir(), "wt2")
	gitRun(t, w.execRoot, "worktree", "add", "-q", "--detach", patched, w.executorSHA)
	writeFile(t, patched, "internal/work.txt", "the executor's change\nlint fixed\n")
	gitRun(t, patched, "add", "-A")
	gitRun(t, patched, "commit", "-q", "-m", "conductor patch")
	patch := gitRun(t, patched, "rev-parse", "HEAD")

	w.gates.fail = false
	spawnsBefore := len(w.gates.measured())
	out, err := e.ResolveStepWith(conn, w.implementID, ResolveOptions{
		As: ResolveRerunGates, Worktree: patched, NowMS: nowMS + 1,
	})
	testsupport.Must(t, err, "resolve --as rerun-gates --worktree: %v", err)

	if out.Repin == nil || out.Repin.ToSHA != patch || out.Repin.FromSHA != w.executorSHA {
		t.Fatalf("repin = %+v, want %s -> %s", out.Repin, w.executorSHA, patch)
	}
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Errorf("implement@0 = %q after passing gates, want %q", got, db.StepDone)
	}

	// THE GATES MEASURED THE PATCHED CHECKOUT, every one of them.
	measured := w.gates.measured()[spawnsBefore:]
	if len(measured) == 0 {
		t.Fatal("no gate ran on the rerun")
	}
	for _, root := range measured {
		if root != patched {
			t.Errorf("a gate measured %q, want the re-pinned checkout %q", root, patched)
		}
	}
	step, err := db.GetStep(conn, w.implementID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.WorkRoot != patched {
		t.Errorf("work_root = %q after the re-pin, want %q", step.WorkRoot, patched)
	}

	// EXACTLY ONE NEW RECORD: the routing stage recomputed the same bytes over
	// the same tree and the DKT-258 guard declined to record them twice.
	artifacts := issueDiffArtifacts(t, conn, w.implementID)
	if len(artifacts) != 2 {
		t.Fatalf("implement@0 holds %d issue.diff artifacts, want 2 (the original "+
			"and the re-pin, with no duplicate from the rerun's routing stage): %+v",
			len(artifacts), artifacts)
	}
	if artifacts[1].Supersedes != artifacts[0].Artifact {
		t.Errorf("newest record supersedes %q, want %s", artifacts[1].Supersedes, artifacts[0].Artifact)
	}
	if got := latestIssueDiffHead(conn, run.ID, step.IssueID); got != patch {
		t.Errorf("the issue's newest recorded head = %q, want %q", got, patch)
	}
}

// TestWorktreeRepinUnchangedRecordsNothing: re-pinning to a checkout the newest
// record already describes, at the same head, is DKT-258's byte-identical rule
// in the re-pin's terms — the resolution proceeds, nothing is recorded, no
// event is written, and the outcome says so rather than the verb refusing.
func TestWorktreeRepinUnchangedRecordsNothing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	w := diffRepinFixture(t, conn, e, run.ID)

	out, err := e.ResolveStepWith(conn, w.implementID, ResolveOptions{
		As: ResolveOverridePass, Worktree: w.worktree, NowMS: nowMS + 1,
	})
	testsupport.Must(t, err, "resolve: %v", err)
	if out.Repin == nil || !out.Repin.Unchanged {
		t.Fatalf("repin = %+v, want Unchanged over an untouched tree", out.Repin)
	}
	if out.Repin.ToSHA != w.executorSHA || out.Repin.Artifact != "" {
		t.Errorf("an unchanged re-pin reports %+v; it must name the head and no new artifact", out.Repin)
	}
	if got := len(issueDiffArtifacts(t, conn, w.implementID)); got != 1 {
		t.Errorf("issue.diff artifacts = %d after an unchanged re-pin, want 1", got)
	}
	if got := len(diffRepinEvents(t, conn)); got != 0 {
		t.Errorf("issue-diff-repinned events = %d after an unchanged re-pin, want 0", got)
	}
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Errorf("implement@0 = %q, want %q — the resolution itself must still land", got, db.StepDone)
	}
}

// TestWorktreeRepinRefusals: every refusal fires BEFORE anything commits, and
// names what actually went wrong rather than the symptom.
func TestWorktreeRepinRefusals(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	w := diffRepinFixture(t, conn, e, run.ID)
	conductorPatch(t, w)

	cases := []struct {
		name     string
		opts     ResolveOptions
		wantText string
		why      string
	}{
		{
			name:     "retry",
			opts:     ResolveOptions{As: ResolveRetry, Worktree: w.worktree},
			wantText: ResolveOverridePass,
			why:      "retry re-executes and records its own diff; the flag rides only the resolutions that keep this record",
		},
		{
			name:     "skip",
			opts:     ResolveOptions{As: ResolveSkip, Worktree: w.worktree},
			wantText: ResolveRerunGates,
			why:      "a skipped step's record is read by nothing downstream",
		},
		{
			name:     "a path that is not there",
			opts:     ResolveOptions{As: ResolveOverridePass, Worktree: filepath.Join(t.TempDir(), "gone")},
			wantText: "not a directory",
			why:      "GitDiff swallows a missing checkout as an empty diff, which would refuse for the wrong reason",
		},
		{
			name:     "a directory that is not a checkout",
			opts:     ResolveOptions{As: ResolveOverridePass, Worktree: t.TempDir()},
			wantText: "could not resolve the HEAD commit",
			why:      "a tree with no commit has no sha to bind the target to",
		},
		{
			name:     "the shared checkout, already integrated",
			opts:     ResolveOptions{As: ResolveOverridePass, Worktree: w.execRoot},
			wantText: "is empty",
			why:      "its diff against its own HEAD is empty, and DKT-259 forbids replacing a recorded change with nothing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.NowMS = nowMS + 1
			_, err := e.ResolveStepWith(conn, w.implementID, tc.opts)
			if err == nil {
				t.Fatalf("the resolution succeeded; %s", tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not name %q; %s", err, tc.wantText, tc.why)
			}
			if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
				t.Errorf("implement@0 = %q after a refused resolution, want it still parked", got)
			}
			if got := len(issueDiffArtifacts(t, conn, w.implementID)); got != 1 {
				t.Errorf("a refused re-pin recorded an artifact: %d issue.diff rows", got)
			}
		})
	}
}

// repinReaderWorkflow parks a NON-holding step so the "nothing to re-pin"
// refusal is reachable: `review` reads issue.diff and holds no tree, and a
// failing gate parks it.
const repinReaderWorkflow = `
[pipeline]
name = "repin-reader"
version = 1
[[step]]
name = "implement"
after = []
executor = "author"
emits = "change-summary"
[[step]]
name = "review"
holds_tree = false
after = ["implement"]
executor = "judge"
emits = "findings"
inputs = ["issue.diff"]
gates = ["lint"]
on_fail = "waiting-human"
`

// TestWorktreeRepinRefusesAStepWithNoRecordOfItsOwn: a `holds_tree = false`
// reader resolves its diff from someone else's record, so a re-pin on it would
// revise a record nothing downstream reads. The refusal points at the producer.
func TestWorktreeRepinRefusesAStepWithNoRecordOfItsOwn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	conn := mustDB(t)
	registerSource(t, conn, []byte(repinReaderWorkflow), "repin-reader.toml")
	issue := createIssue(t, conn, "read the diff", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	gates := &repinGates{fail: true}
	e.Gates = gates
	reviewID := stepIDByInstance(t, conn, "review@0")
	claim, err := ClaimStep(conn, reviewID, ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)
	err = e.CompleteStep(conn, reviewID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("findings"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete review: %v", err)
	if got := stepStatus(t, conn, "review@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: review@0 = %q, want %q", got, db.StepWaitingHuman)
	}

	checkout := gitRepo(t)
	_, err = e.ResolveStepWith(conn, reviewID, ResolveOptions{
		As: ResolveOverridePass, Worktree: checkout, NowMS: nowMS + 1,
	})
	if err == nil {
		t.Fatal("a re-pin on a non-holding reader succeeded; it records no issue.diff of its own")
	}
	if !strings.Contains(err.Error(), "records no issue.diff of its own") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// diffArtifactID lifts the numeric id out of an `ARTIFACT-N` reference.
func diffArtifactID(t *testing.T, ref string) int {
	t.Helper()
	id, err := strconv.Atoi(strings.TrimPrefix(ref, "ARTIFACT-"))
	testsupport.Must(t, err, "parsing %q: %v", ref, err)
	return id
}
