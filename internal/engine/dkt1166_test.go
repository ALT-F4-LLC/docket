package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-1166: a pre-gate measuring a RECONSTRUCTION must not inherit — or leave
// behind — a linter result cache keyed to a directory that is about to be
// deleted.
//
// Harness RUN-64/STEP-2939 recorded `ac-commands: fail, exit 2` on a clean
// tree. Build and tests exited 0; `make lint` reported one forbidigo issue at
// `../docket-pregate-4091742512/internal/tui/screens/timelinecompare_test.go`,
// with golangci-lint warning that it could not read that file. The source
// carries `//nolint:forbidigo` on the line above the call, and the same sha
// linted in an ordinary checkout reports 0 issues.
//
// The cwd was never the problem: pregate.go already binds the reconstruction
// and gate_exec.go already spawns in it. The carrier is golangci-lint's own
// result cache, which keys issues by package CONTENT and stores the absolute
// path each was found at. The content hash outlives a reconstruction; the path
// does not — so an entry written from one reconstruction is replayed in the
// next, its `//nolint` lookup re-opens a file that is gone, and an already
// suppressed issue is re-emitted as live.

// scratchGateScript writes a gate that records the environment its child
// received, and returns the trust argv plus the file it will write.
//
// `/bin/sh <script>` rather than the script's own execute bit: the trust
// entry's argv[0] is what R4 authorizes, and /bin/sh is unambiguously outside
// any repository.
func scratchGateScript(t *testing.T, body string) (argv []string, out string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "gate.sh")
	out = filepath.Join(dir, "gate.out")
	full := "#!/bin/sh\nset -e\nout=" + out + "\n" + body + "\n"
	writeErr := os.WriteFile(script, []byte(full), 0o755)
	testsupport.Must(t, writeErr, "writing the gate script: %v", writeErr)
	return []string{"/bin/sh", script}, out
}

// TestReconstructionGatesGetTreeLifetimeLinterCaches is the mechanism.
//
// A gate spawned into a reconstruction must see linter cache variables that
// point OUTSIDE the tree (a cache inside it would show up in the tree's own
// `git status` and in a linter's file walk) and that are DELETED with it, so
// nothing it writes can be replayed against a path that no longer exists.
func TestReconstructionGatesGetTreeLifetimeLinterCaches(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")
	swept := filepath.Join(t.TempDir(), "gone")

	// The gate reports where it ran and which caches it was handed.
	argv, out := scratchGateScript(t, strings.Join([]string{
		`{`,
		`echo "pwd=$(pwd)"`,
		`echo "subject=$(cat measured.txt)"`,
		`echo "golangci=$GOLANGCI_LINT_CACHE"`,
		`echo "staticcheck=$STATICCHECK_CACHE"`,
		`} > "$out"`,
	}, "\n"))

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)
	setRunExecRoot(t, conn, step.RunID, repoRoot)

	got, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}}, sha, swept, nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)
	if len(got) != 1 || got[0].Verdict != VerdictPass {
		t.Fatalf("results = %+v, want one pass", got)
	}

	seen := map[string]string{}
	body, err := os.ReadFile(out)
	testsupport.Must(t, err, "the gate recorded no environment: %v", err)
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			seen[k] = v
		}
	}

	// AC1: the commands ran IN the reconstruction. This half already held —
	// it is asserted here so a later refactor cannot quietly move the spawn
	// back to the shared checkout and leave the cache fix looking sufficient.
	if seen["subject"] != "under review" {
		t.Errorf("the gate ran in %q, which is not the tree under review "+
			"(measured.txt = %q)", seen["pwd"], seen["subject"])
	}

	for _, name := range []string{"golangci", "staticcheck"} {
		dir := seen[name]
		if dir == "" {
			t.Errorf("%s cache is unset in a reconstruction; the tool would "+
				"resolve its persistent default and write entries naming a "+
				"directory that is about to be deleted", name)
			continue
		}
		// OUTSIDE the tree: the tree is the subject under measurement, and a
		// cache written into it lands in `git status` and in the linter's own
		// file walk.
		if strings.HasPrefix(dir, seen["pwd"]+string(os.PathSeparator)) {
			t.Errorf("%s cache %q is inside the tree under measurement %q",
				name, dir, seen["pwd"])
		}
		// AND GONE. A cache that outlives the reconstruction is the whole
		// defect: its entries name paths that stop existing, and the next run
		// over the same package content replays them.
		if _, statErr := os.Stat(dir); statErr == nil {
			t.Errorf("%s cache %q outlived the pre-gate phase", name, dir)
		}
	}
}

