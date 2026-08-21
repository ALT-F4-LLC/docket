package engine

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// UNION CONFIG ROOTS — activation reads the SHARED corpus and the REPOSITORY's
// own additions as one instance config.
//
// The shape these tests describe is the installed one: `~/.docket/config/` holds
// definitions every project on the machine draws from, and a repository may add
// its own at `<worktree>/.docket/config/` — or add nothing, and carry no
// `.docket/` directory at all. Before this, the global store scanned the
// repository ALONE, which is why a linked git worktree could claim a step and
// then fail to render it: the packet's files lived in a checkout the worktree
// could not see, and the failure landed AFTER the claim recorded.
//
// The union only works because the roots are forbidden to DISAGREE. Every test
// below that asserts a refusal is asserting that: a ref or a `name@version`
// offered twice with different bytes stops the activation and names both files,
// so "first root wins" describes a tie that cannot happen rather than a silent
// shadowing.

// unionRepo puts resolution on the GLOBAL path — the only source with more than
// one root — and hands back the two config directories, neither yet created.
//
// THE TEMP DIRS AND THE DATABASE COME FIRST, BEFORE t.Setenv and t.Chdir, for
// configRepo's reason: `t.TempDir()` reads TMPDIR and `db.Open` takes an
// absolute path, and a helper that rewrote the environment first would be
// building its world against paths it had just moved.
func unionRepo(t *testing.T) (conn *sql.DB, shared, repo string) {
	t.Helper()

	home := t.TempDir()
	work := t.TempDir()
	conn = mustDB(t)

	// DOCKET_PATH is pinned package-wide by TestMain; clearing it is what lets
	// this test see the global store's two roots at all.
	t.Setenv("DOCKET_PATH", "")
	t.Setenv("HOME", home)
	t.Chdir(work)

	return conn, filepath.Join(home, ".docket", "config"), filepath.Join(work, ".docket", "config")
}

// canonical resolves a path the way the scan does, so an assertion compares the
// form the engine records rather than the form the test typed (macOS resolves
// /var to /private/var under every temp directory).
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// ---------------------------------------------------------------------------
// One root at a time
// ---------------------------------------------------------------------------

// TestSharedRootActivatesWithNoRepoConfig is the whole point of the change: a
// repository with NO `.docket/` directory activates against the shared corpus.
func TestSharedRootActivatesWithNoRepoConfig(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)

	if _, err := os.Stat(repo); !os.IsNotExist(err) {
		t.Fatalf("premise: the repository must carry no config root (stat: %v)", err)
	}

	issue := createIssue(t, conn, "corpus only", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if len(result.Registered) != 1 {
		t.Fatalf("registered %d files, want the corpus's one workflow: %+v",
			len(result.Registered), result.Registered)
	}
	if result.IssuesBound != 1 || result.StepsCreated == 0 {
		t.Errorf("bound=%d steps=%d; the run must bind against the shared corpus",
			result.IssuesBound, result.StepsCreated)
	}
}

// TestRepoRootActivatesWithNoSharedConfig is the mirror: a machine whose shared
// corpus is not installed still activates a repository's own config, unchanged.
func TestRepoRootActivatesWithNoSharedConfig(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, repo, "workflows/auto-dev.toml", autoWorkflowSrc)

	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatalf("premise: the shared root must be absent (stat: %v)", err)
	}

	issue := createIssue(t, conn, "repo only", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if len(result.Registered) != 1 || result.StepsCreated == 0 {
		t.Errorf("registered=%d steps=%d; an absent shared root must be skipped, "+
			"not fatal", len(result.Registered), result.StepsCreated)
	}
}

// ---------------------------------------------------------------------------
// The union, and its ordering
// ---------------------------------------------------------------------------

