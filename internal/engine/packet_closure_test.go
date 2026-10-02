package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// PACKET-CLOSURE PINNING (DKT-581): activation pins the closure the bound
// workflows actually reach — packet entries, their `packet_includes`, and
// policy.toml — not every file under the config roots. 7 of 18 terminal runs
// in the measured week were abandoned over pin drift in corpus files they
// never read; RUN-30's verify-pins named 20 drifted pins of which 11 were
// contracts its workflow never renders.

// rewriteConfigFile edits a config file in place — the mid-run corpus install
// these tests simulate.
func rewriteConfigFile(t *testing.T, configDir, rel, body string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(configDir, rel), []byte(body), 0o644)
	testsupport.Must(t, err, "rewriting %s: %v", rel, err)
}

// closureRun activates one issue against a config tree whose workflow declares
// `packet = ["contracts/used.md"]`, with used.md carrying a `packet_includes`
// to fragments/style.md, beside an UNREFERENCED contracts/unused.md and a
// policy.toml.
func closureRun(t *testing.T) (conn *sql.DB, configDir string, runID int) {
	t.Helper()
	conn, configDir = configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/used.md\"]\n")
	writeConfigFile(t, configDir, "contracts/used.md",
		"---\npacket_includes:\n  - fragments/style.md\n---\nthe used contract\n")
	writeConfigFile(t, configDir, "fragments/style.md", "the included fragment\n")
	writeConfigFile(t, configDir, "contracts/unused.md", "referenced by nothing\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "closure subject", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return conn, configDir, run.ID
}

// TestVerifyPinsIgnoresCorpusEditOutsideTheClosure is DKT-581's first
// acceptance criterion, verbatim: a corpus edit to a contract no step in an
// active run references does not fail verify-pins for that run.
func TestVerifyPinsIgnoresCorpusEditOutsideTheClosure(t *testing.T) {
	conn, configDir, runID := closureRun(t)

	// The pin set is the closure: the declared contract, its included
	// fragment, and policy.toml — and NOT the unreferenced contract.
	refs := map[string]bool{}
	for _, p := range pinsByKind(t, conn, runID, db.PinKindFile) {
		refs[filepath.ToSlash(p.Ref)] = true
	}
	for _, want := range []string{"contracts/used.md", "fragments/style.md", "policy.toml"} {
		if !refs[want] {
			t.Errorf("pin refs %v do not include %q; the closure must cover it", refs, want)
		}
	}
	if refs["contracts/unused.md"] {
		t.Fatalf("pin refs %v include the unreferenced contract; the corpus edit "+
			"below would then drift a file this run never reads", refs)
	}

	// The mid-run corpus install, hitting ONLY the unreferenced file.
	rewriteConfigFile(t, configDir, "contracts/unused.md", "REWRITTEN BY THE INSTALL\n")

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Errorf("verify-pins reports drift after an edit outside the closure: %s",
			PinReportReason(report))
	}
}

// TestVerifyPinsStillReportsDriftInsideTheClosure is the second acceptance
// criterion — narrowing the pin set must not silence legitimate detection. A
// directly declared contract AND a fragment reachable only through
// `packet_includes` both still drift.
func TestVerifyPinsStillReportsDriftInsideTheClosure(t *testing.T) {
	conn, configDir, runID := closureRun(t)

	rewriteConfigFile(t, configDir, "contracts/used.md", "the install's new contract\n")
	rewriteConfigFile(t, configDir, "fragments/style.md", "the install's new fragment\n")

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Changed != 2 {
		t.Fatalf("verify-pins reports %d changed pin(s), want 2 (the contract and "+
			"its included fragment): %+v", report.Changed, report.Pins)
	}
	changed := map[string]bool{}
	for _, v := range report.Pins {
		if v.Status == PinChanged {
			changed[filepath.ToSlash(v.Ref)] = true
		}
	}
	if !changed["contracts/used.md"] {
		t.Error("a drifted contract a step's packet directly declares was not reported")
	}
	if !changed["fragments/style.md"] {
		t.Error("a drifted fragment reachable via packet_includes was not reported")
	}

	// And repin — the recovery verb — still adopts exactly those two.
	outcome, err := RepinRun(conn, runID, "corpus install", nowMS)
	testsupport.Must(t, err, "RepinRun: %v", err)
	if len(outcome.Repinned) != 2 {
		t.Fatalf("repinned %d pin(s), want the 2 drifted ones: %+v",
			len(outcome.Repinned), outcome.Repinned)
	}
	after, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins after repin: %v", err)
	if !after.Sound() {
		t.Errorf("the run is still unsound after repin: %s", PinReportReason(after))
	}
}

// closureLoopWorkflowSrc exercises every substitution branch the closure walk
// has: a literal entry, a `{executor}` entry on a FANOUT step (per hint), a
// `{executor}` entry on a plain step (declared executor), a `loop = true`
// body's entry (loop bodies never appear in ordinary expansion but do render
// at loop entry), and a step a false `when` will skip (expansion still
// requires its entries pinned).
const closureLoopWorkflowSrc = `
[pipeline]
name = "closure-loop"
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
fanout = ["judge-a", "judge-b"]
emits = "findings"
inputs = ["implement.change-summary"]
packet = ["contracts/{executor}.md"]

[[step]]
name = "verify"
after = ["review"]
executor = "verify"
emits = "ac-report"
inputs = ["implement.change-summary"]
packet = ["contracts/{executor}.md"]
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 1

[[step]]
name = "fix"
executor = "fix"
loop = true
emits = "change-summary"
inputs = ["implement.change-summary"]
packet = ["contracts/fix.md"]
after_loop = "review"

[[step]]
name = "extra"
after = ["implement"]
executor = "extra"
emits = "note"
when = "labels contains never-applied"
packet = ["contracts/extra.md"]
`

