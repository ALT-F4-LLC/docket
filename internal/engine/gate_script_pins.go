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

	"github.com/ALT-F4-LLC/docket/internal/db"
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

// GateVerdict is one declared gate whose script this run does not pin, or
// whose script could not be checked.
type GateVerdict struct {
	Gate   string `json:"gate"`
	Status string `json:"status"`
	// Reason says why there is nothing pinned, so an operator can tell a
	// `make` entry (retarget it to name the script) from a pin that was never
	// recorded (the run activated before gate scripts were pinned).
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
// any cwd as the pinned file itself and needs no second comparison. Step
// worktrees are not compared: a step may legitimately edit a gate script,
// and comparing at spawn time belongs to the gate runner.
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
) []db.Pin {
	names := declaredGateNames(defs)
	if len(names) == 0 {
		return nil
	}
	store, err := loadStore()
	if err != nil {
		return nil
	}
	// An identity that does not resolve matches only global entries, the
	// closed direction, so it is not an error here.
	identity, _ := gateTrustIdentity(projectIdentity)

	var pins []db.Pin
	seen := map[string]bool{}
	for _, name := range names {
		argv, _ := matchedGateArgv(store, identity, name)
		for _, file := range repoLocalGateFiles(argv, execRoot) {
			if seen[file] {
				continue
			}
			content, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			seen[file] = true
			pins = append(pins, db.Pin{
				Kind: db.PinKindFile, Ref: file, SHA256: workflow.SHA256(content),
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
// absent, is listed too (see executedCopyVerdict). An empty list means every
// declared gate was checked, found pinned, and found matching where it would
// execute.
func gateVerdicts(
	defs map[int]*workflow.Definition, pins []PinVerdict,
	loadStore func() (*trust.Store, error), projectIdentity, execRoot, invokingRoot string,
) []GateVerdict {
	out := []GateVerdict{}
	names := declaredGateNames(defs)
	if len(names) == 0 {
		return out
	}
	store, err := loadStore()
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
		if p.Kind == db.PinKindFile {
			pinned[p.Ref] = p.Pinned
		}
	}
	for _, name := range names {
		argv, unmatched := matchedGateArgv(store, identity, name)
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
		if v, ok := executedCopyVerdict(name, argv, execRoot, invokingRoot, pinned); ok {
			out = append(out, v)
		}
	}
	return out
}

// executedCopyVerdict compares the copy of a gate's script that a gate run
// from invokingRoot would execute with the copy the run pinned under
// execRoot. It reports the first relatively named script whose copy differs
// or is absent, and nothing when invokingRoot is the run's own checkout.
//
// Only relative tokens are compared: bash opens a relative script operand
// against its cwd, so the executed copy is invokingRoot joined with the
// token. That path is read as bash would open it, without the outside-root
// and size skips repoLocalGateFiles applies, so a replacement too large or
// too far away to be pinned is still reported.
func executedCopyVerdict(
	gate string, argv []string, execRoot, invokingRoot string, pinned map[string]string,
) (GateVerdict, bool) {
	if invokingRoot == "" || !filepath.IsAbs(invokingRoot) {
		return GateVerdict{}, false
	}
	root, err := filepath.EvalSymlinks(execRoot)
	if err != nil {
		return GateVerdict{}, false
	}
	if invoking, err := filepath.EvalSymlinks(invokingRoot); err == nil && invoking == root {
		return GateVerdict{}, false
	}
	for _, tok := range gateScriptTokens(argv) {
		if filepath.IsAbs(tok) {
			continue
		}
		pinnedFile, err := filepath.EvalSymlinks(filepath.Join(root, tok))
		if err != nil {
			continue
		}
		want, ok := pinned[pinnedFile]
		if !ok {
			continue
		}
		executed := filepath.Join(invokingRoot, tok)
		found, err := fileSHA256(executed)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return GateVerdict{Gate: gate, Status: GateExecutedCopyAbsent, Reason: fmt.Sprintf(
				"%q, the copy a gate run from %q would execute, does not exist; the run pins %q",
				executed, invokingRoot, pinnedFile)}, true
		case err != nil:
			return GateVerdict{Gate: gate, Status: GateExecutedCopyChanged, Reason: fmt.Sprintf(
				"%q, the copy a gate run from %q would execute, cannot be checked against "+
					"the pinned %q: %v", executed, invokingRoot, pinnedFile, err)}, true
		case found != want:
			return GateVerdict{Gate: gate, Status: GateExecutedCopyChanged, Reason: fmt.Sprintf(
				"%q, the copy a gate run from %q would execute, holds %s; the run pins %q at %s",
				executed, invokingRoot, found, pinnedFile, want)}, true
		}
	}
	return GateVerdict{}, false
}

// fileSHA256 is the hex SHA-256 of a regular file's content, read as a
// stream so an oversized file is hashed without holding it in memory. A
// non-regular file (a directory, a FIFO) is an error, never a read that could
// block.
func fileSHA256(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyGateScripts fills the report's gate half, comparing executed copies
// under invokingRoot. It reads the run's step definitions, exec root, and
// project identity and the trust store; it writes nothing.
func verifyGateScripts(
	conn *sql.DB, runID int, report *PinReport,
	loadStore func() (*trust.Store, error), invokingRoot string,
) error {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return err
	}
	run, err := db.GetRun(conn, runID)
	if err != nil {
		return err
	}
	project, err := db.GetProject(conn, db.DefaultProjectIDOr(run.ProjectID))
	if err != nil {
		return err
	}
	report.Gates = gateVerdicts(defs, report.Pins, loadStore,
		project.Identity, run.ExecRoot, invokingRoot)
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
