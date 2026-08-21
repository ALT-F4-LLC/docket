package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The remaining §9.2 acceptance cases (TDD docs/tdd/gates-trust.md).
//
// gate_exec_test.go proves §9 item 6 at the runner; these cover the end-to-end
// shapes, the structural guards, and the flag table.

// TestPostActivationBodyEditCannotInject is T3, end to end.
//
// An operator approves an issue body at activation; someone edits the body
// afterwards to add or alter a fenced command. THE SNAPSHOTTED COMMAND IS WHAT
// RUNS — S3 harvests from the snapshot, and this stage depends on that rather
// than re-solving it.
func TestPostActivationBodyEditCannotInject(t *testing.T) {
	conn := mustDB(t)
	repoRoot := t.TempDir()
	approvedArgv, approvedSentinel := witnessCommand(t, repoRoot, "approved-ran")
	injectedArgv, injectedSentinel := witnessCommand(t, repoRoot, "injected-ran")

	runID, issueID, stepID := seedFenceStep(t, conn, strings.Join(approvedArgv, " "))

	// THE EDIT: the issue's live body now carries a different command. This is
	// the attacker's move — the row an operator already approved stays put.
	_, err := conn.Exec(
		`UPDATE issues SET description = ? WHERE id = ?`,
		"```checks\n"+strings.Join(injectedArgv, " ")+"\n```", issueID)
	testsupport.Must(t, err, "editing the body: %v", err)

	// Both commands are trusted, so if the injected one does not run, the
	// reason is the snapshot rather than a missing entry.
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t,
		trust.Entry{Name: "checks", Argv: approvedArgv,
			ArgvSHA256: trust.ArgvSHA256(approvedArgv), Repo: mustResolve(repoRoot)},
		trust.Entry{Name: "checks", Argv: injectedArgv,
			ArgvSHA256: trust.ArgvSHA256(injectedArgv), Repo: mustResolve(repoRoot)},
	)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	commands, hashes, err := gateCommands(conn, step, fenceGate())
	testsupport.Must(t, err, "gateCommands: %v", err)
	_, err = runner.Execute(context.Background(), GateSpec{
		Name: "checks", Source: "fence:checks",
		Commands: commands, CommandHashes: hashes,
	}, StepContext{RunID: runID, IssueID: issueID})
	testsupport.Must(t, err, "running the gate: %v", err)

	if !sentinelExists(t, approvedSentinel) {
		t.Error("the SNAPSHOTTED command did not run")
	}
	if sentinelExists(t, injectedSentinel) {
		t.Error("a command added to the body AFTER activation executed; the " +
			"gate must read the snapshot, never the live body")
	}
}

// TestRealResultsCarryNoStubField is T11: nothing this stage produces is a
// stub, and the JSON omits the key entirely rather than emitting `false`.
//
// `stub` means "produced by S3's pass-through". A real result that serialized
// `stub: false` would still be wrong in a subtler way — it would imply the
// distinction is live rather than historical.
func TestRealResultsCarryNoStubField(t *testing.T) {
	repoRoot := t.TempDir()
	argv, _ := witnessCommand(t, repoRoot, "ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	ex, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "Execute: %v", err)
	for _, r := range ex.Results {
		if r.Stub {
			t.Errorf("gate %q produced a stubbed result at S4", r.Gate)
		}
	}

	// The wire shape: `omitempty` means the key is ABSENT, not false.
	encoded, err := json.Marshal(GateResult{
		Gate: "tests", Verdict: VerdictPass,
	})
	testsupport.Must(t, err, "Marshal: %v", err)
	if strings.Contains(string(encoded), "stub") {
		t.Errorf("a real result serializes a `stub` key: %s", encoded)
	}
}

// TestReadVerbsNeverExecute is §2.2's structural claim: no read verb reaches
// internal/exec.
//
// §4 is verbatim that "Read verbs never execute", and this checks it at the
// IMPORT GRAPH rather than by driving each verb — a behavioral test only covers
// the paths it happens to take, while an import a read verb cannot make is a
// path that cannot exist.
func TestReadVerbsNeverExecute(t *testing.T) {
	// The read-shaped verbs named by §2.2, plus the trust reader.
	readVerbs := []string{
		"issue_show.go", "issue_list.go", "issue_log.go", "issue_graph.go",
		"run_status.go", "workflow_show.go", "workflow_list.go",
		"plan.go", "board.go", "stats.go", "export.go",
	}

	cliDir := filepath.Join("..", "cli")
	for _, name := range readVerbs {
		path := filepath.Join(cliDir, name)
		if _, err := os.Stat(path); err != nil {
			continue // a verb that does not exist cannot violate the rule
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		testsupport.Must(t, err, "parsing %s: %v", name, err)
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "internal/exec") {
				t.Errorf("%s imports internal/exec; read verbs never execute (§2.2)", name)
			}
		}
	}

	// The positive half, so the check cannot pass against a package that has
	// no such import anywhere: the code that IS allowed to execute does import
	// it. Without this, deleting internal/exec entirely would "pass".
	found := false
	for _, name := range []string{"gate_exec.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		testsupport.Must(t, err, "parsing %s: %v", name, err)
		for _, imp := range file.Imports {
			if strings.Contains(imp.Path.Value, "internal/exec") {
				found = true
			}
		}
	}
	if !found {
		t.Error("no execution path imports internal/exec; this guard would " +
			"pass vacuously")
	}
}

