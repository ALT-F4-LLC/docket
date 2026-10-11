package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"

	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// §9 item 6 — the trust proof (TDD docs/tdd/gates-trust.md §9.2, threat T1).
//
// engine-spec §9 item 6, verbatim:
//
//	Trust: a cloned malicious repo cannot cause execution without a prior local
//	`trust add` (proof includes fenced-command harvesting); unmatched commands
//	are reported, never run.
//
// The witness is a SENTINEL FILE. "Nothing executed" is only provable from
// outside the engine: a row saying `unmatched` is the engine's own account of
// itself, while a file that does not exist is the filesystem's.

// witnessCommand returns an argv that creates `sentinel` when it runs, and the
// path to check.
//
// It uses /usr/bin/touch rather than a shell one-liner deliberately: this stage
// forbids invoking a command interpreter anywhere, and a test that reached for
// `sh -c` to build its own witness would be proving the opposite of the point.
func witnessCommand(t *testing.T, dir, name string) (argv []string, sentinel string) {
	t.Helper()
	sentinel = filepath.Join(dir, name)
	return []string{"/usr/bin/touch", sentinel}, sentinel
}

func sentinelExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestMaliciousCloneExecutesNothing is §9.2's script, step by step.
//
// Steps 1-5: a repo whose gate is a witness command, NO trust entries, and the
// assertion that the sentinel does not exist.
// Steps 6-7: `trust add` the named gate only; its sentinel appears and nothing
// else does — trust is per-command, not per-repo-blanket.
// Step 8: the clone half — trust granted in the ORIGINAL does not fire in a
// COPY at a different path, which is §3.4's argument mechanized.
func TestMaliciousCloneExecutesNothing(t *testing.T) {
	repoRoot := t.TempDir()
	namedArgv, namedSentinel := witnessCommand(t, repoRoot, "named-ran")
	fenceArgv, fenceSentinel := witnessCommand(t, repoRoot, "fence-ran")

	// ---- Steps 2-5: NO TRUST ENTRIES. --------------------------------------
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t) // the empty allowlist

	named, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the named gate: %v", err)
	fence, err := runner.Execute(context.Background(),
		GateSpec{Name: "checks", Source: "fence:checks",
			Commands: []string{strings.Join(fenceArgv, " ")}}, StepContext{})
	testsupport.Must(t, err, "running the fence gate: %v", err)

	// THE CENTRAL ASSERTION: neither sentinel exists. Nothing ran.
	if sentinelExists(t, namedSentinel) {
		t.Error("the named gate EXECUTED with no trust entry")
	}
	if sentinelExists(t, fenceSentinel) {
		t.Error("the fenced command EXECUTED with no trust entry")
	}

	for _, ex := range []GateExecution{named, fence} {
		if len(ex.Results) != 1 {
			t.Fatalf("got %d results, want 1: %+v", len(ex.Results), ex.Results)
		}
		r := ex.Results[0]
		if r.Verdict != VerdictUnmatched {
			t.Errorf("verdict = %q, want %q", r.Verdict, VerdictUnmatched)
		}
		// NULL, not [] and 0: a zero exit on a gate that never ran is exactly
		// the confusion the forgery threat exists to prevent.
		if r.Argv != nil {
			t.Errorf("argv = %v on an unmatched gate, want nil", r.Argv)
		}
		if r.Exit != nil {
			t.Errorf("exit = %v on an unmatched gate, want nil", r.Exit)
		}
		if r.Reason == "" {
			t.Error("an unmatched gate carries no reason")
		}
		// N3: unmatched is a FAILURE for routing, never a pass.
		if ex.Verdict != VerdictFail {
			t.Errorf("gate verdict = %q, want %q — an unmatched gate fails the step",
				ex.Verdict, VerdictFail)
		}
	}

	// ---- Steps 6-7: trust the NAMED gate only, in THIS repo. ---------------
	identity := mustResolve(repoRoot)
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "tests", Argv: namedArgv,
		ArgvSHA256: trust.ArgvSHA256(namedArgv), Repo: identity,
	})

	_, err = runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the trusted gate: %v", err)
	if !sentinelExists(t, namedSentinel) {
		t.Error("the trusted named gate did not execute")
	}

	fence2, err := runner.Execute(context.Background(),
		GateSpec{Name: "checks", Source: "fence:checks",
			Commands: []string{strings.Join(fenceArgv, " ")}}, StepContext{})
	testsupport.Must(t, err, "running the fence gate: %v", err)

	// TRUST IS PER-COMMAND, NOT PER-REPO-BLANKET. Approving `tests` granted
	// the fenced command nothing.
	if sentinelExists(t, fenceSentinel) {
		t.Error("the fenced command executed under the NAMED gate's entry; " +
			"trust is per-command, not a blanket over the repo")
	}
	if fence2.Results[0].Verdict != VerdictUnmatched {
		t.Errorf("the fenced command is %q, want %q",
			fence2.Results[0].Verdict, VerdictUnmatched)
	}

	// ---- Step 8: THE CLONE HALF. -------------------------------------------
	//
	// A second checkout at a DIFFERENT path. The entry above is bound to the
	// original, so it does not match here — §3.4's whole argument, and the
	// reason the binding is the absolute filesystem path rather than a git
	// remote or a root commit, both of which a clone carries with it.
	cloneRoot := t.TempDir()
	cloneArgv, cloneSentinel := witnessCommand(t, cloneRoot, "clone-ran")

	clone := NewExecRunner(testRepoPaths(cloneRoot))
	clone.LoadStore = sandboxTrust(t, trust.Entry{
		// The SAME gate name and the same shape of command, trusted in the
		// ORIGINAL repo — exactly what a hostile fork would rely on.
		Name: "tests", Argv: cloneArgv,
		ArgvSHA256: trust.ArgvSHA256(cloneArgv), Repo: identity,
	})

	cloned, err := clone.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the gate in the clone: %v", err)
	if sentinelExists(t, cloneSentinel) {
		t.Error("a gate EXECUTED in a clone under an entry bound to another repo")
	}
	if cloned.Results[0].Verdict != VerdictUnmatched {
		t.Errorf("the clone's gate is %q, want %q",
			cloned.Results[0].Verdict, VerdictUnmatched)
	}
	// §3.4's diagnostic: the reason names the repo the entry IS bound to, so
	// an operator hunting a moved repo is told where to look rather than left
	// to guess between four causes of `unmatched`.
	if !strings.Contains(cloned.Results[0].Reason, identity) {
		t.Errorf("the clone's unmatched reason does not name the bound repo: %q",
			cloned.Results[0].Reason)
	}
}

