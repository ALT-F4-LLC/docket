package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// DKT-4251: `unmatched` means no trust entry matched. A gate whose entry
// matched but whose repo root no longer resolves could not measure; it records
// `skipped` with an environment reason, which the operator is not reserved.

func TestARepoRootThatNoLongerResolvesIsSkippedNotUnmatched(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	testsupport.Must(t, err, "resolving git: %v", err)
	identity := t.TempDir()
	gone := filepath.Join(t.TempDir(), "removed")
	argv := []string{gitPath, "rev-parse", "HEAD"}

	runner := NewExecRunner(RepoPaths{ExecRoot: gone, Identity: identity})
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "build", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(identity),
	})

	ex, err := runner.Execute(context.Background(), GateSpec{Name: "build"}, StepContext{})
	testsupport.Must(t, err, "Execute: %v", err)
	if len(ex.Results) != 1 {
		t.Fatalf("%d rows, want 1", len(ex.Results))
	}
	row := ex.Results[0]
	if row.Verdict != VerdictSkipped {
		t.Fatalf("verdict = %q, want %q for an unresolvable repo root", row.Verdict, VerdictSkipped)
	}
	if !strings.HasPrefix(row.Reason, "environment: ") || !strings.Contains(row.Reason, "cannot resolve the repo path") {
		t.Errorf("reason = %q, want an environment reason naming the repo path", row.Reason)
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("execution verdict = %q; the gate did not pass and must route as not-pass", ex.Verdict)
	}
	if row.Exit != nil {
		t.Errorf("exit = %v for a gate that never ran", *row.Exit)
	}
}

func TestAnArgvAbsentFromTheRosterStaysUnmatched(t *testing.T) {
	identity := t.TempDir()
	runner := NewExecRunner(RepoPaths{ExecRoot: identity, Identity: identity})
	runner.LoadStore = sandboxTrust(t) // an empty roster

	ex, err := runner.Execute(context.Background(), GateSpec{Name: "build"}, StepContext{})
	testsupport.Must(t, err, "Execute: %v", err)
	if len(ex.Results) != 1 || ex.Results[0].Verdict != VerdictUnmatched {
		t.Fatalf("rows = %+v; an untrusted gate must stay %q", ex.Results, VerdictUnmatched)
	}
}

// The saga half: an engine whose runner root has vanished parks the step as an
// unmeasured gate, not as an unmatched one.
func TestAVanishedRepoRootDoesNotParkAsGateUnmatched(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	testsupport.Must(t, err, "resolving git: %v", err)
	conn := mustDB(t)
	activatedRun(t, conn)
	identity := t.TempDir()
	argv := []string{gitPath, "rev-parse", "HEAD"}
	var entries []trust.Entry
	for _, name := range gateNames {
		entries = append(entries, trust.Entry{
			Name: name, Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
			Repo: mustResolve(identity),
		})
	}
	runner := NewExecRunner(RepoPaths{
		ExecRoot: filepath.Join(t.TempDir(), "removed"), Identity: identity,
	})
	runner.LoadStore = sandboxTrust(t, entries...)
	e := testEngine()
	e.Gates = runner

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	}), "complete")

	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "reading rows: %v", err)
	for _, row := range rows {
		if row.Verdict == db.GateVerdictUnmatched {
			t.Errorf("gate %s recorded unmatched: %q", row.Gate, row.Reason)
		}
	}
	view, err := LoadStepView(conn, stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.ParkClass == db.ParkClassGateUnmatched {
		t.Errorf("parked as %q for a vanished checkout", view.ParkClass)
	}
	if view.ParkClass != db.ParkClassGateSkipped {
		t.Errorf("park class = %q, want %q", view.ParkClass, db.ParkClassGateSkipped)
	}
}
