package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-805 — repin adopted drifted refs but could not ADD newly-referenced
// ones. On RUN-56 an operator-approved repin adopted contract bytes whose
// `packet_includes` reached two fragments the run never snapshotted; repin
// reported full success, and the next dispatch's every judge step refused at
// claim (VALIDATION_ERROR, "not pinned by this run … start a new run") — the
// disposition repin exists to avoid. These tests pin the closed gap: adopting
// bytes adopts their packet closure, in the same transaction, or refuses up
// front naming what cannot be pinned.

// dkt805WorkflowSrc declares one step reading one contract — the pin set at
// activation is that contract plus policy.toml, and nothing else.
const dkt805WorkflowSrc = `
[pipeline]
name = "dkt805-dev"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
after = []
packet = ["contracts/implement.md"]
`

// dkt805ContractB1 is the corpus edit: the adopted contract bytes now declare
// a fragment the run never pinned.
const dkt805ContractB1 = "---\npacket_includes:\n  - fragments/new.md\n---\n" +
	"the implement contract, second edition\n"

// TestRepinPinsTheAdoptedBytesNewlyRequiredRefs is the acceptance criterion
// verbatim: a run pinned against contract bytes B0 is repinned to B1, where B1
// includes a fragment absent from the pin set. The repin must pin the fragment
// in the same transaction, record its own run-repinned event (null old_sha256,
// added: true), and leave the step claimable with the full packet.
func TestRepinPinsTheAdoptedBytesNewlyRequiredRefs(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt805-dev.toml", dkt805WorkflowSrc)
	writeConfigFile(t, configDir, "contracts/implement.md", "the implement contract\n")
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "closure subject", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	oldSHA := pinSHA(t, conn, run.ID, "contracts/implement.md")
	if n := pinCount(t, conn, run.ID, "fragments/new.md"); n != 0 {
		t.Fatalf("premise: fragments/new.md is already pinned (%d row(s)); B0 "+
			"declares no includes", n)
	}

	// The corpus edit: B1 replaces the contract and creates the fragment it
	// includes — RUN-56's dee7670, a file that did not exist at activation.
	writeConfigFile(t, configDir, "contracts/implement.md", dkt805ContractB1)
	fragmentBody := "the new fragment\n"
	writeConfigFile(t, configDir, "fragments/new.md", fragmentBody)

	outcome, err := RepinRunWith(conn, run.ID, RepinOptions{
		Reason: "dee7670 introduced the fragment"}, nowMS)
	testsupport.Must(t, err, "RepinRunWith: %v", err)

	if len(outcome.Repinned) != 1 || outcome.Repinned[0].Ref != "contracts/implement.md" {
		t.Fatalf("repinned %+v, want exactly contracts/implement.md", outcome.Repinned)
	}
	if len(outcome.Added) != 1 {
		t.Fatalf("added %d pin(s), want 1: %+v", len(outcome.Added), outcome.Added)
	}
	wantSHA := workflow.SHA256([]byte(fragmentBody))
	a := outcome.Added[0]
	if a.Ref != "fragments/new.md" || a.Kind != db.PinKindFile ||
		a.OldSHA256 != "" || a.NewSHA256 != wantSHA || !a.Added {
		t.Errorf("addition = %+v, want fragments/new.md (nothing) -> %s, marked added",
			a, wantSHA)
	}

	// The pin row exists, at the fragment's current disk hash, and the run is
	// SOUND — repin never reports success on a run verify-pins still fails.
	if got := pinSHA(t, conn, run.ID, "fragments/new.md"); got != wantSHA {
		t.Errorf("pinned fragments/new.md at %s, want %s", got, wantSHA)
	}
	after, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !after.Sound() {
		t.Errorf("the run is unsound after the repin: %s", PinReportReason(after))
	}

	// AC-3: the addition carries its own event — the sha, the reason, a null
	// old_sha256, `added: true`, and what requires the ref.
	var added map[string]any
	for _, ev := range repinEvents(t, conn, run.ID) {
		if ev["added"] == true {
			added = ev
		}
	}
	if added == nil {
		t.Fatal("no run-repinned event carries added: true")
	}
	if added["ref"] != "fragments/new.md" || added["new_sha256"] != wantSHA ||
		added["old_sha256"] != nil ||
		added["reason"] != "dee7670 introduced the fragment" {
		t.Errorf("added event = %v, want fragments/new.md at %s with a null "+
			"old_sha256 and the operator's reason", added, wantSHA)
	}
	required, _ := added["required_by"].([]any)
	if len(required) != 1 || required[0] != "implement@0" {
		t.Errorf("required_by = %v, want [implement@0]", added["required_by"])
	}

	// AC-5's second half: the step is CLAIMABLE, and the rendered packet
	// carries both the adopted contract and the fragment it introduced —
	// the exact render RUN-56's dispatch died on.
	stepID := stepIDByInstance(t, conn, "implement@0")
	result, packet, err := NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "")
	testsupport.Must(t, err, "claim after the repin: %v", err)
	if result.Token == "" {
		t.Error("the claim returned no token")
	}
	if packet == nil ||
		!strings.Contains(packet.Packet, "the implement contract, second edition") ||
		!strings.Contains(packet.Packet, "the new fragment") {
		t.Errorf("the packet does not carry the adopted contract and its fragment: %+v",
			packet)
	}

	// The event trail still reconstructs the drifted ref's move: the ordinary
	// repin event carries old -> new for the contract.
	if oldSHA == outcome.Repinned[0].NewSHA256 {
		t.Errorf("premise: B1 hashed identically to B0 (%s)", oldSHA)
	}
}

