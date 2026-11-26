package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// probeRepo builds a throwaway repository with one commit, the same fixture
// gitdiff_test.go's gitRepo builds, so `git rev-parse HEAD` and the probe's
// checkout have a real object database to work against.
func probeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	run("init", "-q", ".")
	writeFile(t, dir, "README.md", "hello\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	return dir
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	testsupport.Must(t, err, "git worktree list: %v", err)
	// One "worktree <path>" line for the main checkout, one more per linked
	// worktree — so N linked worktrees means N+1 such lines.
	return strings.Count(string(out), "worktree ") - 1
}

func noAction(string) bool { return false }

// probeTempDir points os.MkdirTemp at a fresh directory, so everything the
// probe creates beside its worktree lands where the test can see it.
func probeTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// assertNoProbeLeftovers fails when anything the probe created in tmp, the
// tree, its git directory, its cache root or its sidecar, survived
// ProbeTrust's return.
func assertNoProbeLeftovers(t *testing.T, tmp string) {
	t.Helper()
	left, err := os.ReadDir(tmp)
	testsupport.Must(t, err, "reading %s: %v", tmp, err)
	for _, e := range left {
		t.Errorf("probe leftover in %s: %s", tmp, e.Name())
	}
}

// TestProbeTrustRunsEveryEntryEvenAfterAFailure is AC1: nothing
// short-circuits on the first failure, and each entry gets its own row with
// its own exit status.
func TestProbeTrustRunsEveryEntryEvenAfterAFailure(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "fails", Argv: []string{"false"}},
		{Name: "passes-after", Argv: []string{"true"}},
	}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 2 {
		t.Fatalf("want 2 gate rows (nothing short-circuits), got %d: %+v", len(result.Gates), result.Gates)
	}
	if result.Gates[0].Exit == nil || *result.Gates[0].Exit == 0 {
		t.Errorf("fails: want a nonzero exit, got %+v", result.Gates[0])
	}
	if result.Gates[1].Exit == nil || *result.Gates[1].Exit != 0 {
		t.Errorf("passes-after: want exit 0 — the prior failure must not have stopped it: %+v", result.Gates[1])
	}
	if result.Passed {
		t.Error("one failing gate must make the whole probe not-passed")
	}
	if len(result.Failed) != 1 || result.Failed[0] != "fails" {
		t.Errorf("want failed = [fails], got %v", result.Failed)
	}
}

// TestProbeTrustSkipsActionEntries is AC2: an entry the classifier reports as
// an action is never run and is reported in Skipped with a reason.
func TestProbeTrustSkipsActionEntries(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "aggregate", Argv: []string{"true"}},
		{Name: "lint", Argv: []string{"true"}},
	}
	isAction := func(name string) bool { return name == "aggregate" }

	result, err := ProbeTrust(context.Background(), repo, entries, isAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 || result.Gates[0].Name != "lint" {
		t.Fatalf("want exactly the non-action entry run, got %+v", result.Gates)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Name != "aggregate" {
		t.Fatalf("want aggregate skipped, got %+v", result.Skipped)
	}
	if result.Skipped[0].Reason == "" {
		t.Error("a skip must name its reason")
	}
	if !result.Passed {
		t.Errorf("the one runnable entry passed; the probe must report passed: %+v", result)
	}
}

// TestProbeTrustExportsGateBase: a probed gate sees DOCKET_GATE_BASE set to
// the probed HEAD, the same export the record-time runner gives a worktree
// step. Every base-diff gate refuses without it, so a probe that left it unset
// reported healthy gates as failed on clean HEAD; this gate fails exactly the
// way those did — a non-zero exit on an unset or wrong base — and passes only
// when the var names the sha the throwaway worktree was built from.
func TestProbeTrustExportsGateBase(t *testing.T) {
	repo := probeRepo(t)
	head, err := probeHead(repo)
	testsupport.Must(t, err, "resolving HEAD: %v", err)

	entries := []trust.Entry{{
		Name: "diff-scope",
		Argv: []string{"/bin/sh", "-c", `test "$DOCKET_GATE_BASE" = "` + head + `"`},
	}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want one gate row, got %d", len(result.Gates))
	}
	g := result.Gates[0]
	if g.Exit == nil || *g.Exit != 0 {
		t.Errorf("DOCKET_GATE_BASE did not name the probed HEAD %s: %+v", head, g)
	}
	if !result.Passed {
		t.Errorf("the probe must pass a base-diff gate on clean HEAD: %+v", result)
	}
}

// TestProbeTrustEnforcesPerEntryTimeout is AC3: a hung gate reports a timeout
// exit rather than hanging the verb.
func TestProbeTrustEnforcesPerEntryTimeout(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "hangs", Argv: []string{"sleep", "30"}, Timeout: "200ms"},
	}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want one gate row, got %d", len(result.Gates))
	}
	g := result.Gates[0]
	if g.Exit == nil || *g.Exit == 0 {
		t.Errorf("a timed-out gate must not report a zero or absent exit: %+v", g)
	}
	if !strings.Contains(g.LogTail, "timeout") {
		t.Errorf("the log tail should name the timeout, got %q", g.LogTail)
	}
}