// TestGlobalEntryMatchesAnyRepo is the converse of the clone case, so the
// binding check is not simply refusing everything: an entry the operator made
// global with an explicit flag DOES fire in a second checkout.
func TestGlobalEntryMatchesAnyRepo(t *testing.T) {
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "global-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})

	_, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the global gate: %v", err)
	if !sentinelExists(t, sentinel) {
		t.Error("a global entry did not fire; --global must authorize any repo")
	}
}

// TestRunFenceHashTamperIsRefused is §7.3 step 3: a `run_fences` row altered
// after activation is NOT executed.
//
// S3's snapshot closes "the issue body cannot inject"; this closes the narrower
// "the stored row cannot be swapped". A direct database write is outside the
// trust boundary, but it costs one hash to detect and a mismatch is refused
// rather than run.
func TestRunFenceHashTamperIsRefused(t *testing.T) {
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "tampered-ran")
	command := strings.Join(argv, " ")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "checks", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	// The command is TRUSTED — so if it does not run, the refusal is the hash
	// check and nothing else.
	ex, err := runner.Execute(context.Background(), GateSpec{
		Name: "checks", Source: "fence:checks",
		Commands:      []string{command},
		CommandHashes: []string{"0000000000000000000000000000000000000000000000000000000000000000"},
	}, StepContext{})
	testsupport.Must(t, err, "running the tampered gate: %v", err)

	if sentinelExists(t, sentinel) {
		t.Error("a command whose stored hash did not match EXECUTED")
	}
	if ex.Results[0].Verdict != VerdictUnmatched {
		t.Errorf("verdict = %q, want %q", ex.Results[0].Verdict, VerdictUnmatched)
	}
	if !strings.Contains(ex.Results[0].Reason, "hash") {
		t.Errorf("the reason does not name the tamper: %q", ex.Results[0].Reason)
	}

	// And the same command with its CORRECT hash runs, so the check is not
	// refusing everything.
	sum := trustHashOf(command)
	_, err = runner.Execute(context.Background(), GateSpec{
		Name: "checks", Source: "fence:checks",
		Commands: []string{command}, CommandHashes: []string{sum},
	}, StepContext{})
	testsupport.Must(t, err, "running the untampered gate: %v", err)
	if !sentinelExists(t, sentinel) {
		t.Error("a command with a matching hash did not execute")
	}
}

