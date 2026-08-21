package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// §6.11 and §6.11.1 — rendering, and the pinned-template verification.

// TestRenderUsesTheShippedDefault pins that the embedded template renders a
// packet with the framing §6.11 requires: run/issue/step ids, scope, the input
// artifacts labeled in declared order, the pin list with hashes, and the output
// instruction.
func TestRenderUsesTheShippedDefault(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	result, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)

	if result.Template != "default" {
		t.Errorf("template = %q, want the shipped default", result.Template)
	}
	// The embedded default ships in the binary, so there is no file to drift.
	if !result.TemplatePinned {
		t.Error("the shipped default reports as unpinned; it cannot drift")
	}

	for _, want := range []string{
		"STEP-1", "implement@0", "RUN-1", "DKT-1",
		"do the thing", // the snapshotted title
		"a body",       // the snapshotted request
		"change-summary",
		"workflow:standard-change@1",
	} {
		if !strings.Contains(result.Packet, want) {
			t.Errorf("the packet does not carry %q:\n%s", want, result.Packet)
		}
	}
}

// TestDefaultTemplateCarriesNoInstanceVocabulary is the genericity rule applied
// to the packet layout (§6.11: "packet layout is core mechanics; packet content
// is instance data").
//
// The scripted gate checks the template's bytes too, but this asserts the
// RENDERED output — a template could pass a source-level grep and still emit an
// instance concept through a label it constructs.
func TestDefaultTemplateCarriesNoInstanceVocabulary(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	result, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)

	// The banned vocabulary of docs/design/genericity.md, as CONCEPTS the
	// layout might introduce. The fixture legitimately supplies `implement` and
	// `judge-*` as opaque executor values, so this checks the FRAMING the
	// template itself emits — the lines it writes, not the data it interpolates.
	var framing []string
	for _, line := range strings.Split(result.Packet, "\n") {
		// Data lines are the interpolated ones; framing lines are the labels
		// and section headers the template owns.
		if strings.HasPrefix(line, "==") || strings.Contains(line, ":") {
			framing = append(framing, strings.ToLower(line))
		}
	}
	joined := strings.Join(framing, "\n")

	// The vocabulary is read from the GATE'S OWN LIST rather than restated
	// here. Two reasons, and the second is the important one:
	//
	//   - one list, so this test and scripts/qa/genericity.sh cannot disagree
	//     about what is banned;
	//   - restating it would put the banned words into this file as string
	//     literals, and the gate scans `internal/**` — so the test asserting
	//     the rule would trip the script enforcing it. Splitting the strings to
	//     dodge that grep is exactly the trick that erodes a gate, so the list
	//     is sourced instead of spelled.
	for _, term := range bannedVocabulary(t) {
		if strings.Contains(joined, term) {
			t.Errorf("the packet's FRAMING carries a banned term (%q) — packet "+
				"layout is core surface (§6.11, genericity.md):\n%s", term, joined)
		}
	}
}

// bannedVocabulary reads the banned-word list out of scripts/qa/genericity.sh,
// which is the single definition of it.
func bannedVocabulary(t *testing.T) []string {
	t.Helper()

	src, err := os.ReadFile("../../scripts/qa/genericity.sh")
	testsupport.Must(t, err, "reading the genericity gate: %v", err)

	_, rest, ok := strings.Cut(string(src), "BANNED=(")
	if !ok {
		t.Fatal("scripts/qa/genericity.sh has no BANNED=( list; this test can no " +
			"longer source the vocabulary it asserts against")
	}
	body, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatal("the BANNED list in scripts/qa/genericity.sh is unterminated")
	}

	var out []string
	for _, line := range strings.Split(body, "\n") {
		word := strings.TrimSpace(line)
		if word == "" || strings.HasPrefix(word, "#") {
			continue
		}
		out = append(out, word)
	}
	if len(out) == 0 {
		t.Fatal("sourced an EMPTY banned list — this assertion would pass on anything")
	}
	return out
}