func TestProbeTrustRemovesWorktreeOnCanceledContext(t *testing.T) {
	repo := probeRepo(t)
	tmp := probeTempDir(t)
	entries := []trust.Entry{
		{Name: "one", Argv: []string{"true"}},
		{Name: "two", Argv: []string{"true"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ProbeTrust(ctx, repo, entries, noAction)
	if err == nil {
		t.Fatal("a canceled context should surface as an error")
	}
	if n := worktreeCount(t, repo); n != 0 {
		t.Errorf("want no leftover worktree after interruption, got %d", n)
	}
	assertNoProbeLeftovers(t, tmp)
}

// TestProbeTrustGivesLintersAnEphemeralCacheRoot: a probed gate's linter
// caches point into a directory that dies with the probe worktree, not into
// the operator's own cache. Entries cached there name the worktree's paths,
// and a cache that outlives it replays suppressed issues as live.
func TestProbeTrustGivesLintersAnEphemeralCacheRoot(t *testing.T) {
	repo := probeRepo(t)
	probeTempDir(t)
	operatorCache := t.TempDir()
	t.Setenv("GOLANGCI_LINT_CACHE", filepath.Join(operatorCache, "golangci-lint"))
	t.Setenv("STATICCHECK_CACHE", filepath.Join(operatorCache, "staticcheck"))

	entries := []trust.Entry{{
		Name: "lint",
		Argv: []string{"/bin/sh", "-c",
			`printf 'GOLANGCI_LINT_CACHE=%s\nSTATICCHECK_CACHE=%s\n' "$GOLANGCI_LINT_CACHE" "$STATICCHECK_CACHE"`},
	}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)
	if len(result.Gates) != 1 || result.Gates[0].Exit == nil || *result.Gates[0].Exit != 0 {
		t.Fatalf("want one passing gate row, got %+v", result.Gates)
	}

	seen := map[string]string{}
	for _, line := range strings.Split(result.Gates[0].LogTail, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			seen[name] = value
		}
	}
	golangci, staticcheck := seen["GOLANGCI_LINT_CACHE"], seen["STATICCHECK_CACHE"]
	for name, v := range map[string]string{"GOLANGCI_LINT_CACHE": golangci, "STATICCHECK_CACHE": staticcheck} {
		if v == "" {
			t.Errorf("%s is unset in the probed gate's environment", name)
			continue
		}
		if strings.HasPrefix(v, operatorCache) {
			t.Errorf("%s = %q points into the operator's cache %q", name, v, operatorCache)
		}
	}
	if t.Failed() {
		return
	}

	root := filepath.Dir(golangci)
	if filepath.Dir(staticcheck) != root {
		t.Fatalf("want both caches under one root, got %q and %q", golangci, staticcheck)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("cache root %q outlived the probe (stat err: %v)", root, err)
	}
}

// TestProbeTrustRefusesAnEmptyRoster mirrors gate-probe.js's rule: a probe
// that measured nothing must not read as a pass.
func TestProbeTrustRefusesAnEmptyRoster(t *testing.T) {
	repo := probeRepo(t)

	if _, err := ProbeTrust(context.Background(), repo, nil, noAction); err == nil {
		t.Error("an empty roster must refuse rather than report zero gates as a pass")
	}

	entries := []trust.Entry{{Name: "aggregate", Argv: []string{"true"}}}
	isAction := func(string) bool { return true }
	if _, err := ProbeTrust(context.Background(), repo, entries, isAction); err == nil {
		t.Error("a roster that is entirely declared actions must refuse, not report a vacuous pass")
	}
}

// TestProbeTrustHeadIsRepoHead pins Head to the resolved sha, which is what a
// caller uses to say a failure was measured on clean HEAD.
func TestProbeTrustHeadIsRepoHead(t *testing.T) {
	repo := probeRepo(t)
	want, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	testsupport.Must(t, err, "git rev-parse HEAD: %v", err)

	entries := []trust.Entry{{Name: "ok", Argv: []string{"true"}}}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if result.Head != strings.TrimSpace(string(want)) {
		t.Errorf("Head = %q, want %q", result.Head, strings.TrimSpace(string(want)))
	}
}

// markerScript writes an executable script outside the repo that creates
// marker when run, and returns the script's path and the SHA-256 of its bytes,
// the value `trust add` stores as argv0_sha256.
func markerScript(t *testing.T, marker string) (path, pin string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "gate.sh")
	body := []byte("#!/bin/sh\ntouch '" + marker + "'\n")
	err := os.WriteFile(path, body, 0o755)
	testsupport.Must(t, err, "writing %s: %v", path, err)
	sum := sha256.Sum256(body)
	return path, hex.EncodeToString(sum[:])
}

func assertMarker(t *testing.T, marker string, want bool) {
	t.Helper()
	_, err := os.Stat(marker)
	if got := err == nil; got != want {
		t.Errorf("marker %s present = %v, want %v (stat err: %v)", marker, got, want, err)
	}
}

// TestProbeTrustRefusesDriftedContentPin: a pinned script rewritten after
// `trust add` is not spawned, its row fails with the path and both hashes, and
// the rest of the roster still probes.
func TestProbeTrustRefusesDriftedContentPin(t *testing.T) {
	repo := probeRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script, pin := markerScript(t, marker)

	f, err := os.OpenFile(script, os.O_APPEND|os.O_WRONLY, 0)
	testsupport.Must(t, err, "opening %s: %v", script, err)
	_, err = f.WriteString("#")
	testsupport.Must(t, err, "rewriting %s: %v", script, err)
	testsupport.Must(t, f.Close(), "closing %s", script)
	drifted, err := os.ReadFile(script)
	testsupport.Must(t, err, "reading %s: %v", script, err)
	now := sha256.Sum256(drifted)
	resolved, err := filepath.EvalSymlinks(script)
	testsupport.Must(t, err, "resolving %s: %v", script, err)

	entries := []trust.Entry{
		{Name: "pinned", Argv: []string{script}, Argv0SHA256: pin},
		{Name: "benign", Argv: []string{"true"}},
	}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 2 {
		t.Fatalf("want 2 gate rows, got %+v", result.Gates)
	}
	g := result.Gates[0]
	if g.Exit != nil {
		t.Errorf("a drifted pinned entry must not be spawned: %+v", g)
	}
	for _, want := range []string{resolved, pin, hex.EncodeToString(now[:])} {
		if !strings.Contains(g.LogTail, want) {
			t.Errorf("row reason %q does not name %q", g.LogTail, want)
		}
	}
	assertMarker(t, marker, false)
	if b := result.Gates[1]; b.Exit == nil || *b.Exit != 0 {
		t.Errorf("benign: the refused entry must not stop the rest: %+v", b)
	}
	if result.Passed || len(result.Failed) != 1 || result.Failed[0] != "pinned" {
		t.Errorf("want failed = [pinned] and not passed, got %+v", result)
	}
}

// TestProbeTrustRefusesUnhashablePin: a pinned file that cannot be read is
// refused, never treated as a match, and is not spawned.
func TestProbeTrustRefusesUnhashablePin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-0111 file, so the hash would not fail")
	}
	repo := probeRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script, pin := markerScript(t, marker)
	testsupport.Must(t, os.Chmod(script, 0o111), "chmod %s", script)

	entries := []trust.Entry{{Name: "pinned", Argv: []string{script}, Argv0SHA256: pin}}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want 1 gate row, got %+v", result.Gates)
	}
	g := result.Gates[0]
	if g.Exit != nil {
		t.Errorf("an unverifiable pinned entry must not be spawned: %+v", g)
	}
	if want := "cannot verify the content pin of " + script; !strings.Contains(g.LogTail, want) {
		t.Errorf("row reason %q does not contain %q", g.LogTail, want)
	}
	assertMarker(t, marker, false)
	if result.Passed {
		t.Error("an unverifiable pin must fail the probe")
	}
}

