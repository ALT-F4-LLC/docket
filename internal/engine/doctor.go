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
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/exec"
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

// doctorDisposition is AC2 and AC3: `clean` is true only when every check but
// `stragglers` reports OK, and a SKIP counts as not-clean the same as a
// failure — a caller that asked "is this seat sound" and got back "one check
// never ran" has not been told sound.
func doctorDisposition(checks []DoctorCheck) (clean, skipped bool) {
	clean = true
	for _, c := range checks {
		if c.Check == doctorCheckStragglers {
			continue
		}
		if c.Verdict == DoctorSkip {
			skipped = true
		}
		if c.Verdict != DoctorOK {
			clean = false
		}
	}
	return clean, skipped
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
// The shared roots are ~/.docket/config and ~/.docket/bin — the corpus every
// project on the machine draws from (see config.Config.InstanceConfigDirs) —
// compared against the SAME two names under --source, byte for byte. A
// directory present on one side only, and holding no file anywhere under it,
// is DISREGARDED as drift and NAMED in the detail instead: there are no bytes
// on either side to disagree about, and reporting it as DRIFT would flag an
// empty placeholder the same as a real divergence.
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

	var drifted, disregarded []string
	for _, sub := range []string{"config", "bin"} {
		d, empty, err := diffDoctorTree(filepath.Join(sourceRoot, sub), filepath.Join(sharedRoot, sub), sub)
		if err != nil {
			return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: err.Error()}
		}
		drifted = append(drifted, d...)
		disregarded = append(disregarded, empty...)
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
// every reported path with sub (e.g. "config/workflows/foo.toml").
//
// It returns the drifted (differing or one-sided non-empty) paths and,
// separately, sub itself when the two sides disagree on existence but neither
// holds a single file — the "disregard and name" case checkDoctorInstallDrift
// documents.
func diffDoctorTree(a, b, sub string) (drifted, disregarded []string, err error) {
	filesA, err := doctorTreeFiles(a)
	if err != nil {
		return nil, nil, err
	}
	filesB, err := doctorTreeFiles(b)
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

	if len(filesA) == 0 && len(filesB) == 0 && doctorDirExists(a) != doctorDirExists(b) {
		disregarded = append(disregarded, sub)
	}
	return drifted, disregarded, nil
}

// doctorTreeFiles walks root and returns every regular file's path (relative
// to root) mapped to its sha256. A root that does not exist, or is not a
// directory, reports an empty tree rather than an error: "not installed here"
// is a fact the diff already reads from the map being empty.
func doctorTreeFiles(root string) (map[string]string, error) {
	out := map[string]string{}
	if !doctorDirExists(root) {
		return out, nil
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			// Symlinks are check 5's concern (link-farm debris), not a byte
			// comparison here — walking through one could also escape root.
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	return out, nil
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

// checkDoctorLinkFarm is check 5: dangling symlinks under
// <cwd>/.docket/config — the "link-farm debris" a repo-local instance-config
// tree can accumulate once whatever it pointed at moves or is removed.
func checkDoctorLinkFarm(cwd string) DoctorCheck {
	const check = "link-farm"
	root := filepath.Join(cwd, ".docket", "config")
	if !doctorDirExists(root) && !doctorIsSymlink(root) {
		return DoctorCheck{Check: check, Verdict: DoctorOK,
			Detail: root + " does not exist; nothing to check"}
	}

	var dangling []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if _, statErr := os.Stat(path); statErr != nil {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			dangling = append(dangling, rel)
		}
		return nil
	})
	if err != nil {
		return DoctorCheck{Check: check, Verdict: DoctorFail,
			Detail: fmt.Sprintf("walking %s: %v", root, err)}
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		return DoctorCheck{Check: check, Verdict: DoctorFail, Detail: fmt.Sprintf(
			"%d dangling symlink(s) under %s: %v", len(dangling), root, dangling)}
	}
	return DoctorCheck{Check: check, Verdict: DoctorOK, Detail: "no dangling symlinks"}
}

func doctorIsSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// checkDoctorStragglers is check 6: detached worktrees homed under a
// scratch-shaped path — the temp directory pre-gate reconstruction uses
// (pregate_scratch.go's `os.MkdirTemp("", "docket-pregate-")`) — left behind
// by a `release()` that never ran (a crash, a killed process).
//
// AC3: THIS IS A REPORT, NEVER A VERDICT — it always reads OK, and
// doctorDisposition excludes it from `clean` on top of that, so a straggler
// can never itself block an attach. It exists to be SEEN, not acted on by this
// verb: reclaiming one is `git worktree prune`'s job, and doctor is read-only.
func checkDoctorStragglers(cwd string) DoctorCheck {
	out, err := osexec.Command("git", gitDirArgs(cwd, "worktree", "list", "--porcelain")...).Output()
	if err != nil {
		return DoctorCheck{Check: doctorCheckStragglers, Verdict: DoctorOK,
			Detail: "not inside a git repository; nothing to report"}
	}

	tmp := doctorCanonicalPath(os.TempDir())
	var strays []string
	var path string
	var detached bool
	flush := func() {
		if path != "" && detached && exec.Under(tmp, doctorCanonicalPath(path)) {
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
	return DoctorCheck{Check: doctorCheckStragglers, Verdict: DoctorOK, Detail: fmt.Sprintf(
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