// TestGateResultShapeCarriesNoAgentVocabulary is the genericity rule applied to
// the one wire shape this stage adds, checked in Go rather than only by the QA
// gate — the struct tags are core surface.
func TestGateResultShapeCarriesNoAgentVocabulary(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gate.go", nil, parser.SkipObjectResolution)
	testsupport.Must(t, err, "parsing gate.go: %v", err)

	// THE LIST IS SOURCED FROM THE GATE'S OWN SCRIPT, not restated here.
	//
	// Two reasons, and the second is the one that bites: restating it lets the
	// two copies drift, so the weaker one becomes the one that runs. And the
	// gate scans `internal/**` INCLUDING TESTS — a literal `"model"` in this
	// file would itself fail the gate, which is exactly what happened the first
	// time this test was written.
	banned := bannedTermsFromGate(t)

	ast.Inspect(file, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Tag == nil {
			return true
		}
		tag := strings.ToLower(field.Tag.Value)
		for _, word := range banned {
			if strings.Contains(tag, word) {
				t.Errorf("a JSON tag in gate.go names %q: %s", word, field.Tag.Value)
			}
		}
		return true
	})
}

// TestReRunnableFlakyMatrix is §7.5's four-way table, one case per row.
//
// F5 names conflating these two flags as the likely implementation error, so
// each combination is asserted separately rather than inferred from two
// independent tests. `flaky` governs re-running WITHIN one gate stage after a
// failure; `re_runnable` governs re-running AFTER A CRASH.
func TestReRunnableFlakyMatrix(t *testing.T) {
	for _, tc := range []struct {
		reRunnable bool
		flaky      bool
		// wantAttempts is how many rows one FAILING gate stage produces.
		wantAttempts int
		// wantResumeRuns is whether a crashed gate re-runs on resume.
		wantResumeRuns bool
	}{
		{false, false, 1, false},
		// Row 2 is the one an implementation gets wrong by treating the flags
		// as one: a command with a non-deterministic exit may still deploy.
		{false, true, 3, false},
		{true, false, 1, true},
		{true, true, 3, true},
	} {
		name := "rerunnable=" + boolWord(tc.reRunnable) + ",flaky=" + boolWord(tc.flaky)
		t.Run(name, func(t *testing.T) {
			repoRoot := t.TempDir()
			// /usr/bin/false always fails, so a flaky entry exhausts its
			// attempts and a non-flaky one runs exactly once.
			argv := []string{"/usr/bin/false"}

			runner := NewExecRunner(testRepoPaths(repoRoot))
			runner.LoadStore = sandboxTrust(t, trust.Entry{
				Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
				Repo:  mustResolve(repoRoot),
				Flaky: tc.flaky, ReRunnable: tc.reRunnable,
			})

			// WITHIN ONE STAGE: `flaky` alone decides the attempt count.
			ex, err := runner.Execute(context.Background(),
				GateSpec{Name: "tests"}, StepContext{})
			testsupport.Must(t, err, "Execute: %v", err)
			if len(ex.Results) != tc.wantAttempts {
				t.Errorf("%d attempts recorded, want %d — `flaky` governs "+
					"re-running within one stage", len(ex.Results), tc.wantAttempts)
			}
			// F3: each attempt is its own row with an ascending ordinal.
			for i, r := range ex.Results {
				if r.Ordinal != i {
					t.Errorf("attempt %d has ordinal %d; every attempt is "+
						"recorded individually", i, r.Ordinal)
				}
			}
			// F4: the verdict is the LAST attempt's.
			if ex.Verdict != VerdictFail {
				t.Errorf("verdict = %q, want %q — every attempt failed",
					ex.Verdict, VerdictFail)
			}

			// AFTER A CRASH: `re_runnable` alone decides, and it is read from
			// the same entry — so a flaky-but-not-re-runnable command parks.
			store, _ := runner.LoadStore()
			match := store.Lookup(mustResolve(repoRoot), "tests", nil)
			if !match.Matched {
				t.Fatal("the entry did not match")
			}
			if match.Entry.ReRunnable != tc.wantResumeRuns {
				t.Errorf("re_runnable = %v, want %v — `flaky` must not imply it",
					match.Entry.ReRunnable, tc.wantResumeRuns)
			}
		})
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// seedFenceStep inserts a run, an issue with a snapshotted body, one harvested
// fence row, and a step — the shape a real activation leaves behind.
func seedFenceStep(t *testing.T, conn *sql.DB, command string) (runID, issueID, stepID int) {
	t.Helper()

	res, err := conn.Exec(
		`INSERT INTO issues (title, description, status, created_at, updated_at)
		 VALUES ('fence subject', ?, 'todo', '2026-08-03', '2026-08-03')`,
		"```checks\n"+command+"\n```")
	testsupport.Must(t, err, "seeding an issue: %v", err)
	iid, _ := res.LastInsertId()

	// The definition must really declare the fence gate: the report maps a tag
	// back to the gate that harvests it, so an empty definition would make
	// every command report "no gate harvests this tag" and the test would be
	// asserting against its own fixture rather than against the matcher.
	parsed := `{"pipeline":{"name":"fence-wf","version":1},` +
		`"steps":[{"name":"check","executor":"author","emits":"note","after":[],` +
		`"gates":[{"name":"checks","source":"fence:checks","pre":false}]}]}`
	res, err = conn.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms)
		 VALUES ('fence-wf', 1, 'x', '', ?, 1)`, parsed)
	testsupport.Must(t, err, "seeding a workflow: %v", err)
	wid, _ := res.LastInsertId()

	res, err = conn.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'active', 1, 1)`)
	testsupport.Must(t, err, "seeding a run: %v", err)
	rid, _ := res.LastInsertId()

	// The SNAPSHOT: what activation harvested, with its hash. The live body
	// above can change; this cannot.
	sum := sha256.Sum256([]byte(command))
	_, err = conn.Exec(
		`INSERT INTO run_fences (run_id, issue_id, tag, ordinal, command, sha256)
		 VALUES (?, ?, 'checks', 0, ?, ?)`,
		rid, iid, command, hex.EncodeToString(sum[:]))
	testsupport.Must(t, err, "seeding a harvested fence: %v", err)

	res, err = conn.Exec(
		`INSERT INTO steps
		   (run_id, issue_id, workflow_id, step_name, instance, kind, status,
		    created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, 'check', 'check@0', 'executor', 'gated', 1, 1)`,
		rid, iid, wid)
	testsupport.Must(t, err, "seeding a step: %v", err)
	sid, _ := res.LastInsertId()

	return int(rid), int(iid), int(sid)
}