// TestProbeTrustRunsUnchangedPinnedScript: a pinned script whose bytes match
// its pin runs and passes.
func TestProbeTrustRunsUnchangedPinnedScript(t *testing.T) {
	repo := probeRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script, pin := markerScript(t, marker)

	entries := []trust.Entry{{Name: "pinned", Argv: []string{script}, Argv0SHA256: pin}}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want 1 gate row, got %+v", result.Gates)
	}
	if g := result.Gates[0]; g.Exit == nil || *g.Exit != 0 || g.Warning != "" {
		t.Errorf("an unchanged pinned script must run, pass, and carry no warning: %+v", g)
	}
	assertMarker(t, marker, true)
	if !result.Passed {
		t.Errorf("want passed, got %+v", result)
	}
}

// TestProbeTrustWarnsOnUnpinnedAbsoluteEntry: a legacy entry with an absolute
// argv[0] and no pin still runs, and its row carries the re-pin warning; a
// relative argv[0] has nothing to pin and carries none.
func TestProbeTrustWarnsOnUnpinnedAbsoluteEntry(t *testing.T) {
	repo := probeRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script, _ := markerScript(t, marker)

	entries := []trust.Entry{
		{Name: "legacy", Argv: []string{script}},
		{Name: "relative", Argv: []string{"true"}},
	}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 2 {
		t.Fatalf("want 2 gate rows, got %+v", result.Gates)
	}
	legacy := result.Gates[0]
	if legacy.Exit == nil || *legacy.Exit != 0 {
		t.Errorf("a legacy unpinned entry must still run: %+v", legacy)
	}
	assertMarker(t, marker, true)
	for _, want := range []string{"has no content pin", "docket trust add legacy"} {
		if !strings.Contains(legacy.Warning, want) {
			t.Errorf("legacy warning %q does not contain %q", legacy.Warning, want)
		}
	}
	if w := result.Gates[1].Warning; w != "" {
		t.Errorf("a relative argv[0] carries no pin warning, got %q", w)
	}
}