// TestPerLineFenceMatchingIsIndependent is §7.3 step 4: a fence block with
// several lines produces one result PER LINE, each its own decision.
//
// The anti-pattern this rules out is matching the block as a unit, or running
// the matched lines and silently skipping the rest — either would let an
// attacker append a line to a body that a prefix entry happens to cover.
func TestPerLineFenceMatchingIsIndependent(t *testing.T) {
	repoRoot := t.TempDir()
	okArgv, okSentinel := witnessCommand(t, repoRoot, "line-ok")
	badArgv, badSentinel := witnessCommand(t, repoRoot, "line-bad")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "checks", Argv: okArgv, ArgvSHA256: trust.ArgvSHA256(okArgv),
		Repo: mustResolve(repoRoot),
	})

	ex, err := runner.Execute(context.Background(), GateSpec{
		Name: "checks", Source: "fence:checks",
		Commands: []string{strings.Join(okArgv, " "), strings.Join(badArgv, " ")},
	}, StepContext{})
	testsupport.Must(t, err, "running the mixed block: %v", err)

	if len(ex.Results) != 2 {
		t.Fatalf("got %d results for a two-line block, want 2 — each line is "+
			"its own decision with its own record", len(ex.Results))
	}
	if !sentinelExists(t, okSentinel) {
		t.Error("the trusted line did not run")
	}
	if sentinelExists(t, badSentinel) {
		t.Error("the untrusted line RAN; each line must match independently")
	}
	// The gate fails because one line was unmatched (N3).
	if ex.Verdict != VerdictFail {
		t.Errorf("gate verdict = %q, want %q — an unmatched line fails the gate",
			ex.Verdict, VerdictFail)
	}
}

