package engine

import (
	"database/sql"
	"fmt"
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

// GateVerdict is one declared gate whose script this run does not pin.
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

// matchedGateArgv is the argv of the trust entry a declared gate resolves to
// in this repo, or nil when the store cannot be read or no entry matches. An
// unmatched gate is `gate-unmatched`'s subject, not this file's.
func matchedGateArgv(
	store *trust.Store, identity, gate string,
) []string {
	m := store.Lookup(identity, gate, nil)
	if !m.Matched || m.Entry == nil {
		return nil
	}
	return m.Entry.Argv
}

// gateScriptPins is the file pins a run records for the scripts its declared
// gates execute: for each declared gate with a matching trust entry, every
// repo-local file the entry's argv names, hashed now.
//
// The ref is the absolute path. It lies outside every instance-config root, so
// it takes the form readFilePins documents for such a file and verifyFilePin
// already resolves. A store that cannot be read yields no pins and no error:
// the gate preflight reports an unreadable store, and refusing an activation
// over it would make a trust-store fault a run blocker.
func gateScriptPins(
	defs map[int]*workflow.Definition,
	loadStore func() (*trust.Store, error), identityPath, execRoot string,
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
	identity, _ := trust.RepoIdentity(identityPath)

	var pins []db.Pin
	seen := map[string]bool{}
	for _, name := range names {
		for _, file := range repoLocalGateFiles(matchedGateArgv(store, identity, name), execRoot) {
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

// unpinnedGates lists the declared gates whose script the run does not pin:
// an entry whose argv names no repo-local file, or one naming a file the run
// holds no pin for (a run activated before gate scripts were pinned).
func unpinnedGates(
	defs map[int]*workflow.Definition, pins []PinVerdict,
	loadStore func() (*trust.Store, error), identityPath, execRoot string,
) []GateVerdict {
	out := []GateVerdict{}
	names := declaredGateNames(defs)
	if len(names) == 0 {
		return out
	}
	store, err := loadStore()
	if err != nil {
		return out
	}
	identity, _ := trust.RepoIdentity(identityPath)

	pinned := map[string]bool{}
	for _, p := range pins {
		if p.Kind == db.PinKindFile {
			pinned[p.Ref] = true
		}
	}
	for _, name := range names {
		argv := matchedGateArgv(store, identity, name)
		if argv == nil {
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
		for _, file := range files {
			if !pinned[file] {
				out = append(out, GateVerdict{
					Gate: name, Status: GateUnpinned,
					// %q: the path comes from the repo, and this text reaches a
					// terminal.
					Reason: fmt.Sprintf("%q is named by the trusted argv but this run holds no pin for it", file),
				})
				break
			}
		}
	}
	return out
}

// verifyGateScripts fills the report's gate half. It reads the run's step
// definitions and exec root and the trust store; it writes nothing.
func verifyGateScripts(
	conn *sql.DB, runID int, report *PinReport,
	loadStore func() (*trust.Store, error), identityPath string,
) error {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return err
	}
	run, err := db.GetRun(conn, runID)
	if err != nil {
		return err
	}
	report.Gates = unpinnedGates(defs, report.Pins, loadStore, identityPath, run.ExecRoot)
	report.UnpinnedGates = len(report.Gates)
	return nil
}