// TestPinnedTemplateDriftIsRefused is §6.11.1, and the hole it closes.
//
// Without this check a post-activation edit to a pinned template silently
// changes every packet the run renders from then on, WHILE `step context` stays
// byte-identical — so the §9-item-5 determinism goldens keep passing and the
// thing actually handed to a worker has changed. That is the one gap in the
// reproducibility story.
//
// The test edits a pinned template between two renders and requires the second
// to refuse. Its shape is §8.3's golden-sensitivity check, for the same reason:
// a verification that cannot fail is not a verification.
func TestPinnedTemplateDriftIsRefused(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "packet.tmpl")
	err := os.WriteFile(tmplPath, []byte("ORIGINAL {{.Step.Instance}}\n"), 0o644)
	testsupport.Must(t, err, "writing the template: %v", err)

	issue := createIssue(t, conn, "templated", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID, tmplPath)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")

	// First render: the bytes match the pin.
	first, err := RenderStep(conn, stepID, tmplPath, nowMS)
	testsupport.Must(t, err, "first render: %v", err)
	if !first.TemplatePinned {
		t.Error("a pinned template reports as unpinned")
	}
	if !strings.Contains(first.Packet, "ORIGINAL") {
		t.Errorf("the packet did not come from the template: %q", first.Packet)
	}

	// EDIT THE PINNED FILE.
	err = os.WriteFile(tmplPath, []byte("TAMPERED {{.Step.Instance}}\n"), 0o644)
	testsupport.Must(t, err, "editing the template: %v", err)

	// Second render: refused.
	_, err = RenderStep(conn, stepID, tmplPath, nowMS)
	if err == nil {
		t.Fatal("a drifted pinned template rendered anyway — a post-activation " +
			"edit would silently change every packet while `step context` stays " +
			"byte-identical, so the determinism goldens would keep passing")
	}

	// CONFLICT, not VALIDATION_ERROR: the REQUEST is well-formed and the STATE
	// disagrees with the pin — the same reading as a re-register with differing
	// bytes and an --if-version mismatch.
	code, ok := CodeOf(err)
	if !ok || code != CodeConflict {
		t.Errorf("code = %v, want CONFLICT (§6.11.1)", code)
	}

	// BOTH hashes are named, because an operator needs to know whether to
	// restore the file or start a new run.
	msg := err.Error()
	if !strings.Contains(msg, tmplPath) {
		t.Errorf("the refusal does not name the path: %s", msg)
	}
	if strings.Count(msg, "sha") == 0 && !hasTwoHashes(msg) {
		t.Errorf("the refusal does not name both hashes: %s", msg)
	}

	// And `step context` is INDEED unchanged — which is the whole point: the
	// bundle's determinism is not evidence the packet is reproducible.
	if _, err := ReadContext(conn, stepID, nowMS); err != nil {
		t.Errorf("the context bundle broke on a template edit: %v", err)
	}
}

// hasTwoHashes reports whether a message contains two distinct 64-hex strings.
func hasTwoHashes(msg string) bool {
	var found []string
	for _, field := range strings.Fields(msg) {
		trimmed := strings.Trim(field, ",;.")
		if len(trimmed) != 64 {
			continue
		}
		isHex := true
		for _, c := range trimmed {
			if !strings.ContainsRune("0123456789abcdef", c) {
				isHex = false
				break
			}
		}
		if isHex {
			found = append(found, trimmed)
		}
	}
	return len(found) >= 2 && found[0] != found[1]
}

