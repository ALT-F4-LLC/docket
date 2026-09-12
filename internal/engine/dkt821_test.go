package engine

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-821 — `verify-pins` reported a structurally wedged run as healthy.
//
// On RUN-59, minutes after all four review@2 claims died on `packet file
// "fragments/laziness-ladder.md" is not pinned by this run` (VALIDATION_ERROR),
// `docket run verify-pins RUN-59 --json` answered exit 0 with all 30 pins
// `"status":"ok"`. Every pinned ref DID match its bytes — and those very bytes
// referenced two fragments the run never pinned, which is what made every
// remaining judge step unclaimable. A conductor used the verb as a pre-dispatch
// health check and launched four executors into claims that could not succeed.
//
// The per-pin check cannot see this: it asks "do these bytes still match", and
// they do. The missing question is what the bytes REFERENCE. These tests hold
// the verb to both halves.

// dkt821WorkflowSrc declares one step reading one contract — the pin set at
// activation is that contract, whatever its `packet_includes` reach, and
// policy.toml.
const dkt821WorkflowSrc = `
[pipeline]
name = "dkt821-dev"
version = 1

[match]
kind = ["task"]

[[step]]
name = "judge"
executor = "judge"
emits = "change-summary"
after = []
packet = ["contracts/judge.md"]
`

// dkt821ContractB1 is RUN-59's corpus edit: the contract's second edition
// reaches a fragment the run's pin set knows nothing about.
const dkt821ContractB1 = "---\npacket_includes:\n  - fragments/laziness-ladder.md\n---\n" +
	"the judge contract, second edition\n"

// movePinTo is the ONE statement a pre-DKT-805 repin ran: the pin row moves to
// the adopted bytes, and nothing walks what those bytes now reference.
//
// It is how RUN-59 reached the state it is in, and how every run repinned
// before dbd3e7b still sits in the store. A repin TODAY closes the hole in the
// same transaction (dkt805_test.go), so driving one here would build the
// opposite of the fixture — see the companion test below, which drives exactly
// that and asserts the closed result.
func movePinTo(t *testing.T, conn *sql.DB, runID int, ref, sha string) {
	t.Helper()
	res, err := conn.Exec(
		`UPDATE pins SET sha256 = ? WHERE run_id = ? AND ref = ?`, sha, runID, ref)
	testsupport.Must(t, err, "moving pin %s: %v", ref, err)
	n, err := res.RowsAffected()
	testsupport.Must(t, err, "RowsAffected: %v", err)
	if n != 1 {
		t.Fatalf("moving pin %s touched %d row(s), want 1", ref, n)
	}
}

// writtenAt is writeConfigFile returning the path the config ROOTS resolve to.
// On darwin `t.TempDir()` hands back a /tmp path whose real location is
// /private/tmp, and the roots are evaluated — an assertion on the unresolved
// spelling would fail for a reason that has nothing to do with pins.
func writtenAt(t *testing.T, configDir, rel, body string) string {
	t.Helper()
	path := writeConfigFile(t, configDir, rel, body)
	real, err := filepath.EvalSymlinks(path)
	testsupport.Must(t, err, "resolving %s: %v", path, err)
	return real
}

// TestVerifyPinsReportsAReferenceThePinSetDoesNotHold is AC1 and AC2 — RUN-59's
// shape, and the verdict the verb owed it. Every pin matches disk; the pinned
// contract's own bytes reach a fragment the run never pinned; the verb must
// refuse and name BOTH files.
func TestVerifyPinsReportsAReferenceThePinSetDoesNotHold(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt821-dev.toml", dkt821WorkflowSrc)
	writeConfigFile(t, configDir, "contracts/judge.md", "the judge contract\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "closure blindness", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The corpus commit, then the repin that adopted it as repin behaved on the
	// day RUN-59 wedged: the contract's pin moves to B1's bytes, the fragment
	// B1 now reaches is pinned by nothing.
	writeConfigFile(t, configDir, "contracts/judge.md", dkt821ContractB1)
	fragment := writtenAt(t, configDir, "fragments/laziness-ladder.md",
		"the laziness ladder\n")
	movePinTo(t, conn, run.ID, "contracts/judge.md",
		workflow.SHA256([]byte(dkt821ContractB1)))
	if n := pinCount(t, conn, run.ID, "fragments/laziness-ladder.md"); n != 0 {
		t.Fatalf("premise: the fragment is already pinned (%d row(s))", n)
	}

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)

	// THE BLINDNESS ITSELF: the per-pin half is spotless. That is exactly what
	// exit 0 was reporting, and why the closure half had to be a separate
	// question rather than a stricter reading of the same one.
	if report.Changed != 0 || report.Missing != 0 {
		t.Fatalf("changed = %d, missing = %d, want 0 and 0 — the fixture is "+
			"RUN-59's, where every pinned ref matches its bytes",
			report.Changed, report.Missing)
	}
	if report.Sound() {
		t.Fatal("the report reads sound on a run whose pinned contract " +
			"references a fragment the run does not pin — every step reading " +
			"that contract is unclaimable")
	}
	if report.Unpinned != 1 || len(report.References) != 1 {
		t.Fatalf("unpinned = %d, references = %+v, want exactly one",
			report.Unpinned, report.References)
	}

	got := report.References[0]
	if got.Status != PinUnpinnedReference {
		t.Errorf("status = %q, want %q", got.Status, PinUnpinnedReference)
	}
	if got.Ref != "fragments/laziness-ladder.md" {
		t.Errorf("ref = %q, want fragments/laziness-ladder.md", got.Ref)
	}
	// AC1 asks for BOTH names: the ref, and the contract that references it.
	if len(got.IncludedBy) != 1 || got.IncludedBy[0] != "contracts/judge.md" {
		t.Errorf("included_by = %v, want [contracts/judge.md] — an operator "+
			"needs the file that wrote the reference, not just the ref", got.IncludedBy)
	}
	// And the claims that will die, which is what a conductor is deciding about.
	if len(got.RequiredBy) != 1 || got.RequiredBy[0] != "judge@0" {
		t.Errorf("required_by = %v, want [judge@0]", got.RequiredBy)
	}
	// Present-but-unpinned is RUN-59's case, and DKT-818's distinction: the
	// remedy is not on the filesystem, so the report must say the file is there.
	if got.Path != fragment {
		t.Errorf("path = %q, want %q", got.Path, fragment)
	}

	reason := PinReportReason(report)
	for _, want := range []string{
		"contracts/judge.md", "fragments/laziness-ladder.md",
		run.Ref() + " does not pin", fragment,
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to say %q", reason, want)
		}
	}
}

