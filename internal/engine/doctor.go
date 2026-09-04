package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// `docket doctor` — DKT-1285.
//
// Every conductor attaching to a run cleared the same six checks by hand
// before this existed: a diff piped through `head -30` reported `head`'s exit
// rather than the diff's, and an existence test stood in where a byte-diff was
// mandated. The probe lived in dotfiles as attach-probe.js, spending six
// read-only agents per attach. Doctor is that probe, engine-native and
// composed from what already exists: `run verify-pins` for pins, and five new
// checks for the rest.
//
// READ-ONLY, AND IT WRITES NOTHING — no lease reap, no re-pin, no migration
// beyond what any read verb performs. Every check below either shells out to
// `git` (read commands only), reads the filesystem, or calls VerifyPins,
// which is read-only by its own contract.

// DoctorVerdict is one check's answer.
type DoctorVerdict string

const (
	DoctorOK    DoctorVerdict = "OK"
	DoctorFail  DoctorVerdict = "FAIL"
	DoctorDrift DoctorVerdict = "DRIFT"
	DoctorSkip  DoctorVerdict = "SKIP"
	DoctorWarn  DoctorVerdict = "WARN"
)

// DoctorCheck is one row: which check, its verdict, and a human-readable
// reason — the same shape whether the caller reads JSON or the rendered table.
type DoctorCheck struct {
	Check   string
	Verdict DoctorVerdict
	Detail  string
}

// doctorCheckStragglers names the one check AC3 excludes from `clean`: a
// straggler worktree is a report, never a verdict that blocks an attach.
const doctorCheckStragglers = "stragglers"

// DoctorReport is `doctor`'s whole answer: one row per check, plus the two
// summary bits a caller branches on without re-walking the rows.
type DoctorReport struct {
	Clean   bool
	Skipped bool
	Checks  []DoctorCheck
}

// DoctorOptions are one invocation's inputs.
type DoctorOptions struct {
	// Cwd is the invoking process's working directory, canonicalized.
	Cwd string
	// DBPath is the store's database file — the same path the current seat
	// already opened read-write to reach this verb, re-verified directly.
	DBPath string
	// RunID is `--run`'s target, or 0 when absent — AC2's SKIP case.
	RunID int
	// SourceRoot is `--source PATH`, or "" when absent — install-drift's own
	// SKIP case.
	SourceRoot string
	NowMS      int64
}

// Doctor runs all six checks and reports one row each, without
// short-circuiting on an early failure (AC1): a conductor deciding whether to
// attach needs every answer in one call, not the first one that went wrong.
func Doctor(conn *sql.DB, opts DoctorOptions) *DoctorReport {
	checks := []DoctorCheck{
		checkDoctorSeat(opts.Cwd),
		checkDoctorStore(opts.DBPath),
		checkDoctorInstallDrift(opts.SourceRoot),
		checkDoctorPins(conn, opts.RunID),
		checkDoctorLinkFarm(opts.Cwd),
		checkDoctorStragglers(opts.Cwd),
	}
	report := &DoctorReport{Checks: checks}
	report.Clean, report.Skipped = doctorDisposition(checks)
	return report
}

// doctorDisposition is AC2 and AC3, matching attach-probe.js's own reading
// exactly: a check may WARN without moving `clean` — WARN is a fact worth
// surfacing, not a failure — but a SKIP does, the same as a FAIL or a DRIFT.
// `stragglers` is excluded outright (AC3): a report, never a verdict.
func doctorDisposition(checks []DoctorCheck) (clean, skipped bool) {
	ranClean := true
	for _, c := range checks {
		if c.Check == doctorCheckStragglers {
			continue
		}
		switch c.Verdict {
		case DoctorSkip:
			skipped = true
		case DoctorOK, DoctorWarn:
			// Clean-compatible: OK is soundness itself, and WARN is a
			// caveat the reader is told about rather than a fault.
		default: // FAIL, DRIFT
			ranClean = false
		}
	}
	return ranClean && !skipped, skipped
}

