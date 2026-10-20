package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// gatePinStore is the trust store activation and verify-pins read. It is a
// variable so a test can supply a sandbox store; production always uses the
// real one, and there is no flag or environment variable that changes it.
var gatePinStore = trust.Load

// GateUnpinned is the status of a declared gate whose trusted argv names no
// file this run pinned. It is a report, not a refusal: it never makes
// PinReport.Sound false, because most trust entries invoke a build tool
// (`make <target>`) rather than a script, and failing every such run would
// turn a visibility feature into a blanket block.
const GateUnpinned = "unpinned"

// GateUnchecked is the status of a declared gate whose script could not be
// checked at all: the trust store is unreadable, the repo identity does not
// resolve, or no trust entry matches the gate. It is distinct from
// GateUnpinned so a report with nothing checked never reads as all pinned,
// and like GateUnpinned it never makes PinReport.Sound false.
const GateUnchecked = "unchecked"

// GateExecutedCopyChanged is the status of a gate whose script, named
// relative to the cwd, resolves under the invoking exec root to bytes other
// than the pinned ones: the copy a gate run from this checkout would execute
// is not the copy the run pinned. It makes PinReport.Sound false.
const GateExecutedCopyChanged = "executed-copy-changed"

// GateExecutedCopyAbsent is the status of a gate whose relatively named script
// does not exist under the invoking exec root. A gate run there cannot open
// it and fails rather than running other bytes, so it is reported without
// making PinReport.Sound false.
const GateExecutedCopyAbsent = "executed-copy-absent"

// GateStepCopyChanged is the status of a gate whose relatively named script
// resolves, under a work root a step of the run recorded, to bytes other than
// the pinned ones, or to a file that cannot be checked. A gate run in that
// work root would execute that copy. It never makes PinReport.Sound false,
// because an issue may legitimately edit a gate script.
const GateStepCopyChanged = "step-copy-changed"

// GateStepCopyAbsent is the status of a gate whose relatively named script
// does not exist under a work root a step of the run recorded, or of the one
// row a work root that no longer exists gets. Like GateStepCopyChanged it
// never makes PinReport.Sound false.
const GateStepCopyAbsent = "step-copy-absent"

// GateVerdict is one finding about a declared gate's script: the run does not
// pin it, it could not be checked, or a copy of it where a gate runs (the
// invoking exec root, a step's recorded work root) differs from the pin or is
// absent. Status names which.
type GateVerdict struct {
	Gate   string `json:"gate"`
	Status string `json:"status"`
	// Reason says why, naming the paths and steps involved, so an operator can
	// tell a `make` entry (retarget it to name the script) from a pin that was
	// never recorded (the run activated before gate scripts were pinned), or
	// an edited copy from a swept work root.
	Reason string `json:"reason"`
}

// gateInterpreters are the programs whose first non-flag argument is the
// script they run. Only for these is that argument treated as the gate's
// script: pinning every argv token would pin the files a run is meant to edit
// (`pytest tests/test_x.py`) and report them as drift.
var gateInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true,
	"python": true, "python3": true, "node": true, "ruby": true, "perl": true,
}

// maxGateScriptBytes bounds what a gate pin reads: a script, not a binary or a
// data fixture. A larger file is not treated as the gate's script.
const maxGateScriptBytes = 1 << 20

// gateScriptTokens is the argv tokens that can name the gate's script:
// argv[0], and, when argv[0] is an interpreter, its first non-flag argument.
func gateScriptTokens(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	toks := []string{argv[0]}
	if gateInterpreters[filepath.Base(argv[0])] && len(argv) > 1 &&
		argv[1] != "" && !strings.HasPrefix(argv[1], "-") {
		toks = append(toks, argv[1])
	}
	return toks
}