// TestUnionRegistersDisjointRootsSchemasFirst is F2's ordering carried ACROSS
// roots, and the fixture is arranged so root order alone cannot pass it.
//
// The WORKFLOW is in the shared root, which is scanned FIRST, and the SCHEMA it
// names is in the repository root, scanned second. An implementation that
// registered each root in turn would refuse the workflow on a `payload` naming
// a schema that is one root away from existing. Only the global two-group order
// — every schema, then every workflow — registers this pair.
func TestUnionRegistersDisjointRootsSchemasFirst(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/a-payload.toml", payloadWorkflowSrc)
	writeConfigFile(t, repo, "schemas/risk@1.json", riskSchemaSrc)

	issue := createIssue(t, conn, "union order", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "a per-root registration order would fail exactly here: the "+
		"shared root's workflow names a schema the repository root holds: %v", err)

	if len(result.Registered) != 2 {
		t.Fatalf("registered %d files, want the schema and the workflow: %+v",
			len(result.Registered), result.Registered)
	}
	if result.Registered[0].Kind != RegistrationKindSchema {
		t.Errorf("registered %s first; EVERY schema registers before ANY workflow, "+
			"whichever root each came from", result.Registered[0].Kind)
	}
	if result.Registered[1].Kind != RegistrationKindWorkflow {
		t.Errorf("registered %s second, want the workflow", result.Registered[1].Kind)
	}
}

// TestUnionPinsBothRoots: the pinned half unions too, and every ref stays
// ROOT-RELATIVE, so a packet entry means the same string whichever root supplied
// the file.
func TestUnionPinsBothRoots(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, shared, "contracts/house-style.md", "the corpus's contract\n")
	writeConfigFile(t, repo, "contracts/project.md", "this repo's contract\n")

	issue := createIssue(t, conn, "union pins", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if result.PinsFromConfig != 2 {
		t.Fatalf("pinned %d config files, want one from each root", result.PinsFromConfig)
	}

	refs := map[string]bool{}
	for _, p := range pinsByKind(t, conn, run.ID, db.PinKindFile) {
		refs[filepath.ToSlash(p.Ref)] = true
	}
	for _, want := range []string{"contracts/house-style.md", "contracts/project.md"} {
		if !refs[want] {
			t.Errorf("pin refs %v do not include %q; a ref is relative to ITS root, "+
				"so both roots' files address the same way", refs, want)
		}
	}
}

// TestUnionIdenticalDuplicateIsANoOp: the same file in both roots — a repository
// that vendored a copy of what the corpus ships — registers ONCE and pins ONCE.
//
// A second `unchanged` row would describe a decision nobody made, and a second
// pin would double-count the same bytes against the closure caps.
func TestUnionIdenticalDuplicateIsANoOp(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, repo, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, shared, "contracts/fix.md", "identical\n")
	writeConfigFile(t, repo, "contracts/fix.md", "identical\n")

	issue := createIssue(t, conn, "vendored copy", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "identical bytes in two roots must be a no-op, not a refusal: %v", err)

	if len(result.Registered) != 1 {
		t.Errorf("registered %d files, want one — the second root's copy is a no-op: %+v",
			len(result.Registered), result.Registered)
	}
	if result.PinsFromConfig != 1 {
		t.Errorf("pinned %d config files, want one — the ref names one file's bytes",
			result.PinsFromConfig)
	}
	if result.StepsCreated == 0 {
		t.Error("the run did not expand against the deduplicated definition")
	}
}

// TestUnionDifferingRegistryDuplicateRefuses is the registry half of the
// disagreement rule: one `name@version`, two roots, different bytes.
//
// The message names BOTH absolute paths and the identity, because neither file
// is more authoritative than the other — an operator has to look at both to
// decide which one they meant.
func TestUnionDifferingRegistryDuplicateRefuses(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	sharedPath := writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	repoPath := writeConfigFile(t, repo, "workflows/auto-dev.toml",
		strings.Replace(autoWorkflowSrc, `executor = "w"`, `executor = "x"`, 1))

	issue := createIssue(t, conn, "roots disagree", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("two roots defining auto-dev@1 differently activated; first-root-wins " +
			"would make an operator's edit lose silently")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("the refusal is %q, want CONFLICT", code)
	}
	msg := err.Error()
	for _, want := range []string{
		"auto-dev@1", canonical(t, sharedPath), canonical(t, repoPath),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}

	// A HARD refusal: the activation wrote nothing.
	if n := countRows(t, conn, "workflows"); n != 0 {
		t.Errorf("%d workflow rows survived a refused activation, want 0", n)
	}
}