// checkDoctorSeat is check 1: cwd is the git toplevel.
//
// A conductor working from a subdirectory has every relative-path assumption
// the rest of the six checks (and every other verb) make silently wrong, and
// the old probe caught this with a diff piped through `head -30` that reported
// `head`'s exit code instead of the diff's — silently passing every time.
func checkDoctorSeat(cwd string) DoctorCheck {
	const check = "seat"
	out, err := osexec.Command("git", gitDirArgs(cwd, "rev-parse", "--show-toplevel")...).Output()
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail,
			Detail: fmt.Sprintf("%s is not inside a git repository", cwd)}
	}
	// Canonicalize BOTH sides before comparing: git reports resolved paths
	// (macOS: /var vs /private/var) and a bare cwd would not, so an unresolved
	// symlink in either would read as "not the toplevel" when it is.
	toplevel := doctorCanonicalPath(strings.TrimSpace(string(out)))
	if toplevel != doctorCanonicalPath(cwd) {
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: fmt.Sprintf(
			"cwd %s is not the git toplevel; the toplevel is %s", cwd, toplevel)}
	}
	return DoctorCheck{Check: check, Verdict: DoctorOK, Detail: "cwd is the git toplevel"}
}

// checkDoctorStore is check 2: the store opens read-write from this seat.
//
// It opens the database FILE directly with O_RDWR and closes it immediately —
// the literal question the issue asks, answered without any SQL that could
// itself be mistaken for a write.
func checkDoctorStore(dbPath string) DoctorCheck {
	const check = "store"
	if dbPath == "" {
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: "no database path resolved"}
	}
	f, err := os.OpenFile(dbPath, os.O_RDWR, 0)
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: fmt.Sprintf(
			"%s does not open read-write: %v", dbPath, err)}
	}
	f.Close()
	return DoctorCheck{Check: check, Verdict: DoctorOK, Detail: dbPath + " opens read-write"}
}

// checkDoctorInstallDrift is check 3: the shared config root and bin match
// --source's tree.
//
// --source names a dotfiles-shaped checkout root; the two trees it ships are
// at src/user/docket/config and src/user/docket/bin, the same subpath the
// corpus this ports from (attach-probe.js) compares. The shared roots are
// ~/.docket/config and ~/.docket/bin — the corpus every project on the
// machine draws from (see config.Config.InstanceConfigDirs) — compared byte
// for byte. A directory present on one side only, and holding no file
// anywhere under it, is DISREGARDED as drift and NAMED in the detail instead:
// there are no bytes on either side to disagree about, and reporting it as
// DRIFT would flag an empty placeholder the same as a real divergence. A
// TREE MISSING ENTIRELY ON EITHER SIDE is FAIL, not disregarded: an absent
// config or bin directory is not a placeholder, it is nothing installed (or
// nothing shipped) to compare at all.
func checkDoctorInstallDrift(sourceRoot string) DoctorCheck {
	const check = "install-drift"
	if sourceRoot == "" {
		return DoctorCheck{Check: check, Verdict: DoctorSkip, Detail: "no --source given"}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail,
			Detail: fmt.Sprintf("resolving the home directory: %v", err)}
	}
	sharedRoot := filepath.Join(home, ".docket")

	var drifted, disregarded, missing []string
	for _, sub := range []string{"config", "bin"} {
		srcDir := filepath.Join(sourceRoot, "src", "user", "docket", sub)
		installDir := filepath.Join(sharedRoot, sub)

		switch {
		case !doctorDirExists(srcDir):
			missing = append(missing, srcDir+" (source)")
		case !doctorDirExists(installDir):
			missing = append(missing, installDir+" (install)")
		default:
			d, empty, err := diffDoctorTree(srcDir, installDir, sub)
			if err != nil {
				return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: err.Error()}
			}
			drifted = append(drifted, d...)
			disregarded = append(disregarded, empty...)
		}
	}

	if len(missing) > 0 {
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: fmt.Sprintf(
			"missing entirely, nothing to compare: %v", missing)}
	}
	if len(drifted) > 0 {
		return DoctorCheck{Check: check, Verdict: DoctorDrift, Detail: fmt.Sprintf(
			"%d file(s) differ from %s: %v", len(drifted), sourceRoot, drifted)}
	}
	detail := "matches " + sourceRoot
	if len(disregarded) > 0 {
		detail += fmt.Sprintf("; one-sided empty director(ies) disregarded: %v", disregarded)
	}
	return DoctorCheck{Check: check, Verdict: DoctorOK, Detail: detail}
}