// TestClosureCoversHintsLoopBodiesAndSkippedSteps pins the walk's breadth:
// every declared step contributes its entries — fanout per hint, loop bodies,
// `when`-skipped steps — while a contract no declaration reaches stays out.
func TestClosureCoversHintsLoopBodiesAndSkippedSteps(t *testing.T) {
	conn, dir := configRepo(t)
	writeConfigFile(t, dir, "workflows/closure-loop.toml", closureLoopWorkflowSrc)
	for _, rel := range []string{
		"contracts/implement.md", "contracts/judge-a.md", "contracts/judge-b.md",
		"contracts/verify.md", "contracts/fix.md", "contracts/extra.md",
		"contracts/unreachable.md",
	} {
		writeConfigFile(t, dir, rel, "contract body for "+rel+"\n")
	}

	issue := createIssue(t, conn, "hints and loops", "a body", "task", nil)
	run := startRun(t, conn, issue)
	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	refs := map[string]bool{}
	for _, p := range pinsByKind(t, conn, run.ID, db.PinKindFile) {
		refs[filepath.ToSlash(p.Ref)] = true
	}
	for _, want := range []string{
		"contracts/implement.md", // literal entry
		"contracts/judge-a.md",   // fanout hint substitution, first sibling
		"contracts/judge-b.md",   // fanout hint substitution, second sibling
		"contracts/verify.md",    // {executor} against the declared executor
		"contracts/fix.md",       // loop = true body
		"contracts/extra.md",     // `when`-skipped step: expansion still checks it
	} {
		if !refs[want] {
			t.Errorf("pin refs %v do not include %q", refs, want)
		}
	}
	if refs["contracts/unreachable.md"] {
		t.Errorf("pin refs %v include a contract no step declares", refs)
	}
	if result.PinsFromConfig != 6 {
		t.Errorf("pinned %d config files, want the 6 reachable contracts",
			result.PinsFromConfig)
	}
}

// TestReactivationPinsANewlyBoundWorkflowsClosure is RA3 under DKT-581: an
// issue added mid-run binds a workflow whose files the FIRST activation's
// closure never reached, so the re-activation must ADD those pins — while
// RA2's inheritance keeps every already-pinned ref exactly where it was, even
// after a mid-run edit.
func TestReactivationPinsANewlyBoundWorkflowsClosure(t *testing.T) {
	conn, dir := configRepo(t)
	// Both workflows are registered by the FIRST activation's scan (F15: a
	// re-activation never re-scans), but only auto-dev binds then, so only
	// its contract is in the first closure.
	writeConfigFile(t, dir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/a.md\"]\n")
	other := `
[pipeline]
name = "auto-bug"
version = 1

[match]
kind = ["bug"]

[[step]]
name = "diagnose"
executor = "w"
emits = "out"
after = []
packet = ["contracts/b.md"]
`
	writeConfigFile(t, dir, "workflows/auto-bug.toml", other)
	writeConfigFile(t, dir, "contracts/a.md", "contract a, original\n")
	writeConfigFile(t, dir, "contracts/b.md", "contract b\n")

	first := createIssue(t, conn, "first", "a body", "task", nil)
	run := startRun(t, conn, first)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "the first activation: %v", err)

	refs := map[string]string{}
	for _, p := range pinsByKind(t, conn, run.ID, db.PinKindFile) {
		refs[filepath.ToSlash(p.Ref)] = p.SHA256
	}
	if _, ok := refs["contracts/b.md"]; ok {
		t.Fatal("premise: the unbound workflow's contract must not be in the " +
			"first activation's closure")
	}
	pinnedA := refs["contracts/a.md"]
	if pinnedA == "" {
		t.Fatal("premise: the bound workflow's contract must be pinned")
	}

	// The mid-run corpus edit RA2 makes a non-event.
	rewriteConfigFile(t, dir, "contracts/a.md", "contract a, edited mid-run\n")

	second := createIssue(t, conn, "second", "a body", "bug", nil)
	err = db.AddRunIssue(conn, run.ID, second)
	testsupport.Must(t, err, "adding the second issue: %v", err)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "the re-activation: %v", err)
	if !result.Reactivation {
		t.Fatal("premise: the second activation must be a re-activation")
	}

	after := map[string]string{}
	for _, p := range pinsByKind(t, conn, run.ID, db.PinKindFile) {
		after[filepath.ToSlash(p.Ref)] = p.SHA256
	}
	if after["contracts/b.md"] == "" {
		t.Error("RA3: the newly-bound workflow's contract was not pinned, so its " +
			"steps could never render")
	}
	if after["contracts/a.md"] != pinnedA {
		t.Errorf("RA2: the inherited pin moved from %s to %s; a mid-run edit must "+
			"be a non-event", pinnedA, after["contracts/a.md"])
	}
}