// TestLiveWorktreeGatesKeepTheirSharedCaches is the containment half.
//
// Re-analysis costs real time, and it is only worth paying where the tree is
// genuinely throwaway. A gate over a tree that stays on disk — the shared
// checkout, or a live worktree — must see exactly the environment it saw
// before, or a rare false verdict has been traded for a permanent slowdown on
// every gate in every run.
func TestLiveWorktreeGatesKeepTheirSharedCaches(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	repoRoot := t.TempDir()
	live := t.TempDir()
	argv, out := scratchGateScript(t, strings.Join([]string{
		`{`,
		`echo "golangci=$GOLANGCI_LINT_CACHE"`,
		`echo "staticcheck=$STATICCHECK_CACHE"`,
		`} > "$out"`,
	}, "\n"))

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	step := preGateStep(t, conn, e)

	_, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}}, "", live, nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)

	body, err := os.ReadFile(out)
	testsupport.Must(t, err, "the gate recorded no environment: %v", err)
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && v != "" {
			t.Errorf("%s cache was redirected to %q for a gate over a tree "+
				"that stays on disk; its cache should be reused there", k, v)
		}
	}
}

// --- AC2: the reproduction -------------------------------------------------

// golangciFixture seeds a module whose only lint issue is suppressed by a
// `//nolint:forbidigo` directive — the shape of harness's
// internal/tui/screens/timelinecompare_test.go:913.
const golangciModule = `module example.com/dkt1166

go 1.24
`

const golangciConfig = `version: "2"
linters:
  default: none
  enable:
    - forbidigo
  settings:
    forbidigo:
      forbid:
        - pattern: '^exec\.Command$'
          msg: "subprocess exec is only allowed in internal/adapters"
`

const golangciSource = `package pkg

import "os/exec"

// Run mirrors a test-only git fixture subprocess.
func Run() error {
	//nolint:forbidigo // test-only git fixture subprocess
	return exec.Command("true").Run()
}
`

// TestPreGateLintResolvesNolintInAPoisonedCache is DKT-1166's AC2, end to end
// and with the real linter.
//
// It stages the observed failure exactly: one gate lints the sha in a
// throwaway tree with the caches a gate used to get, that tree is deleted, and
// then the pre-gate runs. Before the fix, the second run replayed the first
// one's cached issue, could not re-open the recorded path to find the
// `//nolint`, and reported the suppressed issue as live — RUN-64/STEP-2939's
// `fail, exit 2` over a clean tree. After it, the reconstruction's caches are
// its own and the gate reports `0 issues.`, the answer an in-place checkout of
// that sha gives.
//
// THE POISONING RUN GOES THROUGH THE SAME RUNNER, and that is not incidental:
// golangci-lint's cache key covers more than file content, so an entry written
// by a differently-configured process is simply not found by the gate's child.
// Only a run under the gate's own environment stages the failure the engine
// actually produced.
func TestPreGateLintResolvesNolintInAPoisonedCache(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real linter three times")
	}
	linter, err := exec.LookPath("golangci-lint")
	if err != nil {
		t.Skip("golangci-lint is not installed")
	}
	if out, verErr := exec.Command(linter, "version").CombinedOutput(); verErr != nil ||
		!strings.Contains(string(out), "version 2") {
		t.Skipf("golangci-lint v2 is required for this fixture's config: %s", out)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not installed")
	}

	conn := mustDB(t)
	activatedRun(t, conn)

	// A private HOME (and XDG_CACHE_HOME, which is what os.UserCacheDir reads
	// on Linux) so the linter's DEFAULT cache — the one a gate child resolves
	// when nothing redirects it — lands in a directory this test owns. Without
	// this the poisoning run below would write into the developer's own cache.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	repoRoot := t.TempDir()
	sha := seedGoLintRepo(t, repoRoot)

	// The Go caches stay REAL and shared: they are not the carrier, and a cold
	// GOCACHE would make this test rebuild the standard library. Only the
	// linter's own result cache is under test.
	argv := lintGateArgv(t, linter, goEnvFor(t, "GOCACHE", "GOMODCACHE", "GOPATH"))

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true,
	})
	e := testEngine()
	e.Gates = runner

	// 1. POISON. A gate lints the sha in a throwaway tree with NO ephemeral
	//    cache — the behaviour every pre-gate had — and the tree is then
	//    deleted, exactly as the previous wave's reconstruction was. The cache
	//    now holds an issue naming a path that does not exist.
	first := lintInThrowawayTree(t, runner, repoRoot, sha, "earlier-reconstruction")
	if !strings.Contains(first, "0 issues") {
		t.Fatalf("the fixture is not clean before poisoning; the //nolint "+
			"directive should suppress its only issue:\n%s", first)
	}

	// 2. CONTROL. A second throwaway tree of the same sha, same absent
	//    ephemeral cache, re-emits the suppressed issue. Without this the test
	//    could pass because the cache was never poisoned at all — and it is
	//    what proves step 3 is measuring the fix rather than a cache miss.
	replayed := lintInThrowawayTree(t, runner, repoRoot, sha, "control-reconstruction")
	if strings.Contains(replayed, "0 issues") {
		t.Skipf("the shared cache did not replay the deleted tree's issue, so "+
			"this build of golangci-lint cannot stage the failure:\n%s", replayed)
	}
	if !strings.Contains(replayed, "no such file or directory") {
		t.Logf("the replay did not name a missing file; the staged failure may "+
			"differ from the reported one:\n%s", replayed)
	}

	// 3. THE PRE-GATE, into the poisoned cache: same sha, same environment,
	//    swept worktree — the engine reconstructs and lints.
	step := preGateStep(t, conn, e)
	setRunExecRoot(t, conn, step.RunID, repoRoot)

	got, err := runPreGates(conn, e, step,
		[]workflow.Gate{{Name: "ac-commands", Pre: true}},
		sha, filepath.Join(t.TempDir(), "swept"), nowMS)
	testsupport.Must(t, err, "runPreGates: %v", err)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if !strings.Contains(got[0].Output, "0 issues") {
		t.Errorf("the pre-gate lint did not match an in-place lint of the same "+
			"sha; a `//nolint`-suppressed issue was reported as live:\n%s",
			got[0].Output)
	}
	if got[0].Verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q — the tree is clean, and this is the "+
			"false `fail` RUN-64/STEP-2939 recorded. Output:\n%s",
			got[0].Verdict, VerdictPass, got[0].Output)
	}
}