// diffDoctorTree compares two directories' regular files by content, labeling
// every reported path with sub (e.g. "config/workflows/foo.toml"). Both roots
// are already known to exist (checkDoctorInstallDrift's own FAIL branch
// handles the case where one is missing entirely) — this only compares what
// is under them.
//
// It returns the drifted (differing, or present on one side only) file paths
// and, separately, any SUBDIRECTORY present on one side only that holds no
// file anywhere under it — the "disregard and name" case
// checkDoctorInstallDrift documents, matching `diff -rq`'s own "Only in ..."
// reporting at any depth rather than only at the two roots themselves.
func diffDoctorTree(a, b, sub string) (drifted, disregarded []string, err error) {
	filesA, dirsA, err := doctorTreeWalk(a)
	if err != nil {
		return nil, nil, err
	}
	filesB, dirsB, err := doctorTreeWalk(b)
	if err != nil {
		return nil, nil, err
	}

	seen := make(map[string]bool, len(filesA))
	for rel, sumA := range filesA {
		seen[rel] = true
		if sumB, ok := filesB[rel]; !ok || sumA != sumB {
			drifted = append(drifted, filepath.Join(sub, rel))
		}
	}
	for rel := range filesB {
		if !seen[rel] {
			drifted = append(drifted, filepath.Join(sub, rel))
		}
	}
	sort.Strings(drifted)

	holdsAFile := func(files map[string]string, dir string) bool {
		prefix := dir + string(filepath.Separator)
		for rel := range files {
			if strings.HasPrefix(rel, prefix) {
				return true
			}
		}
		return false
	}
	for dir := range dirsA {
		if !dirsB[dir] && !holdsAFile(filesA, dir) {
			disregarded = append(disregarded, filepath.Join(sub, dir))
		}
	}
	for dir := range dirsB {
		if !dirsA[dir] && !holdsAFile(filesB, dir) {
			disregarded = append(disregarded, filepath.Join(sub, dir))
		}
	}
	sort.Strings(disregarded)
	return drifted, disregarded, nil
}

// doctorTreeWalk walks root and returns every regular file's path (relative
// to root) mapped to its sha256, and every subdirectory's relative path (root
// itself excluded — its own existence is checkDoctorInstallDrift's concern,
// not diffDoctorTree's).
func doctorTreeWalk(root string) (files map[string]string, dirs map[string]bool, err error) {
	// An install activated from a content-addressed store leaves
	// ~/.docket/{config,bin} as symlinks into it, and WalkDir lstats its own
	// root: unresolved, such a root walks nothing at all and every source file
	// reads as drift.
	root = doctorCanonicalPath(root)
	files = map[string]string{}
	dirs = map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		if d.IsDir() {
			dirs[rel] = true
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// Symlinks are check 5's concern (link-farm debris), not a byte
			// comparison here — walking through one could also escape root.
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return files, dirs, nil
}

func doctorDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// checkDoctorPins is check 4: `run verify-pins` for --run, unchanged.
//
// Without --run there is no run to check pins for, and AC2 makes that
// explicit: SKIP, which doctorDisposition then reads as not-clean — a
// conductor who forgot --run is told so rather than shown a clean report that
// silently checked five things instead of six.
func checkDoctorPins(conn *sql.DB, runID int) DoctorCheck {
	const check = "pins"
	if runID == 0 {
		return DoctorCheck{Check: check, Verdict: DoctorSkip, Detail: "no --run given"}
	}
	report, err := VerifyPins(conn, runID)
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: err.Error()}
	}
	if report.Sound() {
		return DoctorCheck{Check: check, Verdict: DoctorOK,
			Detail: fmt.Sprintf("%d pin(s), all sound", len(report.Pins))}
	}
	// The three-way disposition mirrors `run verify-pins`' own exit code
	// table (runRunVerifyPins): changed bytes are the half a repin can fix,
	// missing bytes have nothing to adopt, and an open closure alone is
	// neither — a structural gap repin cannot touch.
	switch {
	case report.Changed > 0:
		return DoctorCheck{Check: check, Verdict: DoctorDrift, Detail: PinReportReason(report)}
	case report.Missing > 0:
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: PinReportReason(report)}
	default:
		return DoctorCheck{Check: check, Verdict: DoctorWarn, Detail: PinReportReason(report)}
	}
}

