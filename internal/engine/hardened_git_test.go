package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// An executor in a linked worktree can write refs/replace for the whole shared
// repository. A gate that measures a checkout built through that ref measures
// a different tree than the commit `step integrate` later picks.
func TestAReplaceRefDoesNotChangeWhatTheEngineReconstructs(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	base := seedGitRepo(t, repo, "measured.txt", "base")
	setRunExecRoot(t, conn, run.ID, repo)

	// H is the commit the writer records; R is a sibling with passing content.
	writeFile(t, repo, "measured.txt", "failing\n")
	gitRun(t, repo, "commit", "-q", "-am", "the recorded work")
	recorded := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "--detach", base)
	writeFile(t, repo, "measured.txt", "passing\n")
	gitRun(t, repo, "commit", "-q", "-am", "a lookalike")
	lookalike := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "replace", recorded, lookalike)

	scratch := reconstructTarget(conn, run.ID, recorded)
	if scratch.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	defer scratch.release()
	got, err := os.ReadFile(filepath.Join(scratch.Dir, "measured.txt"))
	testsupport.Must(t, err, "reading the scratch tree: %v", err)
	if strings.TrimSpace(string(got)) != "failing" {
		t.Errorf("the scratch tree holds %q; a replace ref made the gate measure "+
			"the lookalike instead of the recorded commit", got)
	}
}

// The engine's git reads must not run a program the repository's config or the
// executor's environment names: a linked worktree's `git config` writes the
// shared config, and the detached child inherits the executor's environment.
func TestEngineGitRunsNoProgramTheRepositoryOrEnvironmentNames(t *testing.T) {
	repo := t.TempDir()
	seedGitRepo(t, repo, "f.txt", "one")
	writeFile(t, repo, "f.txt", "two\n")

	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	testsupport.Must(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho ran >> "+marker+"\nexit 0\n"), 0o755), "writing the script")

	gitRun(t, repo, "config", "core.fsmonitor", script)
	gitRun(t, repo, "config", "diff.external", script)
	gitRun(t, repo, "config", "diff.textconv-test.textconv", script)
	writeFile(t, repo, ".gitattributes", "*.txt diff=textconv-test\n")
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", script)

	if _, err := rawDiff(repo, "HEAD", nil); err != nil {
		t.Fatalf("rawDiff: %v", err)
	}
	if _, err := gitCommand("-C", repo, "status", "--porcelain").Output(); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an engine git read ran a program named by the repository config or the " +
			"environment (fsmonitor, external diff or textconv)")
	}
}

// A tree-holding step whose worktree has no resolvable HEAD at record time is
// refused: storing no head would send the gates and the diff to the live tree
// unpinned, so an executor could show one commit to the gates and another to
// the diff.
func TestRecordingFromAWorktreeWithNoHeadIsRefused(t *testing.T) {
	conn := mustDB(t)
	stepID, _, token := claimedImplement(t, conn)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	shared := gitRepo(t)
	setRunExecRoot(t, conn, step.RunID, shared)
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRun(t, shared, "worktree", "add", "-q", "--detach", worktree)
	gitRun(t, worktree, "checkout", "-q", "--orphan", "unborn")

	e := testEngine()
	e.HeadFn = sharedCheckoutHead
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS, WorkDir: worktree,
	})
	if err == nil || !strings.Contains(err.Error(), "no commit resolves") {
		t.Fatalf("recording from an unborn worktree = %v; want a refusal naming the missing HEAD", err)
	}

	// The refusal spent nothing: the same lease records once the work is committed.
	writeFile(t, worktree, "w.txt", "w\n")
	gitRun(t, worktree, "add", "w.txt")
	gitRun(t, worktree, "commit", "-q", "-m", "the work")
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS, WorkDir: worktree,
	}), "recording after committing")
}

// A re-claimed step that records WITHOUT --worktree keeps its earlier
// work_root, and the gates and the diff still measure that tree. An executor
// that orphans it between attempts must be refused, not given an unpinned run.
func TestARecordWithoutWorktreeIsRefusedWhenTheKeptWorkRootHasNoHead(t *testing.T) {
	conn := mustDB(t)
	stepID, _, token := claimedImplement(t, conn)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	shared := gitRepo(t)
	setRunExecRoot(t, conn, step.RunID, shared)
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRun(t, shared, "worktree", "add", "-q", "--detach", worktree)
	gitRun(t, worktree, "checkout", "-q", "--orphan", "unborn")
	_, err = conn.Exec(`UPDATE steps SET work_root = ? WHERE id = ?`, worktree, stepID)
	testsupport.Must(t, err, "keeping the earlier work_root: %v", err)

	e := testEngine()
	e.HeadFn = sharedCheckoutHead
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	if err == nil || !strings.Contains(err.Error(), "no commit resolves") {
		t.Fatalf("a bare re-record over an orphaned work_root = %v; want a refusal", err)
	}
}