// TestTrustFileRewriteMidGateDoesNotChangeWhatRuns is T4: the store is read
// ONCE into an immutable snapshot, and the matched entry's OWN argv is what
// executes.
//
// There is no window because there are not two reads of two different things:
// matching does not produce a PERMISSION that is later applied to a command
// read from elsewhere, it produces THE ARGV ITSELF.
func TestTrustFileRewriteMidGateDoesNotChangeWhatRuns(t *testing.T) {
	repoRoot := t.TempDir()
	approved, approvedSentinel := witnessCommand(t, repoRoot, "approved-ran")
	swapped, swappedSentinel := witnessCommand(t, repoRoot, "swapped-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))

	// The loader SWAPS the entry's argv on every call, simulating a concurrent
	// rewrite of the trust file between the check and the spawn.
	calls := 0
	runner.LoadStore = func() (*trust.Store, error) {
		calls++
		argv := approved
		if calls > 1 {
			argv = swapped
		}
		return &trust.Store{
			Version: trust.FormatVersion,
			Entries: []trust.Entry{{
				Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
				Repo: mustResolve(repoRoot),
			}},
		}, nil
	}

	_, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the gate: %v", err)

	if !sentinelExists(t, approvedSentinel) {
		t.Error("the argv from the snapshot did not run")
	}
	if sentinelExists(t, swappedSentinel) {
		t.Error("a SECOND read of the trust store changed what executed; the " +
			"store must be read once into an immutable snapshot (M1)")
	}
	if calls != 1 {
		t.Errorf("the runner read the trust store %d times for one gate, want 1 — "+
			"a second read is the TOCTOU window", calls)
	}
}

// trustHashOf is the SHA-256 a harvested command carries — the same hash
// activation stored, computed the same way §7.3's verification recomputes it.
func trustHashOf(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}

// TestNetworkAwareReasonAnnotatesOnlyFailingDeclaringGates is the
// diagnosis half.
//
// The defect it addresses is not a wrong verdict — it is an OPAQUE one. A
// gate needing the network runs inside whichever executor claimed the step,
// DNS-fails there, and the failure lands two process layers down, after the
// step is recorded, where nobody sees it. Core cannot know why a command
// exited non-zero, so it does not guess; it surfaces the fact the operator
// declared, which is what makes reachability the first question asked.
func TestNetworkAwareReasonAnnotatesOnlyFailingDeclaringGates(t *testing.T) {
	declaring := &trust.Entry{Name: "vuln-scan", Network: []string{"vuln.go.dev"}}
	plain := &trust.Entry{Name: "tests"}

	t.Run("a failing declaring gate is annotated with its hosts", func(t *testing.T) {
		got := networkAwareReason("exit 1", declaring, VerdictFail)
		if !strings.Contains(got, "vuln.go.dev") {
			t.Errorf("reason %q does not name the declared host", got)
		}
		if !strings.Contains(got, "exit 1") {
			t.Errorf("reason %q dropped the original reason", got)
		}
	})

	t.Run("a PASSING declaring gate is untouched", func(t *testing.T) {
		if got := networkAwareReason("", declaring, VerdictPass); got != "" {
			t.Errorf("reason = %q, want it untouched — a gate that reached its "+
				"hosts has nothing to explain", got)
		}
	})

	t.Run("a failing gate that declared nothing is untouched", func(t *testing.T) {
		if got := networkAwareReason("exit 1", plain, VerdictFail); got != "exit 1" {
			t.Errorf("reason = %q, want %q — no declaration, no annotation",
				got, "exit 1")
		}
	})

	t.Run("an empty reason still gets the note", func(t *testing.T) {
		got := networkAwareReason("", declaring, VerdictFail)
		if !strings.Contains(got, "vuln.go.dev") {
			t.Errorf("reason %q lost the note when there was no prior reason", got)
		}
		if strings.HasPrefix(got, " — ") {
			t.Errorf("reason %q has a dangling separator", got)
		}
	})

	t.Run("a nil entry is safe", func(t *testing.T) {
		if got := networkAwareReason("exit 1", nil, VerdictFail); got != "exit 1" {
			t.Errorf("reason = %q on a nil entry, want it untouched", got)
		}
	})
}

// TestStubEntryMarksEveryResultRowHollow is DKT-265's execution half.
//
// RUN-17's GIT-50 recorded build, secret-scan, and tests all passing, every one
// of them an echo stub; RUN-19's secret-scan and ac-commands passed via
// /usr/bin/true. Nothing downstream could tell those rows from a scanner that
// ran and found nothing.
//
// The test asserts the two halves that make the marker worth having: a stub
// entry still EXECUTES normally (the declaration changes no behavior, so a
// workflow's shape is exercised exactly as before), and the row it produces
// carries the declaration.
func TestStubEntryMarksEveryResultRowHollow(t *testing.T) {
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "stub-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "secret-scan", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true, Stub: true,
	})

	got, err := runner.Execute(context.Background(),
		GateSpec{Name: "secret-scan"}, StepContext{})
	testsupport.Must(t, err, "running the stub gate: %v", err)

	// A stub is a real spawn of a real (trivial) command. Declaring one must
	// not turn into declining to run it, or a repo using stubs to exercise a
	// workflow's shape would stop exercising the spawn path at all.
	if !sentinelExists(t, sentinel) {
		t.Error("the stub entry did not execute; `stub` is a declaration about " +
			"ASSURANCE, and must change nothing about how the command runs")
	}
	if got.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q — a stub that exits 0 passes like any "+
			"other command", got.Verdict, VerdictPass)
	}

	if len(got.Results) == 0 {
		t.Fatal("the gate produced no result rows")
	}
	for i, r := range got.Results {
		if !r.StubEntry {
			t.Errorf("result[%d] does not carry StubEntry; a reviewer reading "+
				"\"secret-scan: pass\" reasonably concludes a secret scan "+
				"happened, and this field is the only thing that can say "+
				"otherwise", i)
		}
	}
}