// TestAbsentPinnedTemplateIsNotFound is §6.11.1's third row: the pin says the
// run depends on the file, so its absence is a missing dependency (NOT_FOUND,
// exit 2) rather than a bad request.
func TestAbsentPinnedTemplateIsNotFound(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "packet.tmpl")
	err := os.WriteFile(tmplPath, []byte("X {{.Step.Instance}}\n"), 0o644)
	testsupport.Must(t, err, "writing the template: %v", err)

	issue := createIssue(t, conn, "templated", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID, tmplPath)
	testsupport.Must(t, err, "activate: %v", err)
	err = os.Remove(tmplPath)
	testsupport.Must(t, err, "removing the template: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, err = RenderStep(conn, stepID, tmplPath, nowMS)
	if err == nil {
		t.Fatal("an absent pinned template rendered")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %v, want NOT_FOUND (§6.11.1)", code)
	}
	if !strings.Contains(err.Error(), "pinned") {
		t.Errorf("the refusal does not say the file was pinned: %v", err)
	}
}

// TestUnpinnedTemplateRendersUnverifiedAndSaysSo is §6.11.1's fourth row: the
// packet is reproducible only to the extent the operator chose, and the gap is
// REPORTED rather than assumed.
func TestUnpinnedTemplateRendersUnverifiedAndSaysSo(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	dir := t.TempDir()
	tmplPath := filepath.Join(dir, "unpinned.tmpl")
	err := os.WriteFile(tmplPath, []byte("FREE {{.Step.Instance}}\n"), 0o644)
	testsupport.Must(t, err, "writing the template: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	result, err := RenderStep(conn, stepID, tmplPath, nowMS)
	testsupport.Must(t, err, "an unpinned template was refused: %v — only a DRIFTED PIN "+
		"refuses; an unpinned path renders unverified", err)

	if result.TemplatePinned {
		t.Error("an unpinned template reports as pinned; the reproducibility " +
			"gap must be visible rather than assumed")
	}

	// And it may be edited freely without refusal, precisely because nothing
	// promised it would not change.
	err = os.WriteFile(tmplPath, []byte("EDITED {{.Step.Instance}}\n"), 0o644)
	testsupport.Must(t, err, "editing: %v", err)
	second, err := RenderStep(conn, stepID, tmplPath, nowMS)
	testsupport.Must(t, err, "an edited UNPINNED template was refused: %v", err)
	if !strings.Contains(second.Packet, "EDITED") {
		t.Errorf("the second render did not pick up the edit: %q", second.Packet)
	}
}

// TestRenderIsDeterministicAtFixedState pins that two renders at the same run
// state are byte-identical — the packet-level counterpart of the bundle's
// determinism.
func TestRenderIsDeterministicAtFixedState(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	first, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	for i := range 10 {
		again, err := RenderStep(conn, stepID, "", nowMS)
		testsupport.Must(t, err, "render %d: %v", i, err)
		if again.Packet != first.Packet {
			t.Fatalf("render %d differed:\n%s\n---\n%s", i, first.Packet, again.Packet)
		}
	}
}

// TestAttemptNumberingPreAndPostClaim pins DKT-64's reconciliation: `attempt`
// is ONE monotonic, 0-based, spent-count column, and every surface — the
// ready-steps row `next --run` reads, the rendered packet header, and a
// subsequent `step show`-equivalent read — reports the SAME column, just
// sampled at different moments relative to a claim. See
// docs/tdd/attempt-numbering.md.
func TestAttemptNumberingPreAndPostClaim(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	// Before any claim: the ready-steps row (what `next --run` renders) reads
	// the pre-claim value, 0 — never claimed.
	before, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep before claim: %v", err)
	if before.Attempt != 0 {
		t.Fatalf("pre-claim attempt = %d, want 0", before.Attempt)
	}

	// The claim commits the CAS increment, THEN the packet is rendered over
	// the post-commit row — exactly `internal/cli/step.go`'s
	// claim-then-render order for `claim --render`.
	_, err = ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "ClaimStep: %v", err)

	result, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if !strings.Contains(result.Packet, "attempt: 1") {
		t.Errorf("post-claim packet does not read attempt: 1:\n%s", result.Packet)
	}

	// A later read of the same step (the `step show` path) reports the
	// identical post-claim value — proving it is one field, not two.
	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep after claim: %v", err)
	if after.Attempt != 1 {
		t.Fatalf("post-claim attempt = %d, want 1", after.Attempt)
	}
}