// TestRepinRefusesWhenANewlyRequiredRefHasNoBytes is the up-front refusal: the
// adopted bytes require a fragment that resolves NOWHERE, so there is nothing
// to pin and proceeding would trade the CONFLICT for a render-time
// VALIDATION_ERROR. The refusal writes nothing, names the ref and its readers
// and the dispositions, and restoring the file is a working recovery.
func TestRepinRefusesWhenANewlyRequiredRefHasNoBytes(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/dkt805-dev.toml", dkt805WorkflowSrc)
	contractB0 := "the implement contract\n"
	writeConfigFile(t, configDir, "contracts/implement.md", contractB0)
	writeConfigFile(t, configDir, "policy.toml", "opaque = \"instance policy\"\n")

	issue := createIssue(t, conn, "closure subject", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	oldSHA := pinSHA(t, conn, run.ID, "contracts/implement.md")

	// The corpus edit, MINUS the fragment: B1 includes a file nobody wrote.
	writeConfigFile(t, configDir, "contracts/implement.md", dkt805ContractB1)

	_, err = RepinRunWith(conn, run.ID, RepinOptions{Reason: "adopt dee7670"}, nowMS)
	if err == nil {
		t.Fatal("a repin whose adopted bytes require an unresolvable ref succeeded")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %q, want %q", code, CodeNotFound)
	}
	// AC-4: the refusal names the ref, what reads it, and the dispositions.
	for _, want := range []string{
		"fragments/new.md", "implement@0", "restore the file", "abandon the run",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err.Error(), want)
		}
	}

	// The refusal wrote NOTHING: the contract pin still records B0, no pin row
	// names the fragment, and no run-repinned event exists.
	if got := pinSHA(t, conn, run.ID, "contracts/implement.md"); got != oldSHA {
		t.Errorf("the refused repin moved the contract pin: %s, want %s", got, oldSHA)
	}
	if n := pinCount(t, conn, run.ID, "fragments/new.md"); n != 0 {
		t.Errorf("the refused repin left %d pin row(s) for the fragment", n)
	}
	if evs := repinEvents(t, conn, run.ID); len(evs) != 0 {
		t.Errorf("the refused repin recorded %d event(s): %v", len(evs), evs)
	}

	// AC-5's second half, under the refusal outcome: the named disposition
	// works. Restoring B0 puts the run back exactly where it was, and the
	// step claims under the original agreement.
	writeConfigFile(t, configDir, "contracts/implement.md", contractB0)
	stepID := stepIDByInstance(t, conn, "implement@0")
	result, packet, err := NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "")
	testsupport.Must(t, err, "claim after restoring B0: %v", err)
	if result.Token == "" {
		t.Error("the claim returned no token")
	}
	if packet == nil || !strings.Contains(packet.Packet, "the implement contract") {
		t.Errorf("the packet does not carry the restored contract: %+v", packet)
	}
}