// TestVerifyPinsReportsAnUnpinnedReferenceWithNoBytesOnDisk is the other half of
// DKT-818's distinction, and the path that still opens this hole with NO repin
// in the story: activation pins the closure it can SEE, and a `packet_includes`
// naming a file no config root holds yet is pinned by nothing. The run
// activates clean, and the reference is unpinned from the first second — before
// and after the file lands.
func TestVerifyPinsReportsAnUnpinnedReferenceWithNoBytesOnDisk(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt821-dev.toml", dkt821WorkflowSrc)
	writeConfigFile(t, configDir, "contracts/judge.md", dkt821ContractB1)
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "the fragment arrives later", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if n := pinCount(t, conn, run.ID, "fragments/laziness-ladder.md"); n != 0 {
		t.Fatalf("premise: the fragment is pinned (%d row(s)) though no root "+
			"held it at activation", n)
	}

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Sound() || report.Unpinned != 1 {
		t.Fatalf("sound = %v, unpinned = %d, want an unsound report naming the "+
			"one unpinned reference", report.Sound(), report.Unpinned)
	}
	if got := report.References[0].Path; got != "" {
		t.Errorf("path = %q, want empty — no instance-config root holds the "+
			"file, and the report must not imply one does", got)
	}
	if reason := PinReportReason(report); !strings.Contains(
		reason, "no instance-config root holds it") {
		t.Errorf("reason = %q, want it to say the file is absent, not merely "+
			"unpinned", reason)
	}

	// The file lands — the pin set is still frozen without it, so the verdict
	// stands and only its remedy changes.
	fragment := writtenAt(t, configDir, "fragments/laziness-ladder.md",
		"the laziness ladder\n")
	report, err = VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Sound() {
		t.Fatal("writing the file made the report sound; the pin set is what " +
			"froze without it, and a file on disk is not a pin")
	}
	if got := report.References[0].Path; got != fragment {
		t.Errorf("path = %q, want %q", got, fragment)
	}
}

// TestARepinTodayLeavesNoUnpinnedReference is the companion the fixture above
// needs: driving a repin on RUN-59's corpus edit no longer produces the wedge,
// because DKT-805 taught repin to pin the closure of the bytes it adopts. The
// two verbs share one walk (unpinnedClosureRefs), and this is the assertion
// that they agree — the disagreement is what DKT-821 was.
func TestARepinTodayLeavesNoUnpinnedReference(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt821-dev.toml", dkt821WorkflowSrc)
	writeConfigFile(t, configDir, "contracts/judge.md", "the judge contract\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "repin closes it", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	writeConfigFile(t, configDir, "contracts/judge.md", dkt821ContractB1)
	writeConfigFile(t, configDir, "fragments/laziness-ladder.md", "the ladder\n")

	_, err = RepinRunWith(conn, run.ID, RepinOptions{
		Reason: "adopting the second edition"}, nowMS)
	testsupport.Must(t, err, "RepinRunWith: %v", err)

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Fatalf("verify-pins refuses a run repin just closed: %s",
			PinReportReason(report))
	}
	if report.Unpinned != 0 || len(report.References) != 0 {
		t.Errorf("references = %+v, want none — repin pinned what the adopted "+
			"bytes require, so there is no hole left to name", report.References)
	}
}

// TestVerifyPinsStaysCleanOnAClosedPinSet is AC3, and it is not vacuous: the
// contract reaches a fragment which reaches a second fragment, and activation
// pinned the whole chain. A closure check that reported those as unpinned would
// fail every healthy run in the fleet.
func TestVerifyPinsStaysCleanOnAClosedPinSet(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt821-dev.toml", dkt821WorkflowSrc)
	writeConfigFile(t, configDir, "contracts/judge.md",
		"---\npacket_includes:\n  - fragments/ladder.md\n---\nthe judge contract\n")
	writeConfigFile(t, configDir, "fragments/ladder.md",
		"---\npacket_includes:\n  - fragments/boundaries.md\n---\nthe ladder\n")
	writeConfigFile(t, configDir, "fragments/boundaries.md", "the boundaries\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "a closed pin set", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The premise: activation pinned the transitive chain, which is the thing
	// the closure check is judged against.
	for _, ref := range []string{
		"contracts/judge.md", "fragments/ladder.md", "fragments/boundaries.md",
	} {
		if n := pinCount(t, conn, run.ID, ref); n != 1 {
			t.Fatalf("premise: %s has %d pin(s), want 1", ref, n)
		}
	}

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Fatalf("a fully closed run reads unsound: %s", PinReportReason(report))
	}
	if report.Unpinned != 0 || len(report.References) != 0 {
		t.Errorf("references = %+v, want none", report.References)
	}
	// Empty, never nil: `--json` emits an array on a clean run so a consumer
	// parses one shape either way.
	if report.References == nil {
		t.Error("references is nil; the wire shape must be an empty array")
	}
}