// TestNonStubEntryMarksNothingHollow is the other direction, and it is the one
// that matters for trust in the marker.
//
// A field that were set unconditionally would read as "every gate is hollow",
// which is exactly as uninformative as the state DKT-265 describes — just
// inverted. The marker is only worth anything if an unmarked entry produces
// unmarked rows.
func TestNonStubEntryMarksNothingHollow(t *testing.T) {
	repoRoot := t.TempDir()
	argv, _ := witnessCommand(t, repoRoot, "real-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "secret-scan", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})

	got, err := runner.Execute(context.Background(),
		GateSpec{Name: "secret-scan"}, StepContext{})
	testsupport.Must(t, err, "running the gate: %v", err)

	if len(got.Results) == 0 {
		t.Fatal("the gate produced no result rows")
	}
	for i, r := range got.Results {
		if r.StubEntry {
			t.Errorf("result[%d] is marked hollow but its entry declared no "+
				"stub; a marker that is always set says nothing", i)
		}
	}
}

// pinScriptHeader precedes the byte a pin test rewrites. The byte sits in a
// comment, so the rewritten script still runs and still plants its marker:
// only the content pin can stop it.
const pinScriptHeader = "#!/bin/sh\n# pin "

// pinnedScript writes an executable shell script outside the repo whose body
// follows a one-byte comment, and returns its path with its SHA-256.
func pinnedScript(t *testing.T, body string) (path, sum string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "gate.sh")
	content := pinScriptHeader + "A\n" + body
	writeErr := os.WriteFile(path, []byte(content), 0o755)
	testsupport.Must(t, writeErr, "writing the pinned script: %v", writeErr)
	return path, trustHashOf(content)
}

// rewritePinByte flips the comment byte in place, the one-byte change a writer
// to the pinned file makes.
func rewritePinByte(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	testsupport.Must(t, err, "opening the pinned script: %v", err)
	defer f.Close()
	_, err = f.WriteAt([]byte("B"), int64(len(pinScriptHeader)))
	testsupport.Must(t, err, "rewriting the pinned script: %v", err)
}

func pinnedEntry(repoRoot, script, sum string) trust.Entry {
	argv := []string{script}
	return trust.Entry{
		Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Argv0SHA256: sum, Repo: mustResolve(repoRoot),
	}
}

// waitBlockedOnTreeLock returns once some goroutine is waiting for the
// in-process tree mutex, so a rewrite the caller makes next lands after the
// gate's Lookup and inside its lock wait.
func waitBlockedOnTreeLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "engine.lockMutexBefore") {
			return
		}
		time.Sleep(treeLockPollInterval)
	}
	t.Fatal("the gate never reached the tree-lock wait")
}

// pinGateSpec builds the gate that runs a pinned script, so each pin re-check
// scenario runs once as a named gate and once as a fence gate: the two wire
// their re-check through separate closures.
type pinGateSpec func(script string) GateSpec

// namedPinGate runs the entry's own argv.
func namedPinGate(string) GateSpec { return GateSpec{Name: "tests"} }

// fencePinGate harvests one line invoking the script by its absolute path.
func fencePinGate(script string) GateSpec {
	return GateSpec{Name: "tests", Source: "fence:tests", Commands: []string{script}}
}

// runTreeGateAcrossHeldLock runs a pinned tree gate on the shared checkout
// while the test holds the tree lock, calls during() once the gate is blocked
// on it, then releases the lock and returns what the gate recorded.
func runTreeGateAcrossHeldLock(t *testing.T, repoRoot string, entry trust.Entry, spec GateSpec, during func()) GateExecution {
	t.Helper()
	entry.Tree = true
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, entry)

	lock, err := acquireTreeLock(runner.LockPath, time.Minute)
	testsupport.Must(t, err, "holding the tree lock: %v", err)

	type outcome struct {
		ex  GateExecution
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ex, err := runner.Execute(context.Background(), spec, StepContext{})
		done <- outcome{ex, err}
	}()

	waitBlockedOnTreeLock(t)
	during()
	lock.release()

	got := <-done
	testsupport.Must(t, got.err, "running the tree gate: %v", got.err)
	return got.ex
}

// TestGatePinRecheckAfterTreeLock is AB-1: the pinned script is rewritten
// while the gate waits for the tree lock, after its Lookup already matched.
// The runner re-verifies the pin once the lock is held and spawns nothing.
func TestGatePinRecheckAfterTreeLock(t *testing.T) {
	checkPinRecheckAfterTreeLock(t, namedPinGate)
}