// fenceGate is the workflow gate the seeded step declares.
func fenceGate() workflow.Gate {
	return workflow.Gate{Name: "checks", Source: "fence:checks"}
}

// TestActivationReportsFenceTrustStatus is T16: the §7.7 report tells an
// operator which harvested commands will actually run, BEFORE the run.
//
// The gate needs a matching entry regardless, so filing an issue grants
// nothing — what the report adds is that an `unmatched` command is visible up
// front rather than discovered at the gate.
func TestActivationReportsFenceTrustStatus(t *testing.T) {
	conn := mustDB(t)
	repoRoot := t.TempDir()
	trustedArgv, _ := witnessCommand(t, repoRoot, "trusted")
	strangerArgv, _ := witnessCommand(t, repoRoot, "stranger")

	runID, _, _ := seedFenceStep(t, conn, strings.Join(trustedArgv, " "))
	// A second harvested line that nothing authorizes.
	sum := sha256.Sum256([]byte(strings.Join(strangerArgv, " ")))
	_, err := conn.Exec(
		`INSERT INTO run_fences (run_id, issue_id, tag, ordinal, command, sha256)
		 SELECT run_id, issue_id, tag, 1, ?, ? FROM run_fences WHERE run_id = ?`,
		strings.Join(strangerArgv, " "), hex.EncodeToString(sum[:]), runID)
	testsupport.Must(t, err, "seeding a second fence: %v", err)

	load := sandboxTrust(t, trust.Entry{
		Name: "checks", Argv: trustedArgv,
		ArgvSHA256: trust.ArgvSHA256(trustedArgv), Repo: mustResolve(repoRoot),
	})
	reports, err := BuildFenceReport(conn, runID, load, repoRoot)
	testsupport.Must(t, err, "BuildFenceReport: %v", err)
	if len(reports) != 2 {
		t.Fatalf("got %d reported commands, want 2", len(reports))
	}

	if !reports[0].Matched || reports[0].Entry != "checks" {
		t.Errorf("the trusted command reports %+v, want matched by `checks`", reports[0])
	}
	if reports[1].Matched {
		t.Error("an unauthorized command reports as matched")
	}
	if reports[1].Reason == "" {
		t.Error("an unmatched command carries no reason; the operator cannot act")
	}
	// Body order is preserved, which is the order the operator read and the
	// order the commands will run in.
	if reports[0].Ordinal != 0 || reports[1].Ordinal != 1 {
		t.Errorf("ordinals = %d,%d; the report must follow body order",
			reports[0].Ordinal, reports[1].Ordinal)
	}
}