// TestUnionDifferingPinnedRefRefuses is the pinned half: one config-relative
// ref, two roots, different bytes.
//
// An ambiguous ref must never activate. It is the string a `packet` entry
// declares and the string the pin row records, so two candidates would mean a
// run whose packet contents depend on which directory the engine looked in
// first — reproducible only by accident.
func TestUnionDifferingPinnedRefRefuses(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	sharedPath := writeConfigFile(t, shared, "contracts/fix.md", "the corpus's version\n")
	repoPath := writeConfigFile(t, repo, "contracts/fix.md", "the repository's version\n")

	issue := createIssue(t, conn, "ambiguous ref", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("an ambiguous ref activated; a packet entry would then resolve to " +
			"whichever root came first")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("the refusal is %q, want CONFLICT", code)
	}
	msg := err.Error()
	for _, want := range []string{
		"contracts/fix.md", canonical(t, sharedPath), canonical(t, repoPath),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// The installed shape: a root that is a symlink
// ---------------------------------------------------------------------------

// TestSharedRootSymlinkScansLikeARealDirectory is the Vorpal install shape, and
// it is the case the old scanner got wrong in a way nothing reported.
//
// `~/.docket/config` is a SYMLINK to a real directory. filepath.WalkDir does not
// follow a symlinked root — it classifies the link as a leaf, and the walk then
// died inside filePin's os.ReadFile with EISDIR. Canonicalizing the root before
// the walk is the whole fix, and this asserts the result is byte-identical to
// what a real directory produces.
func TestSharedRootSymlinkScansLikeARealDirectory(t *testing.T) {
	conn, shared, _ := unionRepo(t)

	// The real corpus, somewhere else entirely, with ONE link pointing at it.
	real := filepath.Join(t.TempDir(), "corpus", "config")
	writeConfigFile(t, real, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, real, "contracts/fix.md", "a contract\n")

	err := os.MkdirAll(filepath.Dir(shared), 0o755)
	testsupport.Must(t, err, "creating the store directory: %v", err)
	err = os.Symlink(real, shared)
	testsupport.Must(t, err, "linking the shared root: %v", err)

	issue := createIssue(t, conn, "linked corpus", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "a symlinked config root failed to scan: %v", err)

	if len(result.Registered) != 1 {
		t.Fatalf("registered %d files through the link, want 1: %+v",
			len(result.Registered), result.Registered)
	}
	if got, want := result.Registered[0].SHA256, workflow.SHA256([]byte(autoWorkflowSrc)); got != want {
		t.Errorf("registered sha256 %s through the link, want %s — the same bytes a "+
			"real root registers", got, want)
	}
	if result.PinsFromConfig != 1 {
		t.Errorf("pinned %d files through the link, want the one contract",
			result.PinsFromConfig)
	}
	// The recorded path is the CANONICAL one, which is what makes a pin's
	// root-relative ref computable at all.
	if wantPrefix := canonical(t, real); !strings.HasPrefix(result.Registered[0].Path, wantPrefix) {
		t.Errorf("recorded path %q, want it under the resolved root %q",
			result.Registered[0].Path, wantPrefix)
	}
}

// TestDanglingConfigRootSymlinkRefuses: a link that resolves NOWHERE is loud.
//
// Lstat succeeds where Stat does not, so a broken link is distinguishable from
// plain absence — and it must be, because treating it as absence is how a
// half-finished install becomes a run that registers nothing and reports
// success.
func TestDanglingConfigRootSymlinkRefuses(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, repo, "workflows/auto-dev.toml", autoWorkflowSrc)

	err := os.MkdirAll(filepath.Dir(shared), 0o755)
	testsupport.Must(t, err, "creating the store directory: %v", err)
	target := filepath.Join(t.TempDir(), "never-installed")
	err = os.Symlink(target, shared)
	testsupport.Must(t, err, "linking the shared root: %v", err)

	issue := createIssue(t, conn, "broken install", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("a dangling config-root symlink was treated as dormancy; a broken " +
			"install must not look like having no config")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("the refusal is %q, want VALIDATION_ERROR", code)
	}
	msg := err.Error()
	for _, want := range []string{shared, target} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}
}