// TestFenceGatePinRecheckAfterTreeLock is AB-1 for a harvested fence line.
func TestFenceGatePinRecheckAfterTreeLock(t *testing.T) {
	checkPinRecheckAfterTreeLock(t, fencePinGate)
}

func checkPinRecheckAfterTreeLock(t *testing.T, gateFor pinGateSpec) {
	repoRoot := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script, pinned := pinnedScript(t, "/usr/bin/touch "+marker+"\n")

	ex := runTreeGateAcrossHeldLock(t, repoRoot, pinnedEntry(repoRoot, script, pinned),
		gateFor(script), func() { rewritePinByte(t, script) })

	if sentinelExists(t, marker) {
		t.Error("the rewritten script RAN after the tree lock was released")
	}
	if len(ex.Results) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(ex.Results), ex.Results)
	}
	row := ex.Results[0]
	if row.Verdict != VerdictUnmatched || ex.Verdict != VerdictFail {
		t.Errorf("row verdict %q, gate verdict %q; want %q and %q",
			row.Verdict, ex.Verdict, VerdictUnmatched, VerdictFail)
	}
	if row.Exit != nil {
		t.Errorf("exit = %d on a gate that spawned nothing, want nil", *row.Exit)
	}
	// A Lookup-time refusal carries no entry; this row must come from the
	// re-check that follows the lock, or the test lost its race.
	if row.TrustEntry != "tests" {
		t.Errorf("the row names entry %q, want %q: the refusal did not come "+
			"from the post-lock re-check", row.TrustEntry, "tests")
	}
	content, err := os.ReadFile(script)
	testsupport.Must(t, err, "reading the rewritten script: %v", err)
	resolved, err := filepath.EvalSymlinks(script)
	testsupport.Must(t, err, "resolving the script path: %v", err)
	for _, want := range []string{resolved, pinned, trustHashOf(string(content))} {
		if !strings.Contains(row.Reason, want) {
			t.Errorf("the reason does not name %s: %q", want, row.Reason)
		}
	}
}

// TestGatePinRecheckUnchangedScriptRuns is the benign half: a pinned tree gate
// whose script is untouched across a contended lock still runs.
func TestGatePinRecheckUnchangedScriptRuns(t *testing.T) {
	checkPinRecheckUnchangedScriptRuns(t, namedPinGate)
}

// TestFenceGatePinRecheckUnchangedScriptRuns is the benign half for a fence
// line: its re-check must still match the line's own argv.
func TestFenceGatePinRecheckUnchangedScriptRuns(t *testing.T) {
	checkPinRecheckUnchangedScriptRuns(t, fencePinGate)
}

func checkPinRecheckUnchangedScriptRuns(t *testing.T, gateFor pinGateSpec) {
	repoRoot := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script, pinned := pinnedScript(t, "/usr/bin/touch "+marker+"\n")

	ex := runTreeGateAcrossHeldLock(t, repoRoot, pinnedEntry(repoRoot, script, pinned),
		gateFor(script), func() {})

	if !sentinelExists(t, marker) {
		t.Errorf("the unchanged pinned script did not run: %+v", ex.Results)
	}
	if len(ex.Results) != 1 || ex.Results[0].Verdict != VerdictPass {
		t.Errorf("rows = %+v, want one %q row", ex.Results, VerdictPass)
	}
	if ex.Verdict != VerdictPass {
		t.Errorf("gate verdict = %q, want %q: %+v", ex.Verdict, VerdictPass, ex.Results)
	}
}

// TestGatePinRecheckBeforeEachAttempt is AB-2: a flaky pinned script whose
// first attempt rewrites one byte of itself and fails. The re-attempt is not
// spawned, and its ordinal records the refusal.
func TestGatePinRecheckBeforeEachAttempt(t *testing.T) {
	checkPinRecheckBeforeEachAttempt(t, namedPinGate)
}

// TestFenceGatePinRecheckBeforeEachAttempt is AB-2 for a harvested fence line.
func TestFenceGatePinRecheckBeforeEachAttempt(t *testing.T) {
	checkPinRecheckBeforeEachAttempt(t, fencePinGate)
}