// TestProbeTrustMeasuresTheWorktreeNotTheSharedCheckout: a gate that reads its
// own cwd sees the throwaway checkout, not the shared repo — so a later `git
// worktree add` of a different branch in the shared checkout cannot leak into
// a probe's measurement.
func TestProbeTrustMeasuresTheWorktreeNotTheSharedCheckout(t *testing.T) {
	repo := probeRepo(t)
	marker := repo + "/marker-only-in-shared-checkout"
	err := os.WriteFile(marker, []byte("x"), 0o644)
	testsupport.Must(t, err, "writing marker: %v", err)
	// Deliberately left UNCOMMITTED: the throwaway worktree is a checkout of
	// HEAD, which never saw this file.
	entries := []trust.Entry{{Name: "check", Argv: []string{"test", "!", "-e", "marker-only-in-shared-checkout"}}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)
	if !result.Passed {
		t.Errorf("the probed tree must be the throwaway HEAD checkout, not the shared working tree: %+v", result.Gates)
	}
}

// TestProbeTrustSurvivesPinnedSymlinkFlip: a pinned entry whose argv[0] is a
// symlink retargeted between a pinned and an unpinned script for the whole
// probe either runs the pinned script and passes, or is refused without a
// spawn. It never records a pass that ran the unpinned script. The flip
// retargets only the link; replacing the hashed file itself is outside what
// the probe binds.
func TestProbeTrustSurvivesPinnedSymlinkFlip(t *testing.T) {
	repo := probeRepo(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the fixture dir: %v", err)
	marker := filepath.Join(dir, "marker")
	script := func(name string) (string, []byte) {
		path := filepath.Join(dir, name+".sh")
		body := []byte("#!/bin/sh\necho " + name + " >> '" + marker + "'\necho " + name + "\n")
		testsupport.Must(t, os.WriteFile(path, body, 0o755), "writing %s", path)
		return path, body
	}
	pinnedPath, pinnedBody := script("pinned")
	unpinnedPath, _ := script("unpinned")
	sum := sha256.Sum256(pinnedBody)
	pin := hex.EncodeToString(sum[:])
	link := filepath.Join(dir, "argv0")
	testsupport.Must(t, os.Symlink(pinnedPath, link), "linking argv0")

	const runs = 200
	entries := make([]trust.Entry, runs)
	for i := range entries {
		entries[i] = trust.Entry{Name: "g" + strconv.Itoa(i), Argv: []string{link}, Argv0SHA256: pin}
	}

	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		tmp := link + ".next"
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			target := pinnedPath
			if i%2 == 1 {
				target = unpinnedPath
			}
			// Symlink then rename replaces the link atomically, so it always
			// names one of the two scripts. The script files are never touched.
			_ = os.Remove(tmp)
			if err := os.Symlink(target, tmp); err != nil {
				t.Errorf("staging the flip: %v", err)
				return
			}
			if err := os.Rename(tmp, link); err != nil {
				t.Errorf("flipping the link: %v", err)
				return
			}
		}
	}()
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	close(stop)
	<-flipped
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != runs {
		t.Fatalf("want %d gate rows, got %d", runs, len(result.Gates))
	}
	var passed, refused int
	for _, g := range result.Gates {
		switch {
		case g.Exit == nil:
			refused++
		case *g.Exit == 0 && strings.TrimSpace(g.LogTail) == "pinned":
			passed++
		default:
			t.Errorf("%s: want a pass of the pinned script or a refusal, got exit %d and output %q",
				g.Name, *g.Exit, g.LogTail)
		}
	}

	b, err := os.ReadFile(marker)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the marker: %v", err)
	}
	ranScripts := strings.Fields(string(b))
	for _, name := range ranScripts {
		if name != "pinned" {
			t.Fatalf("the unpinned script ran (passed=%d refused=%d)", passed, refused)
		}
	}
	if len(ranScripts) != passed {
		t.Errorf("the marker records %d runs, but %d rows passed", len(ranScripts), passed)
	}
	// Both outcomes must occur, or the probe did not exercise the flip.
	if passed == 0 || refused == 0 {
		t.Fatalf("passed=%d refused=%d over %d runs; the flip was not exercised", passed, refused, runs)
	}
	if result.Passed {
		t.Error("a refused pinned entry must fail the probe")
	}
}