// TestOneDirectoryReachedTwiceIsOneRoot: two configured roots that CANONICALIZE
// to the same directory are scanned once.
//
// The plain string comparison config.InstanceConfigDirs does cannot see this —
// HOME arrives as typed while the exec root is canonicalized, so one directory
// can appear under two spellings. Scanning it twice would make every file its
// own duplicate and turn a legal install into a self-conflict.
func TestOneDirectoryReachedTwiceIsOneRoot(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, repo, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, repo, "contracts/fix.md", "a contract\n")

	err := os.MkdirAll(filepath.Dir(shared), 0o755)
	testsupport.Must(t, err, "creating the store directory: %v", err)
	err = os.Symlink(repo, shared)
	testsupport.Must(t, err, "pointing the shared root at the repo's: %v", err)

	issue := createIssue(t, conn, "one root, two names", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "one directory reached twice conflicted with itself: %v", err)
	if len(result.Registered) != 1 || result.PinsFromConfig != 1 {
		t.Errorf("registered=%d pinned=%d, want 1 and 1 — the two roots are one directory",
			len(result.Registered), result.PinsFromConfig)
	}
}

// TestConfigRootThatIsAFileRefusesPerRoot: the "a FILE named config" refusal
// still applies, and it applies to EACH root rather than only the first.
func TestConfigRootThatIsAFileRefusesPerRoot(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)

	err := os.MkdirAll(filepath.Dir(repo), 0o755)
	testsupport.Must(t, err, "creating the repo store directory: %v", err)
	err = os.WriteFile(repo, []byte("not a directory\n"), 0o644)
	testsupport.Must(t, err, "writing the file: %v", err)

	issue := createIssue(t, conn, "config is a file", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("a regular file standing where the second root belongs was ignored; " +
			"an operator who created one meant something")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("the refusal is %q, want VALIDATION_ERROR", code)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("the refusal does not say what is wrong:\n%s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Resolution: precedence, and the portability it buys
// ---------------------------------------------------------------------------

// TestPacketResolutionPrefersTheFirstRoot exercises the precedence rule
// directly: two roots hold the same ref, and the FIRST one's bytes are what
// resolve.
//
// Activation refuses this state (TestUnionDifferingPinnedRefRefuses), which is
// exactly why the rule is safe — but the resolver must still be DEFINITE about
// it rather than depending on which directory the filesystem answered first.
func TestPacketResolutionPrefersTheFirstRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFixture(t, first, "checklists/a.md", "FROM THE SHARED ROOT\n")
	writeFixture(t, second, "checklists/a.md", "FROM THE REPO ROOT\n")

	// Pinned as the FIRST root's bytes — what activation would have recorded.
	pins := testPinSet(t, first, "checklists/a.md")
	pins["checklists/a.md"] = pins[filepath.Join(first, "checklists/a.md")]

	files, err := resolvePacketFiles(pins, []string{first, second},
		[]string{"checklists/a.md"})
	testsupport.Must(t, err, "resolvePacketFiles: %v", err)
	if len(files) != 1 || strings.TrimSpace(files[0].Body) != "FROM THE SHARED ROOT" {
		t.Errorf("resolved %+v, want the FIRST root's bytes", files)
	}
}

// TestPacketResolutionFallsThroughToALaterRoot: a ref the first root does not
// hold resolves from the second, which is what makes the union a union at
// resolution and not only at activation.
func TestPacketResolutionFallsThroughToALaterRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFixture(t, second, "checklists/a.md", "ONLY IN THE SECOND ROOT\n")

	pins := testPinSet(t, second, "checklists/a.md")
	pins["checklists/a.md"] = pins[filepath.Join(second, "checklists/a.md")]

	files, err := resolvePacketFiles(pins, []string{first, second},
		[]string{"checklists/a.md"})
	testsupport.Must(t, err, "resolvePacketFiles: %v", err)
	if len(files) != 1 || strings.TrimSpace(files[0].Body) != "ONLY IN THE SECOND ROOT" {
		t.Errorf("resolved %+v, want the second root's file", files)
	}
}