// lintGateArgv writes the gate the AC2 test trusts: the real linter, with the
// real Go caches exported so only the linter's own result cache is in play.
//
// --allow-parallel-runners: golangci-lint otherwise takes a lock file in
// os.TempDir() that every golangci-lint on the machine shares, so a
// developer's own lint run would fail this test with an unrelated message.
func lintGateArgv(t *testing.T, linter string, goEnv []string) []string {
	t.Helper()
	var body strings.Builder
	body.WriteString("#!/bin/sh\n")
	for _, kv := range goEnv {
		body.WriteString("export " + kv + "\n")
	}
	body.WriteString("exec " + linter + " run --allow-parallel-runners ./...\n")
	script := filepath.Join(t.TempDir(), "lint.sh")
	writeErr := os.WriteFile(script, []byte(body.String()), 0o755)
	testsupport.Must(t, writeErr, "writing the lint gate: %v", writeErr)
	return []string{"/bin/sh", script}
}

// lintInThrowawayTree runs the lint gate in a worktree of `sha` that is
// removed immediately afterwards, with NO ephemeral cache — a pre-gate's
// behaviour before DKT-1166 — and returns what the gate printed.
//
// It goes through the ExecRunner rather than spawning the linter directly so
// the child sees the constructed gate environment: the cache entries it writes
// are then the ones a later gate child will actually look up.
func lintInThrowawayTree(
	t *testing.T, runner *ExecRunner, repoRoot, sha, name string,
) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, repoRoot, "worktree", "add", "--detach", "-q", dir, sha)
	defer gitRun(t, repoRoot, "worktree", "remove", "--force", dir)

	res, err := runner.Execute(context.Background(),
		GateSpec{Name: "ac-commands", Pre: true}, StepContext{WorkRoot: dir})
	testsupport.Must(t, err, "running the lint gate in %s: %v", name, err)
	if len(res.Results) != 1 {
		t.Fatalf("the lint gate in %s produced %d rows, want 1", name, len(res.Results))
	}
	return res.Results[0].Output
}

// seedGoLintRepo commits the fixture module and returns its sha.
func seedGoLintRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitRun(t, dir, "init", "-q")
	files := map[string]string{
		"go.mod":        golangciModule,
		".golangci.yml": golangciConfig,
		"pkg/a.go":      golangciSource,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		mkErr := os.MkdirAll(filepath.Dir(path), 0o755)
		testsupport.Must(t, mkErr, "creating %s: %v", name, mkErr)
		wrErr := os.WriteFile(path, []byte(body), 0o644)
		testsupport.Must(t, wrErr, "writing %s: %v", name, wrErr)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "the change under review")
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// goEnvFor reads the real values of the named `go env` variables, as
// `NAME=value` assignments a gate script can export.
func goEnvFor(t *testing.T, names ...string) []string {
	t.Helper()
	out := make([]string, 0, len(names))
	for _, name := range names {
		raw, err := exec.Command("go", "env", name).Output()
		testsupport.Must(t, err, "go env %s: %v", name, err)
		out = append(out, name+"="+strings.TrimSpace(string(raw)))
	}
	return out
}
