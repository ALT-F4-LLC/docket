package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// DKT-582 at the CLI seam: `--drop` and `--drop-unresolvable` have to be
// PARSED and have to REACH the engine's disposition. The behaviour itself is
// pinned in internal/engine; what can only break here is the wiring.

// repinViaCLI runs the command's own handler over the command's OWN flag set —
// the definitions init() registers, not a test's re-declaration of them — so a
// renamed flag or a mistyped Get* call fails here.
func repinViaCLI(t *testing.T, conn *sql.DB, ref string, set map[string]string) error {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().AddFlagSet(runRepinCmd.Flags())
	// The flag objects are shared with the package-level command, so each call
	// starts from the declared defaults rather than the previous call's values.
	resetRepinFlags(t)
	t.Cleanup(func() { resetRepinFlags(t) })
	for name, value := range set {
		testsupport.Must(t, cmd.Flags().Set(name, value), "--%s=%s", name, value)
	}
	w, _ := bufWriter(false)
	return runRunRepin(cmd, ref, w)
}

// resetRepinFlags puts every `run repin` flag back to its declared default.
func resetRepinFlags(t *testing.T) {
	t.Helper()
	runRepinCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			testsupport.Must(t, sv.Replace(nil), "resetting --%s", f.Name)
		} else {
			testsupport.Must(t, f.Value.Set(f.DefValue), "resetting --%s", f.Name)
		}
		f.Changed = false
	})
}

// TestRunRepinDropFlagsReachTheEngine: the SAME run, the SAME deleted ref, and
// two different refusals — NOT_FOUND without the flag (repin has no bytes to
// adopt) and CONFLICT with it (the drop was accepted, and the run then failed
// the quiescence guard for having no non-terminal step). The change in code is
// the evidence that the flag crossed the seam.
func TestRunRepinDropFlagsReachTheEngine(t *testing.T) {
	conn := newTestDB(t)
	run, _ := driftedRun(t, conn, model.RunActive)

	// The corpus commit these flags exist for: the pinned file is DELETED, not
	// edited.
	testsupport.Must(t, os.Remove(filepath.Join(os.Getenv("DOCKET_PATH"),
		"config", "contracts", "synthesize-findings.md")), "deleting the contract")

	err := repinViaCLI(t, conn, run.Ref(), map[string]string{
		"reason": "cc92e38 deleted it"})
	if err == nil {
		t.Fatal("repin proceeded with a pinned ref deleted and no disposition")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("code = %s, want %s: %v", got, output.ErrNotFound, err)
	}
	if !strings.Contains(err.Error(), "--drop") {
		t.Errorf("the refusal %q does not point at the disposition that would "+
			"resolve it", err)
	}

	err = repinViaCLI(t, conn, run.Ref(), map[string]string{
		"reason": "cc92e38 deleted it", "drop-unresolvable": "true"})
	if err == nil {
		t.Fatal("repin proceeded on a run with no non-terminal step")
	}
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %s, want %s — the drop must have been accepted and the "+
			"refusal must come from the quiescence guard instead: %v",
			got, output.ErrConflict, err)
	}

	// And --drop takes a ref, repeatably, reaching the same disposition.
	err = repinViaCLI(t, conn, run.Ref(), map[string]string{
		"reason": "cc92e38 deleted it", "drop": "contracts/synthesize-findings.md"})
	if err == nil {
		t.Fatal("repin proceeded on a run with no non-terminal step")
	}
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %s, want %s: %v", got, output.ErrConflict, err)
	}
}

// TestRunRepinHelpDocumentsTheDisposition: the Long text is where the refusal
// semantics are stated, and DKT-582 changed them — NOT_FOUND is now refused
// UNLESS the ref is unread and covered by a flag.
func TestRunRepinHelpDocumentsTheDisposition(t *testing.T) {
	for _, want := range []string{
		"--drop REF", "--drop-unresolvable", "UNLESS", "NULL new_sha256",
	} {
		if !strings.Contains(runRepinCmd.Long, want) {
			t.Errorf("`run repin --help` does not mention %q", want)
		}
	}
	for _, name := range []string{"drop", "drop-unresolvable"} {
		if runRepinCmd.Flags().Lookup(name) == nil {
			t.Errorf("`run repin` declares no --%s flag", name)
		}
	}
}

// TestRenderRepinOutcomeStatesDrops: a dropped pin is a different fact from a
// moved one, and the human output has to say so rather than silently omitting
// a ref the operator asked about.
func TestRenderRepinOutcomeStatesDrops(t *testing.T) {
	got := renderRepinOutcome(&engine.RepinOutcome{
		Run: "RUN-42",
		Repinned: []engine.RepinChange{{
			Kind: "file", Ref: "contracts/implement.md",
			OldSHA256: "aaaaaaaaaaaa", NewSHA256: "bbbbbbbbbbbb",
		}},
		Dropped: []engine.RepinChange{{
			Kind: "file", Ref: "contracts/test-infra.md",
			OldSHA256: "cccccccccccc", Dropped: true,
		}},
		Unchanged: 3,
	})
	for _, want := range []string{
		"contracts/implement.md", "contracts/test-infra.md", "dropped", "1 dropped",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the outcome does not state %q:\n%s", want, got)
		}
	}
}