// TestSharedRootPacketResolvesFromALinkedWorktree is the portability
// consequence, live-fired against a real `git worktree add`.
//
// THIS IS THE BUG THE CHANGE EXISTS FOR. A run activated in the main checkout
// pins `contracts/fix.md`; a session working in a linked worktree — which has no
// `.docket/` of its own and never will — claims one of its steps and renders it.
// Before the union, the render failed with `packet file "…" is pinned by this run
// but is no longer on disk`, and it failed AFTER the claim recorded, stranding a
// claimed step nobody held a token for.
func TestSharedRootPacketResolvesFromALinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	conn, shared, _ := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, shared, "contracts/fix.md", "the corpus's contract\n")

	// The main checkout is the cwd unionRepo chdir'd into.
	main, err := os.Getwd()
	testsupport.Must(t, err, "Getwd: %v", err)
	gitInitRepo(t, main)
	mustGitCmd(t, main, "commit", "--allow-empty", "-m", "seed")
	worktree := filepath.Join(t.TempDir(), "wt")
	mustGitCmd(t, main, "worktree", "add", worktree)

	issue := createIssue(t, conn, "portable packet", "body", "task", nil)
	run := startRun(t, conn, issue)
	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if result.PinsFromConfig != 1 {
		t.Fatalf("pinned %d config files, want the one contract", result.PinsFromConfig)
	}

	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)

	// MOVE INTO THE WORKTREE. It carries no `.docket/` at all, so the only root
	// that can answer is the shared one.
	t.Chdir(worktree)
	if _, err := os.Stat(filepath.Join(worktree, ".docket")); !os.IsNotExist(err) {
		t.Fatalf("premise: the worktree must carry no .docket (stat: %v)", err)
	}

	files, err := resolvePacketFiles(
		packetPinsForRun(pins), instanceConfigRoots(), []string{"contracts/fix.md"})
	testsupport.Must(t, err, "a packet file in the shared root did not resolve from a "+
		"linked worktree — the failure that stranded a claimed step: %v", err)

	if len(files) != 1 || strings.TrimSpace(files[0].Body) != "the corpus's contract" {
		t.Errorf("resolved %+v, want the corpus's contract inlined", files)
	}
}

// TestRepoRootPacketStillResolves is the twin: a repository's OWN pinned file
// resolves from that repository, unchanged by the shared root sitting ahead of
// it in the list.
func TestRepoRootPacketStillResolves(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, repo, "contracts/project.md", "this repo's contract\n")

	issue := createIssue(t, conn, "repo packet", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)

	files, err := resolvePacketFiles(
		packetPinsForRun(pins), instanceConfigRoots(), []string{"contracts/project.md"})
	testsupport.Must(t, err, "resolvePacketFiles: %v", err)
	if len(files) != 1 || strings.TrimSpace(files[0].Body) != "this repo's contract" {
		t.Errorf("resolved %+v, want the repository's own contract", files)
	}
}

func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	mustGitCmd(t, dir, "init", "-q")
	mustGitCmd(t, dir, "config", "user.email", "test@example.invalid")
	mustGitCmd(t, dir, "config", "user.name", "test")
}

func mustGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