func checkPinRecheckBeforeEachAttempt(t *testing.T, gateFor pinGateSpec) {
	repoRoot := t.TempDir()
	markers := t.TempDir()
	first := filepath.Join(markers, "attempt-0")
	second := filepath.Join(markers, "attempt-1")
	script := filepath.Join(t.TempDir(), "gate.sh")
	body := fmt.Sprintf(
		"if [ -e %[1]s ]; then /usr/bin/touch %[2]s; exit 0; fi\n"+
			"/usr/bin/touch %[1]s\n"+
			"printf B | /bin/dd of=%[3]s bs=1 seek=%[4]d conv=notrunc 2>/dev/null\n"+
			"exit 1\n",
		first, second, script, len(pinScriptHeader))
	content := pinScriptHeader + "A\n" + body
	writeErr := os.WriteFile(script, []byte(content), 0o755)
	testsupport.Must(t, writeErr, "writing the self-rewriting script: %v", writeErr)

	entry := pinnedEntry(repoRoot, script, trustHashOf(content))
	entry.Flaky = true
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, entry)

	ex, err := runner.Execute(context.Background(), gateFor(script), StepContext{})
	testsupport.Must(t, err, "running the flaky gate: %v", err)

	if !sentinelExists(t, first) {
		t.Fatal("the first attempt did not run")
	}
	if sentinelExists(t, second) {
		t.Error("the re-attempt RAN the rewritten script")
	}
	if len(ex.Results) != 2 {
		t.Fatalf("got %d rows, want 2 (the failed run, then the refusal): %+v",
			len(ex.Results), ex.Results)
	}
	if r := ex.Results[0]; r.Ordinal != 0 || r.Verdict != VerdictFail {
		t.Errorf("row 0 = ordinal %d verdict %q, want 0 and %q", r.Ordinal, r.Verdict, VerdictFail)
	}
	if r := ex.Results[1]; r.Ordinal != 1 || r.Verdict != VerdictUnmatched || r.Exit != nil {
		t.Errorf("row 1 = ordinal %d verdict %q exit %v, want 1, %q, nil",
			r.Ordinal, r.Verdict, r.Exit, VerdictUnmatched)
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("gate verdict = %q, want %q", ex.Verdict, VerdictFail)
	}
}

const legacyPinText = "has no content pin"

// legacyEntry is pinnedEntry without its content pin: an absolute argv[0]
// trusted before argv0_sha256 existed.
func legacyEntry(repoRoot, script string) trust.Entry {
	return pinnedEntry(repoRoot, script, "")
}

// assertNoLegacyWarning fails when any row carries the legacy-pin text.
func assertNoLegacyWarning(t *testing.T, rows []GateResultRow) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("the gate produced no rows")
	}
	for i, r := range rows {
		if strings.Contains(r.Reason, legacyPinText) {
			t.Errorf("row %d carries the legacy-pin warning: %q", i, r.Reason)
		}
	}
}

