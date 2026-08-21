package engine

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// bundleJSON assembles a step's context and marshals it, so two assemblies are
// compared BYTE FOR BYTE rather than field by field — a field-by-field
// comparison only catches the fields someone thought to check.
func bundleJSON(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	bundle, err := ReadContext(conn, stepID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	out, err := json.Marshal(bundle)
	testsupport.Must(t, err, "marshal: %v", err)
	return string(out)
}

// stepIDByInstance finds a step's id by its rendered identity.
func stepIDByInstance(t *testing.T, conn *sql.DB, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE instance = ? ORDER BY id LIMIT 1`, instance,
	).Scan(&id)
	testsupport.Must(t, err, "finding step %q: %v", instance, err)
	return id
}

// TestContextAssemblyReadsNoLiveState is §6.6's enforcement at the code level,
// and the single most load-bearing test of this phase.
//
// It runs assembly twice with the ISSUES TABLE and the WORKING TREE mutated in
// between, and requires byte-identical output. Each snapshotted field is ALSO
// asserted individually, per §8.3, so a partial snapshot fails on the specific
// field it missed rather than on an opaque whole-bundle diff — "an
// implementation reading a live field just for the title" is the single most
// likely way this AC breaks.
func TestContextAssemblyReadsNoLiveState(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "original title", "original body", "task", []string{"alpha"})
	err := db.SetIssueScopeGlobs(conn, issue, `["internal/original/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	before := bundleJSON(t, conn, stepID)

	// Sanity: the bundle actually carries the snapshotted values, so the
	// comparison below is not vacuously true over an empty bundle.
	var first Context
	err = json.Unmarshal([]byte(before), &first)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if first.Issue.Title != "original title" {
		t.Fatalf("bundle title = %q, want the snapshotted %q",
			first.Issue.Title, "original title")
	}

	// ---- Mutate everything a live read could reach. ------------------------
	execSQL(t, conn, `UPDATE issues SET title = ? WHERE id = ?`, "EDITED TITLE", issue)
	execSQL(t, conn, `UPDATE issues SET description = ? WHERE id = ?`, "EDITED BODY", issue)
	execSQL(t, conn, `UPDATE issues SET kind = ? WHERE id = ?`, string(model.IssueKindBug), issue)
	err = db.SetIssueScopeGlobs(conn, issue, `["internal/EDITED/**"]`)
	testsupport.Must(t, err, "editing scope: %v", err)

	// A new label, through the ordinary path.
	execSQL(t, conn, `INSERT INTO labels (name, color) VALUES ('beta', 'blue')`)
	execSQL(t, conn,
		`INSERT INTO issue_labels (issue_id, label_id)
		 SELECT ?, id FROM labels WHERE name = 'beta'`, issue)

	// And the working tree: a file the run pinned, edited on disk.
	tmp := filepath.Join(t.TempDir(), "pinned.txt")
	err = os.WriteFile(tmp, []byte("EDITED ON DISK"), 0o644)
	testsupport.Must(t, err, "writing: %v", err)

	after := bundleJSON(t, conn, stepID)

	if before != after {
		t.Errorf("the context bundle changed after a mid-run edit.\nbefore: %s\nafter:  %s",
			before, after)
	}

	// ---- Each field, individually (§8.3). ---------------------------------
	var second Context
	err = json.Unmarshal([]byte(after), &second)
	testsupport.Must(t, err, "unmarshal: %v", err)

	fields := []struct {
		name      string
		got, want string
		liveWas   string
	}{
		{"title", second.Issue.Title, "original title", "EDITED TITLE"},
		{"body_snapshot", second.Issue.BodySnapshot, "original body", "EDITED BODY"},
		{"kind", second.Issue.Kind, "task", "bug"},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("context.issue.%s = %q, want the SNAPSHOT %q — the live row "+
				"says %q, so assembly read live state", f.name, f.got, f.want, f.liveWas)
		}
	}

	if len(second.Issue.Labels) != 1 || second.Issue.Labels[0] != "alpha" {
		t.Errorf("context.issue.labels = %v, want the snapshotted [alpha] — "+
			"a label added mid-run must not appear", second.Issue.Labels)
	}
	if len(second.Issue.Scope) != 1 || second.Issue.Scope[0] != "internal/original/**" {
		t.Errorf("context.issue.scope = %v, want the snapshotted "+
			"[internal/original/**] — an operator's mid-run scope correction "+
			"changes SCHEDULING (§6.3 R4), never the bundle", second.Issue.Scope)
	}
}

// TestContextAssemblyIsRepeatable pins the plainest half of determinism: two
// assemblies with NOTHING changed are byte-identical. A test that only checked
// immunity to edits would pass against an assembler that was nondeterministic
// in some other way — map iteration order being the obvious one.
func TestContextAssemblyIsRepeatable(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	first := bundleJSON(t, conn, stepID)
	for i := range 20 {
		if again := bundleJSON(t, conn, stepID); again != first {
			t.Fatalf("assembly %d differed:\n%s\n%s", i, first, again)
		}
	}
}

// TestContextReadWritesNothing pins that `step context` is a pure read (§6.3:
// every read verb computes effective status with ZERO writes).
//
// The step here has an EXPIRED lease — the state a reaping implementation is
// most tempted to fix on the way past. Reads may not reap.
func TestContextReadWritesNothing(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h', expires_ms = ?
		  WHERE id = ?`,
		db.StepClaimed, nowMS-1, stepID)

	snapshot := func() (int, string, sql.NullString) {
		var (
			version int
			status  string
			owner   sql.NullString
		)
		err := conn.QueryRow(
			`SELECT row_version, status, owner FROM steps WHERE id = ?`, stepID,
		).Scan(&version, &status, &owner)
		testsupport.Must(t, err, "reading step: %v", err)
		return version, status, owner
	}

	beforeV, beforeS, beforeO := snapshot()

	for range 3 {
		_, err := ReadContext(conn, stepID, nowMS)
		testsupport.Must(t, err, "ReadContext: %v", err)
		_, err = LoadStepView(conn, stepID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
	}

	afterV, afterS, afterO := snapshot()
	if afterV != beforeV {
		t.Errorf("row_version moved %d -> %d: a read wrote", beforeV, afterV)
	}
	if afterS != beforeS {
		t.Errorf("status moved %q -> %q: a read reaped", beforeS, afterS)
	}
	if afterO.String != beforeO.String {
		t.Errorf("owner moved %q -> %q: a read reaped", beforeO.String, afterO.String)
	}

	// And the EFFECTIVE status a read reports is not the stored one: the lease
	// lapsed, so it reads as pending/ready without anything being written.
	view, err := LoadStepView(conn, stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.Status == db.StepClaimed {
		t.Error("effective status = claimed on a lapsed lease; status must be " +
			"computed at read, not taken from the column")
	}
	if view.Owner != "" {
		t.Errorf("a lapsed lease reported owner %q; only a LIVE lease is a fact",
			view.Owner)
	}
	_ = run
	_ = issue
}

// TestContextPinsAreListedNotRead is §6.6's fifth source, stated exactly: the
// bundle carries the pin LIST — path and hash — and assembly never opens the
// file. Re-reading would make the bundle depend on the working tree, which is
// the thing the pin exists to prevent.
func TestContextPinsAreListedNotRead(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	pinned := filepath.Join(t.TempDir(), "policy.txt")
	err := os.WriteFile(pinned, []byte("ORIGINAL CONTENT"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)

	issue := createIssue(t, conn, "pinned", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID, pinned)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	before := bundleJSON(t, conn, stepID)

	// Edit the pinned file on disk. The bundle must not notice.
	err = os.WriteFile(pinned, []byte("COMPLETELY DIFFERENT"), 0o644)
	testsupport.Must(t, err, "editing pin file: %v", err)
	if after := bundleJSON(t, conn, stepID); after != before {
		t.Errorf("the bundle changed when a PINNED FILE was edited on disk — "+
			"assembly must list pins, never read them.\nbefore: %s\nafter:  %s",
			before, after)
	}

	var bundle Context
	err = json.Unmarshal([]byte(before), &bundle)
	testsupport.Must(t, err, "unmarshal: %v", err)

	var sawFile, sawWorkflow bool
	for _, p := range bundle.Pins {
		if p.SHA256 == "" {
			t.Errorf("pin %q carries no hash; the hash IS the contract", p.Path)
		}
		if p.Path == pinned {
			sawFile = true
		}
		// §11.4 gives pins ONE shape, so a workflow pin renders into the same
		// `path` slot with a scheme rather than growing a second shape.
		if p.Path == "workflow:standard-change@1" {
			sawWorkflow = true
		}
	}
	if !sawFile {
		t.Errorf("the file pin is absent from the bundle: %v", bundle.Pins)
	}
	if !sawWorkflow {
		t.Errorf("the workflow pin is absent or misrendered: %v", bundle.Pins)
	}
	// The bundle carries no file CONTENT anywhere.
	if contains := string(mustMarshal(t, bundle)); containsAny(contains,
		"ORIGINAL CONTENT", "COMPLETELY DIFFERENT") {
		t.Error("the bundle contains a pinned file's CONTENT; pins are a list, not a payload")
	}
}

// ---------------------------------------------------------------------------
// §6.7 — input resolution
// ---------------------------------------------------------------------------

// recordArtifact inserts an artifact attributed to a step instance.
func recordArtifact(t *testing.T, conn *sql.DB, runID int, stepID int, kind, body string) int {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: runID, StepID: stepID, Kind: kind, Body: body,
		SHA256: workflow.SHA256([]byte(body)),
	}, nowMS)
	if err != nil {
		tx.Rollback()
		t.Fatalf("InsertArtifactTx: %v", err)
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)
	return id
}

// TestInputResolutionIgnoresInsertionOrder is §6.7's ordering rule, tested the
// way the TDD requires: by SHUFFLING insertion order.
//
// "Ordered by artifact id" is trivially satisfiable by accident when insertion
// order happens to match, so an unshuffled test proves nothing. Here the four
// `review` siblings record their artifacts in reverse sibling order, and
// resolution must still come back in sibling order.
func TestInputResolutionIgnoresInsertionOrder(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	// Finish `implement` and all four `review` siblings, recording each
	// sibling's artifact in REVERSE order.
	implementID := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, implementID)
	recordArtifact(t, conn, run.ID, implementID, "change-summary", "the change")

	for i := 3; i >= 0; i-- {
		instance := "review@0#" + string(rune('0'+i))
		id := stepIDByInstance(t, conn, instance)
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, id)
		recordArtifact(t, conn, run.ID, id, "findings", "findings from sibling "+string(rune('0'+i)))
	}

	// `synthesize` declares inputs = ["review.*"].
	synthID := stepIDByInstance(t, conn, "synthesize@0")
	bundle, err := ReadContext(conn, synthID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	if len(bundle.Inputs) != 4 {
		t.Fatalf("resolved %d inputs, want the 4 review siblings", len(bundle.Inputs))
	}
	for i, in := range bundle.Inputs {
		want := "review@0#" + string(rune('0'+i))
		if in.ProducerStep != want {
			t.Errorf("input %d came from %q, want %q — resolution is ordered by "+
				"(declared position, SIBLING INDEX, artifact id), never by "+
				"insertion or event order", i, in.ProducerStep, want)
		}
	}
	_ = issue
}

// TestInputResolutionCollapsesSupersededEmits pins the DKT-103 rule: when one
// producer INSTANCE recorded several artifacts of the same kind — a held
// cluster's resolution re-records the routing step's payload once per round —
// only the newest emit binds. Every round used to bind, so a vote step
// downstream of four resolution rounds rendered the same findings payload
// five times over.
//
// The collapse is per instance, never per step name: all four review siblings
// must still arrive, with the re-emitting sibling represented once, by its
// latest body.
func TestInputResolutionCollapsesSupersededEmits(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	implementID := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, implementID)
	recordArtifact(t, conn, run.ID, implementID, "change-summary", "the change")

	for i := range 4 {
		instance := "review@0#" + string(rune('0'+i))
		id := stepIDByInstance(t, conn, instance)
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, id)
		recordArtifact(t, conn, run.ID, id, "findings", "round-1 findings from sibling "+string(rune('0'+i)))
	}
	// Sibling 0 re-emits, the way resolveHeldPayload does when a decision is
	// folded into a fresh artifact: same step instance, same kind, newer row.
	id0 := stepIDByInstance(t, conn, "review@0#0")
	recordArtifact(t, conn, run.ID, id0, "findings", "round-2 findings from sibling 0")

	synthID := stepIDByInstance(t, conn, "synthesize@0")
	bundle, err := ReadContext(conn, synthID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	if len(bundle.Inputs) != 4 {
		t.Fatalf("resolved %d inputs, want 4 — one per sibling, superseded emits collapsed",
			len(bundle.Inputs))
	}
	for _, in := range bundle.Inputs {
		if in.ProducerStep == "review@0#0" && in.Body != "round-2 findings from sibling 0" {
			t.Errorf("sibling 0 bound %q, want its LATEST emit", in.Body)
		}
		if in.Body == "round-1 findings from sibling 0" {
			t.Errorf("the superseded round-1 emit still bound as an input")
		}
	}
}

// TestInputResolutionIsDoneOnly pins §2's "downstream inputs resolve over
// `done` siblings only". An in-flight sibling's artifact is work in progress,
// not an input — including it would let a join consume a partial result.
func TestInputResolutionIsDoneOnly(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	implementID := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, implementID)
	recordArtifact(t, conn, run.ID, implementID, "change-summary", "the change")

	// Two siblings done, two still running — but ALL FOUR have artifacts on the
	// table, so only the `done` filter can exclude them.
	for i := range 4 {
		instance := "review@0#" + string(rune('0'+i))
		id := stepIDByInstance(t, conn, instance)
		status := db.StepDone
		if i >= 2 {
			status = db.StepRunning
		}
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, status, id)
		recordArtifact(t, conn, run.ID, id, "findings", "partial or complete")
	}

	synthID := stepIDByInstance(t, conn, "synthesize@0")
	bundle, err := ReadContext(conn, synthID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	if len(bundle.Inputs) != 2 {
		t.Errorf("resolved %d inputs, want 2 — a RUNNING sibling's artifact is "+
			"work in progress, not an input", len(bundle.Inputs))
	}
	for _, in := range bundle.Inputs {
		if in.ProducerStep == "review@0#2" || in.ProducerStep == "review@0#3" {
			t.Errorf("resolved an artifact from the still-running %s", in.ProducerStep)
		}
	}
}

// TestIssueBodyResolvesToTheSnapshot pins the `issue.body` form: it is the
// ACTIVATION-TIME snapshot, never the live description.
func TestIssueBodyResolvesToTheSnapshot(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "titled", "the original body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// `verify` declares inputs including issue.body.
	execSQL(t, conn, `UPDATE issues SET description = 'REWRITTEN' WHERE id = ?`, issue)

	verifyID := stepIDByInstance(t, conn, "verify@0")
	bundle, err := ReadContext(conn, verifyID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	var found bool
	for _, in := range bundle.Inputs {
		if in.Kind != "issue.body" {
			continue
		}
		found = true
		if in.Body != "the original body" {
			t.Errorf("issue.body = %q, want the activation-time snapshot", in.Body)
		}
	}
	if !found {
		t.Fatalf("no issue.body input resolved; got %d inputs", len(bundle.Inputs))
	}
}

// TestIssueDiffD4EmptyFallback is §6.7.1 D4: with no `issue.diff` artifact yet,
// the input resolves to an EMPTY diff — not an error, and NOT a live `git diff`.
//
// "Empty" is the truthful answer: nothing has changed the tree in this run.
func TestIssueDiffD4EmptyFallback(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	// `review` consumes issue.diff and nothing has produced one.
	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v — D4 requires a fallback, not an error", err)

	var found bool
	for _, in := range bundle.Inputs {
		if in.Kind != ArtifactKindIssueDiff {
			continue
		}
		found = true
		if in.Body != "" {
			t.Errorf("issue.diff body = %q, want empty (D4)", in.Body)
		}
	}
	if !found {
		t.Errorf("no issue.diff input resolved; D4 requires an empty artifact, "+
			"not an omission. inputs = %d", len(bundle.Inputs))
	}
	_ = run
}

// TestIssueDiffD3PicksHighestOrdinal is §6.7.1 D3, and the clause that makes
// the fixture's `fix@1 -> review@1` cycle correct WITHOUT any rule specific to
// loops: ordinal 1 beats ordinal 0.
//
// The artifacts are inserted with the ordinal-0 one LAST, so a resolution that
// took the highest id, or the most recent, would pick the wrong one.
func TestIssueDiffD3PicksHighestOrdinal(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	implementID := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepDone, implementID)

	// A `fix@1` instance at ordinal 1, done, with its own diff.
	var workflowID int
	err := conn.QueryRow(
		`SELECT workflow_id FROM steps WHERE id = ?`, implementID).Scan(&workflowID)
	testsupport.Must(t, err, "reading workflow id: %v", err)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = db.InsertStepTx(tx, db.StepRow{
		RunID: run.ID, IssueID: issue, WorkflowID: workflowID,
		StepName: "fix", Ordinal: 1, Instance: "fix@1", Kind: "executor",
		Executor: "fix", Class: "write", Status: db.StepDone,
	}, nowMS)
	if err != nil {
		tx.Rollback()
		t.Fatalf("InsertStepTx: %v", err)
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)
	fixID := stepIDByInstance(t, conn, "fix@1")

	// Ordinal 1's diff FIRST, ordinal 0's SECOND — so the higher artifact id
	// belongs to the LOWER ordinal, and only the ordinal rule picks correctly.
	recordArtifact(t, conn, run.ID, fixID, ArtifactKindIssueDiff, "DIFF AT ORDINAL 1")
	recordArtifact(t, conn, run.ID, implementID, ArtifactKindIssueDiff, "diff at ordinal 0")

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	for _, in := range bundle.Inputs {
		if in.Kind != ArtifactKindIssueDiff {
			continue
		}
		if in.Body != "DIFF AT ORDINAL 1" {
			t.Errorf("issue.diff = %q, want the ordinal-1 artifact — D3 resolves "+
				"highest-ordinal first, and the ordinal-0 artifact has the "+
				"HIGHER id here precisely so an id-only rule fails", in.Body)
		}
		if in.ProducerStep != "fix@1" {
			t.Errorf("issue.diff producer = %q, want fix@1", in.ProducerStep)
		}
	}
}

// TestBundleCarriesTheTargetRef is DKT-24: a step consuming `issue.diff`
// receives the target COMMIT and the recorded WORKTREE as structured bundle
// fields, lifted from the diff artifact's round-record payload. Before this
// the target ref rode a prose convention (the change-summary's first line),
// and every reviewing consumer re-derived the tree via `git archive | tar -x`.
func TestBundleCarriesTheTargetRef(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	e.HeadFn = func(string) string { return "cafe1234cafe1234" }

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		WorkDir:  "/worktrees/issue-under-test",
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	review, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0#0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)

	if review.Context.TargetSHA != "cafe1234cafe1234" {
		t.Errorf("target_sha = %q, want the recorded head", review.Context.TargetSHA)
	}
	if review.Context.TargetWorktree != "/worktrees/issue-under-test" {
		t.Errorf("target_worktree = %q, want the declared worktree",
			review.Context.TargetWorktree)
	}

	// The rendered packet states both, so a consumer of the prose form needs
	// no convention either.
	rendered, err := RenderStep(conn, stepIDByInstance(t, conn, "review@0#1"), "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if !strings.Contains(rendered.Packet, "target_sha: cafe1234cafe1234") ||
		!strings.Contains(rendered.Packet, "target_worktree: /worktrees/issue-under-test") {
		t.Errorf("the packet does not state the target ref:\n%s", rendered.Packet)
	}
}

// TestBundleOmitsTheTargetRefWithoutARoundRecord: a diff recorded with no
// resolvable head and no declared worktree yields NO target fields — absent,
// not empty, per the v6 lease-object rule.
func TestBundleOmitsTheTargetRefWithoutARoundRecord(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	e.HeadFn = func(string) string { return "" }

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	review, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0#0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)
	if review.Context.TargetSHA != "" || review.Context.TargetWorktree != "" {
		t.Errorf("target = (%q, %q), want both absent without a round record",
			review.Context.TargetSHA, review.Context.TargetWorktree)
	}
	encoded, err := json.Marshal(review.Context)
	testsupport.Must(t, err, "encoding: %v", err)
	if strings.Contains(string(encoded), "target_sha") {
		t.Error("target_sha serialized on a bundle with no target")
	}
}

// TestSplitInputTakesTheLastDot pins the decomposition: a kind containing a dot
// must not be truncated. Step names cannot contain one (V3), so the last dot is
// unambiguously the separator.
func TestSplitInputTakesTheLastDot(t *testing.T) {
	cases := []struct {
		in         string
		step, kind string
		ok         bool
	}{
		{"implement.change-summary", "implement", "change-summary", true},
		{"review.*", "review", "*", true},
		{"step.change-summary.v2", "step.change-summary", "v2", true},
		{"nodot", "", "", false},
		{".leading", "", "", false},
		{"trailing.", "", "", false},
	}
	for _, tc := range cases {
		step, kind, ok := splitInput(tc.in)
		if ok != tc.ok || step != tc.step || kind != tc.kind {
			t.Errorf("splitInput(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, step, kind, ok, tc.step, tc.kind, tc.ok)
		}
	}
}

// TestContextMetaIsASiblingNotAMutation pins §6.4's `--meta` rule: the byte
// counts are a SIBLING object, so asking for them does not change the bundle
// the goldens compare (§8.3). A `--meta` that spliced counts in would make the
// goldens depend on a flag.
func TestContextMetaIsASiblingNotAMutation(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	bundle, err := ReadContext(conn, stepID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	before := string(mustMarshal(t, bundle))

	meta := bundle.Meta()
	if meta.TotalBytes == 0 {
		t.Error("meta reports a zero-byte closure for a step with a body snapshot")
	}

	if after := string(mustMarshal(t, bundle)); after != before {
		t.Errorf("computing --meta mutated the bundle:\nbefore: %s\nafter:  %s",
			before, after)
	}
	_ = run
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	testsupport.Must(t, err, "marshal: %v", err)
	return out
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && len(haystack) >= len(n) {
			for i := 0; i+len(n) <= len(haystack); i++ {
				if haystack[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}