// TestActivationReportEscapesFenceControlBytes is T18 end to end.
//
// A fenced command carrying `\x1b[2K\r` would repaint the very line an operator
// is approving. The report renders it ESCAPED and VISIBLE, while the stored
// bytes stay exactly what activation hashed — E5, which is what keeps §7.3's
// hash verification meaningful.
func TestActivationReportEscapesFenceControlBytes(t *testing.T) {
	conn := mustDB(t)
	repoRoot := t.TempDir()

	// The attack line: what a terminal SHOWS diverges from what is STORED.
	hostile := "/usr/bin/true\x1b[2K\r  /usr/bin/false"
	runID, _, _ := seedFenceStep(t, conn, hostile)

	reports, err := BuildFenceReport(conn, runID, sandboxTrust(t), repoRoot)
	testsupport.Must(t, err, "BuildFenceReport: %v", err)
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}

	// E5: the STORED bytes are unmodified — escaping is a rendering transform.
	if reports[0].Command != hostile {
		t.Error("the report mutated the stored command; escaping happens at " +
			"the print boundary, so the hash still verifies")
	}

	// The rendered form: escapes VISIBLE, and no raw control byte written.
	var rendered strings.Builder
	RenderFenceReport(&rendered, reports)
	out := rendered.String()
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\r') {
		t.Error("a raw control byte reached the writer; the operator's line " +
			"can be repainted by content an issue author wrote")
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("the escape is not visible in the rendered report: %q", out)
	}

	// E4: the JSON form carries the RAW bytes, since encoding/json escapes
	// controls by contract and the consumer is a program.
	encoded, err := json.Marshal(reports[0])
	testsupport.Must(t, err, "Marshal: %v", err)
	var back FenceReport
	err = json.Unmarshal(encoded, &back)
	testsupport.Must(t, err, "Unmarshal: %v", err)
	if back.Command != hostile {
		t.Error("the JSON form did not round-trip the original bytes; a " +
			"machine consumer must see what was stored")
	}
}

// bannedTermsFromGate parses the BANNED array out of scripts/qa/genericity.sh.
//
// The gate scans internal/** including tests, so a Go file that RESTATED the
// list would fail the very check it implements. Reading the gate's own source
// is both the honest version and the only one that compiles cleanly.
func bannedTermsFromGate(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "qa", "genericity.sh"))
	if err != nil {
		t.Skipf("genericity.sh unavailable: %v", err)
	}

	var out []string
	inArray := false
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "BANNED=("):
			inArray = true
		case inArray && trimmed == ")":
			inArray = false
		case inArray && trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		t.Fatal("could not read BANNED from genericity.sh; this guard would " +
			"pass vacuously against an empty list")
	}
	return out
}