// checkDoctorLinkFarm is check 5: symlinks under <cwd>/.docket/config — the
// "link-farm debris" a retired install model left behind.
//
// A symlink there at all is the debris, whether or not it still resolves:
// that model put symlinks under a repo's own instance-config tree, and a
// present one is evidence of it whatever its current target is. Real files
// under the same directory are the repo's own additions and are not this
// check's concern.
func checkDoctorLinkFarm(cwd string) DoctorCheck {
	const check = "link-farm"
	root := filepath.Join(cwd, ".docket", "config")
	if !doctorDirExists(root) && !doctorIsSymlink(root) {
		return DoctorCheck{Check: check, Verdict: DoctorOK,
			Detail: root + " does not exist; nothing to check"}
	}

	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail,
			Detail: fmt.Sprintf("walking %s: %v", root, err)}
	}
	if len(found) > 0 {
		sort.Strings(found)
		return DoctorCheck{Check: check, Verdict: DoctorDrift, Detail: fmt.Sprintf(
			"%d symlink(s) under %s from the retired link-farm model: %v", len(found), root, found)}
	}
	return DoctorCheck{Check: check, Verdict: DoctorOK, Detail: "no symlinks under " + root}
}

func doctorIsSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// isScratchShapedPath reports whether path is homed under a scratch
// directory a session's own worktree can be left behind in — the same rule
// attach-probe.js's stragglers check uses: a `/scratchpad` path component, or
// a path rooted at /tmp/claude- or /private/tmp/claude- (a Claude session's
// own temp root — macOS resolves /tmp through /private).
func isScratchShapedPath(path string) bool {
	if strings.HasPrefix(path, "/tmp/claude-") || strings.HasPrefix(path, "/private/tmp/claude-") {
		return true
	}
	return slices.Contains(strings.Split(path, string(filepath.Separator)), "scratchpad")
}

// checkDoctorStragglers is check 6: detached worktrees homed under a
// scratch-shaped path — left behind by a session (or a pre-gate
// reconstruction, pregate_scratch.go) that never cleaned up after itself (a
// crash, a killed process).
//
// AC3: THIS IS A REPORT, NEVER A VERDICT that blocks — it reads OK or WARN
// only, never FAIL, and doctorDisposition excludes it from `clean` on top of
// that, so a straggler can never itself block an attach. It exists to be
// SEEN, not acted on by this verb: reclaiming one is `git worktree prune`'s
// job (or the owning session's own close sweep), and doctor is read-only.
func checkDoctorStragglers(cwd string) DoctorCheck {
	out, err := osexec.Command("git", gitDirArgs(cwd, "worktree", "list", "--porcelain")...).Output()
	if err != nil {
		return DoctorCheck{Check: doctorCheckStragglers, Verdict: DoctorOK,
			Detail: "not inside a git repository; nothing to report"}
	}

	var strays []string
	var path string
	var detached bool
	flush := func() {
		if path != "" && detached && isScratchShapedPath(path) {
			strays = append(strays, path)
		}
		path, detached = "", false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "detached":
			detached = true
		}
	}
	flush()

	if len(strays) == 0 {
		return DoctorCheck{Check: doctorCheckStragglers, Verdict: DoctorOK,
			Detail: "no detached worktrees under a scratch path"}
	}
	sort.Strings(strays)
	return DoctorCheck{Check: doctorCheckStragglers, Verdict: DoctorWarn, Detail: fmt.Sprintf(
		"%d detached worktree(s) under a scratch path (report only): %v", len(strays), strays)}
}

// doctorCanonicalPath resolves symlinks best-effort, mirroring
// config.canonicalPath: it only has to be stable across the two paths a check
// compares, not perfect, and a path that does not resolve (not yet created,
// permission) is returned as-is.
func doctorCanonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
