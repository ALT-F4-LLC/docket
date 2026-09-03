package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket doctor` — DKT-1285.

// ---------------------------------------------------------------------------
// checkDoctorSeat — check 1
// ---------------------------------------------------------------------------

func TestCheckDoctorSeatOKAtToplevel(t *testing.T) {
	repo := probeRepo(t)
	c := checkDoctorSeat(repo)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK: %s", c.Verdict, c.Detail)
	}
}

func TestCheckDoctorSeatFailsFromSubdirectory(t *testing.T) {
	repo := probeRepo(t)
	sub := filepath.Join(repo, "internal")
	testsupport.Must(t, os.MkdirAll(sub, 0o755), "mkdir: %v", nil)

	c := checkDoctorSeat(sub)
	if c.Verdict != DoctorFail {
		t.Errorf("verdict = %s, want FAIL from a subdirectory", c.Verdict)
	}
	if !strings.Contains(c.Detail, repo) && !strings.Contains(c.Detail, doctorCanonicalPath(repo)) {
		t.Errorf("detail %q does not name the real toplevel", c.Detail)
	}
}

func TestCheckDoctorSeatFailsOutsideARepo(t *testing.T) {
	dir := t.TempDir()
	c := checkDoctorSeat(dir)
	if c.Verdict != DoctorFail {
		t.Errorf("verdict = %s, want FAIL outside a git repository", c.Verdict)
	}
}

// ---------------------------------------------------------------------------
// checkDoctorStore — check 2
// ---------------------------------------------------------------------------

func TestCheckDoctorStoreOKForAWritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issues.db")
	testsupport.Must(t, os.WriteFile(path, nil, 0o644), "seeding a file: %v", nil)

	c := checkDoctorStore(path)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK: %s", c.Verdict, c.Detail)
	}
}

func TestCheckDoctorStoreFailsForAMissingPath(t *testing.T) {
	c := checkDoctorStore(filepath.Join(t.TempDir(), "nonexistent", "issues.db"))
	if c.Verdict != DoctorFail {
		t.Errorf("verdict = %s, want FAIL for a path that does not open", c.Verdict)
	}
}

func TestCheckDoctorStoreFailsWithNoPath(t *testing.T) {
	c := checkDoctorStore("")
	if c.Verdict != DoctorFail {
		t.Errorf("verdict = %s, want FAIL with no database path resolved", c.Verdict)
	}
}

// ---------------------------------------------------------------------------
// checkDoctorInstallDrift — check 3
// ---------------------------------------------------------------------------

func TestCheckDoctorInstallDriftSkipsWithoutSource(t *testing.T) {
	c := checkDoctorInstallDrift("")
	if c.Verdict != DoctorSkip {
		t.Errorf("verdict = %s, want SKIP with no --source", c.Verdict)
	}
}

func TestCheckDoctorInstallDriftMatchesIdenticalTrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, home, ".docket/config/workflows/foo.toml", "same\n")

	source := t.TempDir()
	writeFile(t, source, "config/workflows/foo.toml", "same\n")

	c := checkDoctorInstallDrift(source)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK for byte-identical trees: %s", c.Verdict, c.Detail)
	}
}

func TestCheckDoctorInstallDriftReportsAChangedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, home, ".docket/config/workflows/foo.toml", "installed\n")

	source := t.TempDir()
	writeFile(t, source, "config/workflows/foo.toml", "source\n")

	c := checkDoctorInstallDrift(source)
	if c.Verdict != DoctorDrift {
		t.Errorf("verdict = %s, want DRIFT for a changed file", c.Verdict)
	}
	if !strings.Contains(c.Detail, "config/workflows/foo.toml") {
		t.Errorf("detail %q does not name the drifted file", c.Detail)
	}
}

func TestCheckDoctorInstallDriftReportsAMissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Nothing installed under ~/.docket/bin.

	source := t.TempDir()
	writeFile(t, source, "bin/tool.sh", "#!/bin/sh\n")

	c := checkDoctorInstallDrift(source)
	if c.Verdict != DoctorDrift {
		t.Errorf("verdict = %s, want DRIFT for a file only the source has", c.Verdict)
	}
}

// TestCheckDoctorInstallDriftDisregardsOneSidedEmptyDir is AC1's own
// vocabulary: a directory present on one side only, holding no file anywhere
// under it, is named but never counted as drift.
func TestCheckDoctorInstallDriftDisregardsOneSidedEmptyDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, home, ".docket/config/workflows/foo.toml", "same\n")

	source := t.TempDir()
	writeFile(t, source, "config/workflows/foo.toml", "same\n")
	// An empty bin/ directory on the source side only — no file under it.
	testsupport.Must(t,
		os.MkdirAll(filepath.Join(source, "bin"), 0o755), "mkdir bin: %v", nil)

	c := checkDoctorInstallDrift(source)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK — an empty one-sided directory is not drift: %s",
			c.Verdict, c.Detail)
	}
	if !strings.Contains(c.Detail, "bin") {
		t.Errorf("detail %q does not name the disregarded directory", c.Detail)
	}
}

// ---------------------------------------------------------------------------
// checkDoctorPins — check 4
// ---------------------------------------------------------------------------

func TestCheckDoctorPinsSkipsWithoutRun(t *testing.T) {
	conn := mustDB(t)
	c := checkDoctorPins(conn, 0)
	if c.Verdict != DoctorSkip {
		t.Errorf("verdict = %s, want SKIP with no --run", c.Verdict)
	}
}

func TestCheckDoctorPinsOKForASoundRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	c := checkDoctorPins(conn, run.ID)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK for a freshly activated run: %s", c.Verdict, c.Detail)
	}
}

// ---------------------------------------------------------------------------
// checkDoctorLinkFarm — check 5
// ---------------------------------------------------------------------------

func TestCheckDoctorLinkFarmOKWithNoConfigDir(t *testing.T) {
	c := checkDoctorLinkFarm(t.TempDir())
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK with no .docket/config at all", c.Verdict)
	}
}

func TestCheckDoctorLinkFarmFailsOnADanglingSymlink(t *testing.T) {
	cwd := t.TempDir()
	configDir := filepath.Join(cwd, ".docket", "config")
	testsupport.Must(t, os.MkdirAll(configDir, 0o755), "mkdir: %v", nil)
	testsupport.Must(t,
		os.Symlink(filepath.Join(cwd, "nowhere"), filepath.Join(configDir, "dangling")),
		"symlink: %v", nil)

	c := checkDoctorLinkFarm(cwd)
	if c.Verdict != DoctorFail {
		t.Errorf("verdict = %s, want FAIL on a dangling symlink", c.Verdict)
	}
	if !strings.Contains(c.Detail, "dangling") {
		t.Errorf("detail %q does not name the dangling entry", c.Detail)
	}
}

func TestCheckDoctorLinkFarmOKOnALiveSymlink(t *testing.T) {
	cwd := t.TempDir()
	configDir := filepath.Join(cwd, ".docket", "config")
	testsupport.Must(t, os.MkdirAll(configDir, 0o755), "mkdir: %v", nil)
	target := filepath.Join(cwd, "target.toml")
	testsupport.Must(t, os.WriteFile(target, []byte("x"), 0o644), "writing target: %v", nil)
	testsupport.Must(t,
		os.Symlink(target, filepath.Join(configDir, "live")), "symlink: %v", nil)

	c := checkDoctorLinkFarm(cwd)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK — the symlink resolves: %s", c.Verdict, c.Detail)
	}
}

// ---------------------------------------------------------------------------
// checkDoctorStragglers — check 6
// ---------------------------------------------------------------------------

// TestCheckDoctorStragglersReportsAScratchWorktreeButNeverFails is AC3's own
// check-level half: a detached worktree under a scratch (temp) path is named
// in the detail, and the verdict is ALWAYS OK — a report, never a verdict.
func TestCheckDoctorStragglersReportsAScratchWorktreeButNeverFails(t *testing.T) {
	repo := probeRepo(t)

	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	scratch, err := os.MkdirTemp("", "docket-pregate-")
	testsupport.Must(t, err, "MkdirTemp: %v", err)
	testsupport.Must(t, os.Remove(scratch), "removing the reserved dir: %v", err)
	t.Cleanup(func() { os.RemoveAll(scratch) })
	runGit(t, repo, "worktree", "add", "--detach", scratch, head)
	t.Cleanup(func() { runGit(t, repo, "worktree", "remove", "--force", scratch) })

	c := checkDoctorStragglers(repo)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK always — stragglers are a report, not a verdict", c.Verdict)
	}
	if !strings.Contains(c.Detail, doctorCanonicalPath(scratch)) && !strings.Contains(c.Detail, scratch) {
		t.Errorf("detail %q does not name the scratch worktree %s", c.Detail, scratch)
	}
}

func TestCheckDoctorStragglersOKWithNone(t *testing.T) {
	repo := probeRepo(t)
	c := checkDoctorStragglers(repo)
	if c.Verdict != DoctorOK {
		t.Errorf("verdict = %s, want OK", c.Verdict)
	}
}

// ---------------------------------------------------------------------------
// Doctor — the composed report
// ---------------------------------------------------------------------------

// doctorFixture builds a repo whose toplevel doubles as a docket store: a real
// database at <repo>/issues.db, so checkDoctorStore and checkDoctorSeat can
// both answer OK against the SAME cwd.
func doctorFixture(t *testing.T) (repo string, conn *sql.DB) {
	t.Helper()
	repo = probeRepo(t)
	path := filepath.Join(repo, "issues.db")
	var err error
	conn, err = db.Open(path)
	testsupport.Must(t, err, "opening database: %v", err)
	t.Cleanup(func() { conn.Close() })
	testsupport.Must(t, db.Initialize(conn), "Initialize: %v", nil)
	testsupport.Must(t, db.Migrate(conn), "Migrate: %v", nil)
	return repo, conn
}

// TestDoctorRunsEverySixWithoutShortCircuiting is AC1: one row per check, all
// six present, whatever the individual verdicts — including check 1 (seat)
// failing, which does not stop the rest from running.
func TestDoctorRunsEverySixWithoutShortCircuiting(t *testing.T) {
	repo, conn := doctorFixture(t)
	sub := filepath.Join(repo, "internal")
	testsupport.Must(t, os.MkdirAll(sub, 0o755), "mkdir: %v", nil)

	report := Doctor(conn, DoctorOptions{
		Cwd: sub, DBPath: filepath.Join(repo, "issues.db"), NowMS: nowMS,
	})

	want := []string{"seat", "store", "install-drift", "pins", "link-farm", "stragglers"}
	if len(report.Checks) != len(want) {
		t.Fatalf("checks = %v, want %d rows", report.Checks, len(want))
	}
	for i, name := range want {
		if report.Checks[i].Check != name {
			t.Errorf("checks[%d] = %q, want %q", i, report.Checks[i].Check, name)
		}
	}
	if report.Checks[0].Verdict != DoctorFail {
		t.Errorf("seat verdict = %s, want FAIL from a subdirectory", report.Checks[0].Verdict)
	}
	// The seat FAILING did not stop the rest: `store` still answers.
	if report.Checks[1].Verdict != DoctorOK {
		t.Errorf("store verdict = %s, want OK — a seat failure must not short-circuit it",
			report.Checks[1].Verdict)
	}
}

// TestDoctorNoRunSkipsPinsAndReportsUnclean is AC2, verbatim.
func TestDoctorNoRunSkipsPinsAndReportsUnclean(t *testing.T) {
	repo, conn := doctorFixture(t)

	report := Doctor(conn, DoctorOptions{
		Cwd: repo, DBPath: filepath.Join(repo, "issues.db"), NowMS: nowMS,
	})

	var pins *DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Check == "pins" {
			pins = &report.Checks[i]
		}
	}
	if pins == nil || pins.Verdict != DoctorSkip {
		t.Fatalf("pins = %+v, want SKIP without --run", pins)
	}
	if report.Clean {
		t.Error("AC2: clean = true without --run, want false")
	}
	if !report.Skipped {
		t.Error("AC2: skipped = false without --run, want true")
	}
}

// TestDoctorStragglersNeverMoveClean is AC3, verbatim: a straggler worktree
// under a scratch path does not, by itself, make an otherwise-clean report
// unclean.
func TestDoctorStragglersNeverMoveClean(t *testing.T) {
	repo, conn := doctorFixture(t)
	run, _ := activatedRun(t, conn)

	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	scratch, err := os.MkdirTemp("", "docket-pregate-")
	testsupport.Must(t, err, "MkdirTemp: %v", err)
	testsupport.Must(t, os.Remove(scratch), "removing the reserved dir: %v", err)
	t.Cleanup(func() { os.RemoveAll(scratch) })
	runGit(t, repo, "worktree", "add", "--detach", scratch, head)
	t.Cleanup(func() { runGit(t, repo, "worktree", "remove", "--force", scratch) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	source := t.TempDir()

	report := Doctor(conn, DoctorOptions{
		Cwd: repo, DBPath: filepath.Join(repo, "issues.db"),
		RunID: run.ID, SourceRoot: source, NowMS: nowMS,
	})

	var strays *DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Check == doctorCheckStragglers {
			strays = &report.Checks[i]
		}
	}
	if strays == nil || !strings.Contains(strays.Detail, "1 detached") {
		t.Fatalf("premise: the straggler must be reported: %+v", strays)
	}
	if !report.Clean {
		t.Errorf("AC3: a reported straggler made an otherwise-clean report unclean: %+v",
			report.Checks)
	}
}

// TestDoctorWritesNothing is AC4: no row count anywhere changes.
func TestDoctorWritesNothing(t *testing.T) {
	repo, conn := doctorFixture(t)
	run, _ := activatedRun(t, conn)

	before := countRows(t, conn, "runs") + countRows(t, conn, "steps") +
		countRows(t, conn, "events") + countRows(t, conn, "reap_acks")

	Doctor(conn, DoctorOptions{
		Cwd: repo, DBPath: filepath.Join(repo, "issues.db"), RunID: run.ID, NowMS: nowMS,
	})

	after := countRows(t, conn, "runs") + countRows(t, conn, "steps") +
		countRows(t, conn, "events") + countRows(t, conn, "reap_acks")
	if before != after {
		t.Errorf("row counts changed: before %d, after %d", before, after)
	}
}