// TestGateRowCarriesLegacyPinWarning: a named gate whose entry has an absolute
// argv[0] and no content pin still runs, and every row it records names the
// missing pin and the re-pin command, after the reason the row already had.
func TestGateRowCarriesLegacyPinWarning(t *testing.T) {
	repoRoot := t.TempDir()
	marker := filepath.Join(t.TempDir(), "attempt-0")
	script := filepath.Join(t.TempDir(), "gate.sh")
	body := fmt.Sprintf("#!/bin/sh\nif [ -e %[1]s ]; then exit 0; fi\n/usr/bin/touch %[1]s\nexit 1\n", marker)
	writeErr := os.WriteFile(script, []byte(body), 0o755)
	testsupport.Must(t, writeErr, "writing the flaky script: %v", writeErr)

	entry := legacyEntry(repoRoot, script)
	entry.Flaky = true
	entry.Network = []string{"vuln.go.dev"}
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, entry)

	ex, err := runner.Execute(context.Background(), GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the legacy gate: %v", err)

	if len(ex.Results) != 2 {
		t.Fatalf("got %d rows, want 2 (fail, then pass): %+v", len(ex.Results), ex.Results)
	}
	if ex.Results[0].Verdict != VerdictFail || ex.Results[1].Verdict != VerdictPass ||
		ex.Verdict != VerdictPass {
		t.Errorf("verdicts = %q, %q, gate %q; want fail, pass, gate pass",
			ex.Results[0].Verdict, ex.Results[1].Verdict, ex.Verdict)
	}
	for i, r := range ex.Results {
		for _, want := range []string{legacyPinText, "docket trust add tests --"} {
			if !strings.Contains(r.Reason, want) {
				t.Errorf("row %d reason lacks %q: %q", i, want, r.Reason)
			}
		}
	}
	failing := ex.Results[0].Reason
	network := strings.Index(failing, "declares network access")
	if network < 0 || network > strings.Index(failing, legacyPinText) {
		t.Errorf("the failing row's network reason is missing or not first: %q", failing)
	}

	// An early-return row carries it too: a spent pre-gate budget spawns nothing.
	skipped, err := runner.Execute(context.Background(), GateSpec{Name: "tests"},
		StepContext{Deadline: time.Now().Add(-time.Second)})
	testsupport.Must(t, err, "running the gate on a spent budget: %v", err)
	if len(skipped.Results) != 1 || skipped.Results[0].Verdict != VerdictSkipped ||
		!strings.Contains(skipped.Results[0].Reason, legacyPinText) {
		t.Errorf("the budget-skip row lacks the warning: %+v", skipped.Results)
	}

	t.Run("a pinned entry records no warning", func(t *testing.T) {
		pinned, sum := pinnedScript(t, "exit 0\n")
		runner.LoadStore = sandboxTrust(t, pinnedEntry(repoRoot, pinned, sum))
		got, err := runner.Execute(context.Background(), GateSpec{Name: "tests"}, StepContext{})
		testsupport.Must(t, err, "running the pinned gate: %v", err)
		if got.Verdict != VerdictPass {
			t.Errorf("gate verdict = %q, want %q: %+v", got.Verdict, VerdictPass, got.Results)
		}
		assertNoLegacyWarning(t, got.Results)
	})

	t.Run("a relative argv[0] records no warning", func(t *testing.T) {
		argv := []string{"true"}
		runner.LoadStore = sandboxTrust(t, trust.Entry{
			Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
			Repo: mustResolve(repoRoot),
		})
		got, err := runner.Execute(context.Background(), GateSpec{Name: "tests"}, StepContext{})
		testsupport.Must(t, err, "running the relative gate: %v", err)
		if got.Verdict != VerdictPass {
			t.Errorf("gate verdict = %q, want %q: %+v", got.Verdict, VerdictPass, got.Results)
		}
		assertNoLegacyWarning(t, got.Results)
	})
}

// TestFenceRowCarriesLegacyPinWarning: the warning is per line, attributed to
// the entry that matched that line, not a flag on the whole fence gate.
func TestFenceRowCarriesLegacyPinWarning(t *testing.T) {
	repoRoot := t.TempDir()
	legacyScript, _ := pinnedScript(t, "exit 0\n")
	pinnedPath, sum := pinnedScript(t, "exit 0\n")

	legacy := legacyEntry(repoRoot, legacyScript)
	legacy.Name = "checks"
	pinned := pinnedEntry(repoRoot, pinnedPath, sum)
	pinned.Name = "checks"
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, legacy, pinned)

	ex, err := runner.Execute(context.Background(), GateSpec{
		Name: "checks", Source: "fence:checks",
		Commands: []string{legacyScript, pinnedPath},
	}, StepContext{})
	testsupport.Must(t, err, "running the fence gate: %v", err)

	if len(ex.Results) != 2 {
		t.Fatalf("got %d rows for two lines, want 2: %+v", len(ex.Results), ex.Results)
	}
	if ex.Verdict != VerdictPass {
		t.Errorf("gate verdict = %q, want %q: %+v", ex.Verdict, VerdictPass, ex.Results)
	}
	for _, want := range []string{legacyPinText, "docket trust add checks --"} {
		if !strings.Contains(ex.Results[0].Reason, want) {
			t.Errorf("the legacy line's reason lacks %q: %q", want, ex.Results[0].Reason)
		}
	}
	assertNoLegacyWarning(t, ex.Results[1:])
}