// repoLocalGateFiles returns the regular files under execRoot that a trusted
// argv names as its script, in argv order, as absolute symlink-resolved paths.
//
// "Repo-local" means relative to the exec root, or an absolute path inside it:
// a trust entry that invokes `bash /abs/repo/scripts/qa/x.sh` is how the
// ac-commands gate is wired, so an absolute path under the root must resolve.
// A path that resolves outside the root (a symlink out, `..`) is not
// repo-local, and neither is a bare program name such as `make`, which no file
// under the root answers to. See gateScriptTokens for which tokens count.
//
// It is a function of the argv and the tree, never of the trust store, so the
// pin recorded at activation and the check made by verify-pins cannot
// disagree about which files a gate names.
func repoLocalGateFiles(argv []string, execRoot string) []string {
	if execRoot == "" || !filepath.IsAbs(execRoot) {
		return nil
	}
	root, err := filepath.EvalSymlinks(execRoot)
	if err != nil {
		return nil
	}

	var out []string
	seen := map[string]bool{}
	for _, tok := range gateScriptTokens(argv) {
		candidate := tok
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(root, candidate)
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == "." || isOutsideRoot(rel) {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxGateScriptBytes {
			continue
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}
	return out
}

// declaredGateNames is every non-fence gate the definitions declare, sorted.
func declaredGateNames(defs map[int]*workflow.Definition) []string {
	set := map[string]bool{}
	for _, def := range defs {
		if def == nil {
			continue
		}
		for _, step := range def.Steps {
			for _, gate := range step.Gates {
				if _, isFence := fenceTag(gate); isFence || gate.Name == "" {
					continue
				}
				set[gate.Name] = true
			}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// gateTrustIdentity resolves a run's recorded project identity to the form
// trust entries bind to. An empty identity is an error rather than a path:
// trust.RepoIdentity would resolve "" to the process's cwd, which is the
// invoker's answer this identity replaces.
func gateTrustIdentity(projectIdentity string) (string, error) {
	if projectIdentity == "" {
		return "", errors.New("the run's project records no identity")
	}
	return trust.RepoIdentity(projectIdentity)
}

// matchedGateArgv is the argv of the trust entry a declared gate resolves to
// in this repo, or nil and the store's unmatched reason when no entry matches.
func matchedGateArgv(
	store *trust.Store, identity, gate string,
) ([]string, string) {
	m := store.Lookup(identity, gate, nil)
	if !m.Matched || m.Entry == nil {
		return nil, m.Reason
	}
	return m.Entry.Argv, ""
}

// gateScriptPins is the file pins a run records for the scripts its declared
// gates execute: for each declared gate with a matching trust entry, every
// repo-local file the entry's argv names, hashed now.
//
// The pin root is the run's recorded ExecRoot, the checkout the run was
// started in, read at activation before any step writes. It is the trusted
// baseline every other tree is compared to. Two other roots were rejected:
//   - The executing step's worktree does not exist at activation, and the
//     step's executor writes it, so pinning it would record the bytes the pin
//     exists to check.
//   - The checkout recorded per step (steps.work_root) is set when the step
//     records, after its executor wrote it, so it has the same flaw and is
//     also absent at activation.
//
// The executed copy is checked against the pin by verify-pins, not here. A
// relatively named script runs from the gate's cwd, so verify-pins hashes
// the same relative path under the invoking exec root and reports a
// difference as GateExecutedCopyChanged. An absolutely named script runs from
// any cwd as the pinned file itself and needs no second comparison.
// verify-pins also hashes that relative path under each work root the run's
// steps recorded, where those steps' gates ran, and reports a difference or
// an absence as a non-blocking row (GateStepCopyChanged, GateStepCopyAbsent):
// a step may legitimately edit a gate script, and refusing a changed copy at
// spawn time belongs to the gate runner.
//
// Trust entries are matched under the run's project identity
// (projectIdentity, projects.identity), in activation and in verify-pins
// alike, so the pin set and the report agree about which entries matched.
// The invoking process's identity was rejected because the answer would then
// change with the caller's cwd. Deriving the identity from ExecRoot through
// git was rejected because it re-derives what the project row already
// records. The gate runner still matches under its invoker's identity, so a
// gate run from another repository fails as unmatched; the report describes
// what the run trusts, not that failure.
//
// The ref is the absolute path. It lies outside every instance-config root, so
// it takes the form readFilePins documents for such a file and verifyFilePin
// already resolves. A store that cannot be read yields no pins and no error:
// the gate preflight reports an unreadable store, and refusing an activation
// over it would make a trust-store fault a run blocker.
func gateScriptPins(
	defs map[int]*workflow.Definition,
	loadStore func() (*trust.Store, error), projectIdentity, execRoot string,
) []store.Pin {
	names := declaredGateNames(defs)
	if len(names) == 0 {
		return nil
	}
	trusted, err := loadStore()
	if err != nil {
		return nil
	}
	// An identity that does not resolve matches only global entries, the
	// closed direction, so it is not an error here.
	identity, _ := gateTrustIdentity(projectIdentity)

	var pins []store.Pin
	seen := map[string]bool{}
	for _, name := range names {
		argv, _ := matchedGateArgv(trusted, identity, name)
		for _, file := range repoLocalGateFiles(argv, execRoot) {
			if seen[file] {
				continue
			}
			content, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			seen[file] = true
			pins = append(pins, store.Pin{
				Kind: store.PinKindFile, Ref: file, SHA256: workflow.SHA256(content),
			})
		}
	}
	return pins
}

// gateVerdicts lists the declared gates whose script the run does not pin:
// an entry whose argv names no repo-local file, or one naming a file the run
// holds no pin for (a run activated before gate scripts were pinned). A gate
// that cannot be checked at all is listed as GateUnchecked with the cause. A
// pinned gate whose copy under invokingRoot differs from its pin, or is
// absent, is listed too (see executedCopyVerdict), and so is each differing
// or absent copy under stepRoots (see stepCopyVerdicts). A step work root
// that no longer exists is listed once, under the first gate with a script to
// compare there (see goneRootVerdicts), not once per gate and script. An
// empty list means every declared gate was checked, found pinned, and found
// matching where it would execute.
func gateVerdicts(
	defs map[int]*workflow.Definition, pins []PinVerdict,
	loadStore func() (*trust.Store, error), projectIdentity, execRoot, invokingRoot string,
	stepRoots []stepWorkRoot,
) []GateVerdict {
	out := []GateVerdict{}
	names := declaredGateNames(defs)
	if len(names) == 0 {
		return out
	}
	trusted, err := loadStore()
	if err != nil {
		for _, name := range names {
			out = append(out, GateVerdict{
				Gate: name, Status: GateUnchecked,
				Reason: fmt.Sprintf("the trust store could not be read: %v", err),
			})
		}
		return out
	}
	identity, identityErr := gateTrustIdentity(projectIdentity)

	pinned := map[string]string{}
	for _, p := range pins {
		if p.Kind == store.PinKindFile {
			pinned[p.Ref] = p.Pinned
		}
	}
	goneRootsListed := false
	for _, name := range names {
		argv, unmatched := matchedGateArgv(trusted, identity, name)
		if argv == nil {
			reason := "unmatched: " + unmatched
			if identityErr != nil {
				// Only global entries can match without an identity, so a
				// repo-bound entry for this gate may exist and be unreachable.
				reason = fmt.Sprintf("the repo identity did not resolve (%v), "+
					"so no repo-bound trust entry can match", identityErr)
			}
			out = append(out, GateVerdict{Gate: name, Status: GateUnchecked, Reason: reason})
			continue
		}
		if execRoot == "" || !filepath.IsAbs(execRoot) {
			out = append(out, GateVerdict{
				Gate: name, Status: GateUnpinned,
				Reason: "the run records no exec root, so no script can be resolved",
			})
			continue
		}
		files := repoLocalGateFiles(argv, execRoot)
		if len(files) == 0 {
			out = append(out, GateVerdict{
				Gate: name, Status: GateUnpinned,
				Reason: "the trusted argv names no script under the run's exec root, " +
					"so no script content is pinned",
			})
			continue
		}
		unpinned := false
		for _, file := range files {
			if _, ok := pinned[file]; !ok {
				out = append(out, GateVerdict{
					Gate: name, Status: GateUnpinned,
					// %q: the path comes from the repo, and this text reaches a
					// terminal.
					Reason: fmt.Sprintf("%q is named by the trusted argv but this run holds no pin for it", file),
				})
				unpinned = true
				break
			}
		}
		if unpinned {
			continue
		}
		scripts := pinnedRelativeScripts(argv, execRoot, pinned)
		if len(scripts) == 0 {
			continue
		}
		if v, ok := executedCopyVerdict(name, scripts, execRoot, invokingRoot); ok {
			out = append(out, v)
		}
		if !goneRootsListed {
			out = append(out, goneRootVerdicts(name, stepRoots)...)
			goneRootsListed = true
		}
		out = append(out, stepCopyVerdicts(name, scripts, stepRoots)...)
	}
	return out
}

// pinnedRelativeScript is a relatively named gate script token and the pin
// that every other tree's copy of it is compared with.
type pinnedRelativeScript struct {
	tok, pinnedFile, want string
}

// pinnedRelativeScripts is the relatively named script tokens of argv whose
// file under execRoot, symlinks resolved, the run pins. Bash opens a relative
// script operand against its cwd, so a gate run from another tree executes
// that tree's copy of each. An absolutely named script runs as the pinned
// file itself from any cwd and is not listed.
func pinnedRelativeScripts(
	argv []string, execRoot string, pinned map[string]string,
) []pinnedRelativeScript {
	root, err := filepath.EvalSymlinks(execRoot)
	if err != nil {
		return nil
	}
	var out []pinnedRelativeScript
	for _, tok := range gateScriptTokens(argv) {
		if filepath.IsAbs(tok) {
			continue
		}
		pinnedFile, err := filepath.EvalSymlinks(filepath.Join(root, tok))
		if err != nil {
			continue
		}
		if want, ok := pinned[pinnedFile]; ok {
			out = append(out, pinnedRelativeScript{tok: tok, pinnedFile: pinnedFile, want: want})
		}
	}
	return out
}

// copyComparison is how one tree's copy of a pinned script compares with the
// pin.
type copyComparison int

const (
	copyMatches copyComparison = iota
	copyAbsent
	copyUncheckable
	copyDiffers
)

// compareCopy compares the file at path, the copy a gate run would open, with
// the pinned hash want. The detail is what a Reason reports: how the copy
// differs, or why it cannot be checked.
//
// The path is read as bash would open it, following symlinks and without the
// outside-root skip repoLocalGateFiles applies, so a replacement too far away
// to be pinned is still compared. A copy larger than maxGateScriptBytes
// differs without being read, because no pin covers a file that large. A
// non-regular file (a directory, a FIFO) cannot be checked and is never read,
// since a read could block.
func compareCopy(path, want string) (copyComparison, string) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return copyAbsent, ""
	case err != nil:
		return copyUncheckable, err.Error()
	case !info.Mode().IsRegular():
		return copyUncheckable, "not a regular file"
	case info.Size() > maxGateScriptBytes:
		return copyDiffers, fmt.Sprintf("holds %d bytes, more than any pinned script", info.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return copyUncheckable, err.Error()
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return copyUncheckable, err.Error()
	}
	if found := hex.EncodeToString(h.Sum(nil)); found != want {
		return copyDiffers, "holds " + found
	}
	return copyMatches, ""
}

// copyReason is the Reason for a copy that does not match its pin. where names
// the copy and the gate run that would execute it. The detail is quoted: it
// can carry a path from a tree an executor chose, and the text reaches a
// terminal.
func copyReason(cmp copyComparison, detail, where string, s pinnedRelativeScript) string {
	switch cmp {
	case copyAbsent:
		return fmt.Sprintf("%s does not exist; the run pins %q", where, s.pinnedFile)
	case copyUncheckable:
		return fmt.Sprintf("%s cannot be checked against the pinned %q: %q", where, s.pinnedFile, detail)
	default:
		return fmt.Sprintf("%s %s; the run pins %q at %s", where, detail, s.pinnedFile, s.want)
	}
}

// executedCopyVerdict compares the copy of each of a gate's relatively named,
// pinned scripts that a gate run from invokingRoot would execute with the
// pin. It reports the first copy that differs or is absent, and nothing when
// invokingRoot is the run's own checkout.
func executedCopyVerdict(
	gate string, scripts []pinnedRelativeScript, execRoot, invokingRoot string,
) (GateVerdict, bool) {
	if invokingRoot == "" || !filepath.IsAbs(invokingRoot) ||
		resolvedPath(invokingRoot) == resolvedPath(execRoot) {
		return GateVerdict{}, false
	}
	for _, s := range scripts {
		executed := filepath.Join(invokingRoot, s.tok)
		cmp, detail := compareCopy(executed, s.want)
		if cmp == copyMatches {
			continue
		}
		status := GateExecutedCopyChanged
		if cmp == copyAbsent {
			status = GateExecutedCopyAbsent
		}
		where := fmt.Sprintf("%q, the copy a gate run from %q would execute,", executed, invokingRoot)
		return GateVerdict{Gate: gate, Status: status, Reason: copyReason(cmp, detail, where, s)}, true
	}
	return GateVerdict{}, false
}

// stepWorkRoot is one distinct work root the run's steps recorded
// (steps.work_root), as first recorded, the steps that recorded it, and
// whether it no longer exists.
type stepWorkRoot struct {
	path  string
	steps []string
	gone  bool
}

// stepWorkRoots groups the run's recorded step work roots by symlink-resolved
// path, in step order. A root that is execRoot or invokingRoot, by resolved
// path or by recorded spelling, is left out: verifyFilePin and
// executedCopyVerdict already compare those trees, and a step row would
// repeat theirs. A root that no longer exists is kept under its recorded
// spelling and marked gone, so its absence is reported rather than skipped.
func stepWorkRoots(steps []*store.Step, execRoot, invokingRoot string) []stepWorkRoot {
	covered := map[string]bool{}
	for _, root := range []string{execRoot, invokingRoot} {
		if root != "" {
			covered[filepath.Clean(root)] = true
			covered[resolvedPath(root)] = true
		}
	}
	var out []stepWorkRoot
	index := map[string]int{}
	for _, s := range steps {
		if s.WorkRoot == "" {
			continue
		}
		key := resolvedPath(s.WorkRoot)
		if covered[key] || covered[filepath.Clean(s.WorkRoot)] {
			continue
		}
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			_, err := os.Stat(s.WorkRoot)
			out = append(out, stepWorkRoot{path: s.WorkRoot, gone: errors.Is(err, os.ErrNotExist)})
		}
		out[i].steps = append(out[i].steps, s.Ref())
	}
	return out
}

// resolvedPath is path with symlinks resolved, or cleaned when it does not
// resolve.
func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// goneRootVerdicts lists each step work root that no longer exists as one
// GateStepCopyAbsent row under gate, naming the root and every step that
// recorded it. No copy under a gone root can be compared for any gate, so
// gateVerdicts lists these once, under the first gate with a script to
// compare, rather than once per gate and script: work roots are swept at
// integration, and per-script rows for each would bury a differing copy.
func goneRootVerdicts(gate string, roots []stepWorkRoot) []GateVerdict {
	var out []GateVerdict
	for _, wr := range roots {
		if wr.gone {
			out = append(out, GateVerdict{Gate: gate, Status: GateStepCopyAbsent, Reason: fmt.Sprintf(
				"the work root %q recorded by %s no longer exists, so no gate script copy under it "+
					"can be checked", wr.path, strings.Join(wr.steps, ", "))})
		}
	}
	return out
}

// stepCopyVerdicts compares each existing step work root's copy of the
// gate's relatively named, pinned scripts with the pin, and lists one row per
// root and script whose copy differs, cannot be checked
// (GateStepCopyChanged), or is absent (GateStepCopyAbsent). Each row names
// every step that recorded the root. Unlike executedCopyVerdict it reports
// every difference, not the first. Gone roots are goneRootVerdicts' rows.
func stepCopyVerdicts(gate string, scripts []pinnedRelativeScript, roots []stepWorkRoot) []GateVerdict {
	var out []GateVerdict
	for _, wr := range roots {
		if wr.gone {
			continue
		}
		steps := strings.Join(wr.steps, ", ")
		for _, s := range scripts {
			executed := filepath.Join(wr.path, s.tok)
			cmp, detail := compareCopy(executed, s.want)
			if cmp == copyMatches {
				continue
			}
			status := GateStepCopyChanged
			if cmp == copyAbsent {
				status = GateStepCopyAbsent
			}
			where := fmt.Sprintf("%q, the copy a gate run in the work root of %s would execute,",
				executed, steps)
			out = append(out, GateVerdict{Gate: gate, Status: status, Reason: copyReason(cmp, detail, where, s)})
		}
	}
	return out
}

// verifyGateScripts fills the report's gate half, comparing executed copies
// under invokingRoot and under the run's recorded step work roots. It reads
// the run's step definitions, steps, exec root, and project identity and the
// trust store; it writes nothing.
func verifyGateScripts(
	conn *sql.DB, runID int, report *PinReport,
	loadStore func() (*trust.Store, error), invokingRoot string,
) error {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return err
	}
	st := store.StoreOn(conn)
	run, err := st.GetRun(runID)
	if err != nil {
		return err
	}
	project, err := st.GetProject(store.DefaultProjectIDOr(run.ProjectID))
	if err != nil {
		return err
	}
	steps, err := st.ListRunSteps(runID)
	if err != nil {
		return err
	}
	report.Gates = gateVerdicts(defs, report.Pins, loadStore,
		project.Identity, run.ExecRoot, invokingRoot,
		stepWorkRoots(steps, run.ExecRoot, invokingRoot))
	report.UnpinnedGates, report.ExecutedCopyChanged = 0, 0
	for _, g := range report.Gates {
		switch g.Status {
		case GateUnpinned:
			report.UnpinnedGates++
		case GateExecutedCopyChanged:
			report.ExecutedCopyChanged++
		}
	}
	return nil
}