// log -p applies textconv, log/show verify a signature through gpg.program when
// log.showSignature is set, and the shared config is writable by the executor.
func TestEngineLogAndShowRunNoTextconvOrSignatureProgram(t *testing.T) {
	repo := t.TempDir()
	first := seedGitRepo(t, repo, "f.txt", "one")
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "prog.sh")
	testsupport.Must(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho ran >> "+marker+"\nexit 0\n"), 0o755), "writing the script")

	gitRun(t, repo, "config", "diff.conv.textconv", script)
	gitRun(t, repo, "config", "log.showSignature", "true")
	gitRun(t, repo, "config", "gpg.program", script)
	writeFile(t, repo, ".gitattributes", "*.txt diff=conv\n")
	writeFile(t, repo, "f.txt", "two\n")
	gitRun(t, repo, "add", ".gitattributes", "f.txt")
	gitRun(t, repo, "commit", "-q", "-m", "second")
	// A commit carrying a gpgsig header, which log.showSignature verifies.
	tree := gitRun(t, repo, "rev-parse", "HEAD^{tree}")
	raw := "tree " + tree + "\nparent " + first + "\nauthor t <t@t> 1 +0000\ncommitter t <t@t> 1 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\nsigned\n"
	rawFile := filepath.Join(t.TempDir(), "commit.txt")
	testsupport.Must(t, os.WriteFile(rawFile, []byte(raw), 0o644), "writing the commit")
	signed := gitRun(t, repo, "hash-object", "-t", "commit", "-w", rawFile)
	gitRun(t, repo, "update-ref", "HEAD", signed)

	if _, ok := gitPatchIDPairs(repo, "-2"); !ok {
		t.Fatal("gitPatchIDPairs failed")
	}
	if _, ok := gitOutputLines(repo, "show", "-s", "--format=%B", signed); !ok {
		t.Fatal("gitOutputLines show failed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an engine log or show ran the textconv or gpg program named by the shared config")
	}
}

// The engine's git environment keeps the executor's transport and lazy-fetch
// settings out.
func TestEngineGitEnvironmentDropsTransportAndRedirects(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "/bin/false")
	t.Setenv("GIT_PROXY_COMMAND", "/bin/false")
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext")
	t.Setenv("GIT_NO_LAZY_FETCH", "0")
	env := strings.Join(integrateGitEnv(), "\n")
	for _, bad := range []string{"GIT_SSH_COMMAND=", "GIT_PROXY_COMMAND=", "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=0"} {
		if strings.Contains(env, bad) {
			t.Errorf("the engine git environment keeps %s", bad)
		}
	}
	if !strings.Contains(env, "GIT_NO_LAZY_FETCH=1") {
		t.Error("the engine git environment does not turn lazy fetch off")
	}
}

// Git reads hook.<name>.command and hook.<name>.event from the repository
// config, which an executor writes, and core.hooksPath does not stop them.
func TestEngineGitRunsNoHookTheConfigNames(t *testing.T) {
	repo := t.TempDir()
	base := seedGitRepo(t, repo, "f.txt", "one")
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	testsupport.Must(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho ran >> "+marker+"\nexit 0\n"), 0o755), "writing the script")
	// Names an executor can choose: plain, dotted, upper case, and holding a space,
	// a tab or an equals sign.
	for i, name := range []string{"x", "a.b", "Up.Case", "sp ace", "tab\tname", "eq=name"} {
		ev := []string{"post-checkout", "post-index-change", "reference-transaction"}[i%3]
		gitRun(t, repo, "config", "hook."+name+".event", ev)
		gitRun(t, repo, "config", "hook."+name+".command", script)
	}
	for _, ev := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		gitRun(t, repo, "config", "hook.x"+ev+".event", ev)
		gitRun(t, repo, "config", "hook.x"+ev+".command", script)
	}

	// Control: plain git fires the config hook here. On a git that predates the
	// keys the control stays silent and there is nothing to defend against.
	ctl := filepath.Join(t.TempDir(), "ctl")
	gitRun(t, repo, "worktree", "add", "-q", "--detach", ctl, base)
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git does not run hook.<name> keys from the repository config")
	}
	testsupport.Must(t, os.Remove(marker), "clearing the control marker")

	scratch := filepath.Join(t.TempDir(), "scratch")
	if out, err := gitCommand("-C", repo, "worktree", "add", "-q", "--detach", scratch, base).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	writeFile(t, scratch, "g.txt", "g\n")
	if _, err := rawDiff(scratch, "HEAD", nil); err != nil {
		t.Fatalf("rawDiff: %v", err)
	}
	if _, err := integrateGit(repo, "status", "--porcelain"); err != nil {
		t.Fatalf("integrateGit status: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an engine git command ran a hook named by hook.<name>.command in the repository config")
	}
}

// Every git the engine runs against an executor-influenced tree goes through
// the hardened constructors; a raw exec of git anywhere else bypasses them.
func TestNoRawGitExecOutsideTheHardenedConstructors(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package: %v", err)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "integrate_git.go" {
			continue
		}
		src, err := os.ReadFile(name)
		testsupport.Must(t, err, "reading %s: %v", name, err)
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `Command("git"`) || strings.Contains(line, `CommandContext(ctx, "git"`) ||
				strings.Contains(line, `Command("git",`) {
				t.Errorf("%s:%d runs git without the hardened constructor: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestGitDirOfReadsOnlyTheLeadingGlobalOptions(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-C", "/r", "diff", "--", "x"}, "/r"},
		{[]string{"-c", "a=b", "-C", "/r", "status"}, "/r"},
		{[]string{"--literal-pathspecs", "-C", "/r", "log"}, "/r"},
		{[]string{"diff", "--", "-C", "/other"}, ""},
		{[]string{"ls-files", "-C", "/other"}, ""},
		{nil, ""},
	} {
		if got := gitDirOf(c.args); got != c.want {
			t.Errorf("gitDirOf(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