// TestTrustProbeWritesNothingToTheGitDir: with the repository's worktrees/
// directory not writable, ProbeTrust still runs its entries in a tree where
// `git rev-parse HEAD` equals the probed commit, adds no entry under the git
// directory's worktrees/, and removes the tree afterwards.
//
// The gate counts worktrees/ WHILE the probe runs, as well as the test
// asserting the probe's success, so the test does not pass vacuously for a
// user the mode bits do not stop: a probe that registered a worktree would
// succeed as root, and its release would remove the entry before any
// assertion after ProbeTrust could see it. The listings taken afterwards
// catch anything left behind.
func TestTrustProbeWritesNothingToTheGitDir(t *testing.T) {
	repo := probeRepo(t)
	tmp := probeTempDir(t)
	head := gitRun(t, repo, "rev-parse", "HEAD")

	worktrees := filepath.Join(repo, ".git", "worktrees")
	testsupport.Must(t, os.Mkdir(worktrees, 0o555), "making worktrees/ read-only")
	t.Cleanup(func() { _ = os.Chmod(worktrees, 0o755) })
	before := gitDirListing(t, repo)

	// The gate records where it ran, what HEAD it saw and how many entries
	// worktrees/ held meanwhile, outside the temp directory the probe's
	// leftovers are counted in.
	seen := filepath.Join(t.TempDir(), "seen")
	entries := []trust.Entry{{Name: "head", Argv: []string{"sh", "-c",
		`{ pwd; git rev-parse HEAD; ls -A "$1" | wc -l; } > "$0"`, seen, worktrees}}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust with worktrees/ not writable: %v", err)
	if !result.Passed {
		t.Fatalf("the probe's entry failed: %+v", result.Gates)
	}

	raw, err := os.ReadFile(seen)
	testsupport.Must(t, err, "reading what the gate saw: %v", err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("the gate wrote %q, want its directory, HEAD and the worktrees/ count", raw)
	}
	if n := strings.TrimSpace(lines[2]); n != "0" {
		t.Errorf("worktrees/ held %s entries while the probe ran, want 0", n)
	}
	if lines[1] != head {
		t.Errorf("git rev-parse HEAD in the probe tree = %s, want the probed commit %s", lines[1], head)
	}
	if lines[0] == repo || !strings.HasPrefix(lines[0], tmp) {
		t.Errorf("the gate ran in %s, want a throwaway tree under %s", lines[0], tmp)
	}

	got, err := os.ReadDir(worktrees)
	testsupport.Must(t, err, "reading worktrees/: %v", err)
	if len(got) != 0 {
		t.Errorf("the probe added %d entries under the git directory's worktrees/", len(got))
	}
	if after := gitDirListing(t, repo); !slices.Equal(before, after) {
		t.Errorf("the probe changed the repository's git directory:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := os.Lstat(lines[0]); !os.IsNotExist(err) {
		t.Errorf("the probe tree %s survived ProbeTrust's return (lstat: %v)", lines[0], err)
	}
	assertNoProbeLeftovers(t, tmp)
}

// TestTrustProbeCheckoutFailureLeavesNothing: a commit the repository does
// not hold is refused with an error, and everything made for the tree before
// the checkout failed is removed.
func TestTrustProbeCheckoutFailureLeavesNothing(t *testing.T) {
	repo := probeRepo(t)
	tmp := probeTempDir(t)

	if _, err := probeWorktree(repo, strings.Repeat("1", 40)); err == nil {
		t.Fatal("probeWorktree checked out a commit the repository does not hold")
	}
	assertNoProbeLeftovers(t, tmp)
}

// TestTrustProbeTreeIsSweptOnlyOnceItsLockIsGone: the pre-gate sweep leaves a
// probe tree alone while the probe holds it, and removes it, sidecar and all,
// once the holder is gone, as it would after a killed probe.
func TestTrustProbeTreeIsSweptOnlyOnceItsLockIsGone(t *testing.T) {
	repo := probeRepo(t)
	tmp := probeTempDir(t)
	head := gitRun(t, repo, "rev-parse", "HEAD")

	h, err := probeWorktree(repo, head)
	testsupport.Must(t, err, "probeWorktree: %v", err)
	t.Cleanup(h.release)

	if swept := sweepStalePreGateScratch(repo); len(swept) != 0 {
		t.Fatalf("the sweep removed %v while the probe held its tree", swept)
	}
	if _, err := os.Stat(h.Dir); err != nil {
		t.Fatalf("the held probe tree is gone after a sweep: %v", err)
	}

	// Closing the lock file is what the kernel does for a killed probe.
	h.tree.lock.Close()
	if swept := sweepStalePreGateScratch(repo); !slices.Equal(swept, []string{h.Dir}) {
		t.Errorf("the sweep removed %v, want the abandoned probe tree %s", swept, h.Dir)
	}
	assertNoProbeLeftovers(t, tmp)
}

// commitProbeFile writes rel with body in repo and commits it, so the probe's
// throwaway worktree (a checkout of HEAD) holds the file. It returns the
// SHA-256 of body, the value `trust add` stores as a declared file's pin.
func commitProbeFile(t *testing.T, repo, rel, body string) string {
	t.Helper()
	writeFile(t, repo, rel, body)
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "add " + rel}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// TestProbeTrustRefusesDeclaredFileModifiedInSharedCheckout: the probe hashes
// the worktree's committed bytes, never the shared checkout's. A declared file
// edited but not committed there, and pinned at its edited bytes, does not match
// the worktree, so the entry is refused with the changed-since-trusted reason
// and nothing runs.
func TestProbeTrustRefusesDeclaredFileModifiedInSharedCheckout(t *testing.T) {
	repo := probeRepo(t)
	commitProbeFile(t, repo, "gate/sourced.conf", "committed\n")
	writeFile(t, repo, "gate/sourced.conf", "edited, uncommitted\n")
	dirty := sha256.Sum256([]byte("edited, uncommitted\n"))
	marker := filepath.Join(t.TempDir(), "ran")
	script, scriptPin := markerScript(t, marker)

	entries := []trust.Entry{{
		Name: "declares", Argv: []string{script}, Argv0SHA256: scriptPin,
		DeclaredFiles: []trust.DeclaredFile{{Path: "gate/sourced.conf", SHA256: hex.EncodeToString(dirty[:])}},
	}}
	assertDeclaredFileRefused(t, repo, entries, marker, "gate/sourced.conf")
}

// TestProbeTrustRefusesDeclaredFileWithWrongSHA: a declared file pinned to a
// sha256 its committed bytes do not hash to is refused and nothing runs.
func TestProbeTrustRefusesDeclaredFileWithWrongSHA(t *testing.T) {
	repo := probeRepo(t)
	commitProbeFile(t, repo, "gate/sourced.conf", "committed\n")
	marker := filepath.Join(t.TempDir(), "ran")
	script, scriptPin := markerScript(t, marker)

	entries := []trust.Entry{{
		Name: "declares", Argv: []string{script}, Argv0SHA256: scriptPin,
		DeclaredFiles: []trust.DeclaredFile{{Path: "gate/sourced.conf", SHA256: strings.Repeat("0", 64)}},
	}}
	assertDeclaredFileRefused(t, repo, entries, marker, "gate/sourced.conf")
}

// assertDeclaredFileRefused probes entries and requires one row refused with
// the changed-since-trusted reason naming declared, with Exit nil, the marker
// absent, and the probe failed.
func assertDeclaredFileRefused(t *testing.T, repo string, entries []trust.Entry, marker, declared string) {
	t.Helper()
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want 1 gate row, got %+v", result.Gates)
	}
	g := result.Gates[0]
	if g.Exit != nil {
		t.Errorf("a declared-file mismatch must not be spawned: %+v", g)
	}
	for _, want := range []string{"declared file " + declared, "changed since it was trusted"} {
		if !strings.Contains(g.LogTail, want) {
			t.Errorf("row reason %q does not contain %q", g.LogTail, want)
		}
	}
	assertMarker(t, marker, false)
	if result.Passed {
		t.Error("a declared-file mismatch must fail the probe")
	}
}

// TestProbeTrustIgnoresDirtySharedCheckoutWhenHeadMatchesThePin: the gate
// spawns in the clean-HEAD worktree and reads those bytes, so a declared file
// edited but not committed in the shared checkout does not matter when the pin
// matches the committed bytes. Only drift under the root the gate spawns in
// counts.
func TestProbeTrustIgnoresDirtySharedCheckoutWhenHeadMatchesThePin(t *testing.T) {
	repo := probeRepo(t)
	pin := commitProbeFile(t, repo, "gate/sourced.conf", "committed\n")
	writeFile(t, repo, "gate/sourced.conf", "edited, uncommitted\n")
	marker := filepath.Join(t.TempDir(), "ran")
	script, scriptPin := markerScript(t, marker)

	entries := []trust.Entry{{
		Name: "declares", Argv: []string{script}, Argv0SHA256: scriptPin,
		DeclaredFiles: []trust.DeclaredFile{{Path: "gate/sourced.conf", SHA256: pin}},
	}}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want 1 gate row, got %+v", result.Gates)
	}
	if g := result.Gates[0]; g.Exit == nil || *g.Exit != 0 {
		t.Errorf("a pin that matches the worktree's bytes must run, whatever the shared checkout holds: %+v", g)
	}
	assertMarker(t, marker, true)
	if !result.Passed {
		t.Errorf("want passed, got %+v", result)
	}
}
