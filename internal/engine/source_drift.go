package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// SOURCE DRIFT (DKT-590): a registered workflow's `source_path` and
// `source_sha256` are two columns nothing ever compared against each other.
//
// The measured state that produced this: `workflow show investigation --json`
// reported version 4 at sha 4cb066e3, while the file at the recorded
// `source_path` was version 8 at sha 6ed74d17 — and every other registered
// workflow on the machine was the same. RUN-40 then bound investigation@4 and
// ran it while the operator read v8 on disk. Nothing anywhere said so, because
// the path was documented as "provenance only … never re-read".
//
// Provenance nobody checks is provenance nobody can trust. This file reads it
// once, at the two moments it matters — describing a registered workflow, and
// binding one to a run — and reports the verdict. IT NEVER REPAIRS ANYTHING:
// registration and de-registration of installed files belong to the install
// path, so a drifted workflow is surfaced, never re-registered, re-hashed, or
// dropped on the operator's behalf.

// CheckWorkflowSource reads the file at a registered workflow's recorded
// `source_path` and reports whether its bytes still hash to `source_sha256`.
//
// It hashes with workflow.SHA256 — the SAME function registration hashed with —
// so a mismatch is a real difference in bytes and never an artifact of two
// hashers disagreeing.
//
// The verdict is never an error: a missing file is a fact about the repository
// to report, not a failure of the reporting. Callers decide what each state is
// worth (see checkBoundSources for activation's disposition).
func CheckWorkflowSource(sourcePath, registeredSHA string) *model.WorkflowSourceStatus {
	status := &model.WorkflowSourceStatus{
		Path:             sourcePath,
		RegisteredSHA256: registeredSHA,
	}

	if strings.TrimSpace(sourcePath) == "" {
		status.State = model.WorkflowSourceUnchecked
		status.Reason = "no source path was recorded at registration " +
			"(a definition registered from stdin has no file to compare against)"
		return status
	}

	path, err := expandHomePath(sourcePath)
	if err != nil {
		status.State = model.WorkflowSourceUnchecked
		status.Reason = err.Error()
		return status
	}

	// A RELATIVE recorded path is not resolvable from here, and pretending
	// otherwise is worse than declining. `docket workflow register wf.toml`
	// stores exactly what was typed; resolving that against the cwd of some
	// later invocation would compare the registered bytes against a DIFFERENT
	// file that merely shares a name, and report drift that does not exist.
	// The auto-registered corpus — the population this check exists for —
	// records absolute paths (config.InstanceConfigDirs joins an absolute
	// root), so nothing this check is aimed at lands here.
	if !filepath.IsAbs(path) {
		status.State = model.WorkflowSourceUnchecked
		status.Reason = fmt.Sprintf(
			"the recorded source path %q is relative, so it names no particular "+
				"file from here; only an absolute path can be compared", sourcePath)
		return status
	}

	src, err := os.ReadFile(path)
	if err != nil {
		status.State = model.WorkflowSourceUnreadable
		if errors.Is(err, fs.ErrNotExist) {
			status.Reason = "the file no longer exists at that path"
		} else {
			status.Reason = err.Error()
		}
		return status
	}

	status.CurrentSHA256 = workflow.SHA256(src)
	if status.CurrentSHA256 == registeredSHA {
		status.State = model.WorkflowSourceMatches
		return status
	}
	status.State = model.WorkflowSourceDrifted
	return status
}

// expandHomePath expands a leading `~` to the user's home directory.
//
// The repository records absolute paths everywhere it records one itself, so
// this only reaches a path an operator typed with the tilde QUOTED (`docket
// workflow register '~/wf.toml'`, which the shell hands over unexpanded). It is
// here rather than left to the caller so both call sites resolve a stored path
// identically.
func expandHomePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"the recorded source path %q starts with ~ and the home directory "+
				"cannot be resolved: %v", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// DescribeWorkflowSource renders one verdict as a human-readable clause, so
// `workflow show` and `run activate` say the same thing about the same state.
func DescribeWorkflowSource(s *model.WorkflowSourceStatus) string {
	if s == nil {
		return ""
	}
	switch s.State {
	case model.WorkflowSourceMatches:
		return "matches — the file at this path still holds the registered bytes"
	case model.WorkflowSourceDrifted:
		return fmt.Sprintf(
			"DRIFTED — the file at this path now hashes to sha256:%s, not the "+
				"registered sha256:%s; what is registered is NOT what is on disk",
			s.CurrentSHA256, s.RegisteredSHA256)
	case model.WorkflowSourceUnreadable:
		return fmt.Sprintf("UNREADABLE — %s; the registered bytes are intact, "+
			"but their provenance no longer resolves", s.Reason)
	default:
		return fmt.Sprintf("unchecked — %s", s.Reason)
	}
}

// SourceWarning is one BOUND workflow whose registered source file no longer
// answers for the registered bytes, on the terms activation warns rather than
// refuses about (see checkBoundSources).
//
// It travels on the result for ScopeWarnings' reason: the engine holds no
// output dependency, and the verb picks the channel — stderr in human mode, an
// array in JSON.
type SourceWarning struct {
	Workflow string `json:"workflow"`
	// State is the model.WorkflowSourceState value, so a consumer branches on
	// the verdict rather than on prose.
	State            string `json:"state"`
	SourcePath       string `json:"source_path,omitempty"`
	RegisteredSHA256 string `json:"registered_sha256"`
	CurrentSHA256    string `json:"current_sha256,omitempty"`
	Reason           string `json:"reason"`
}

// checkBoundSources is activation's half of DKT-590: every workflow this run's
// issues bind to, checked against the bytes at its own recorded `source_path`.
//
// THE DISPOSITION TURNS ON WHETHER THE BINDING IS NEW, not on whether the
// activation is:
//
//   - A binding made HERE with a drifted source REFUSES the whole activation
//     (CONFLICT). It is the same fact F9's collision refusal already refuses
//     on — registered bytes and file bytes disagreeing at one name@version —
//     reached from the other side, and the same argument decides it: a run
//     that binds and pins the registered bytes while an operator reads
//     something else at that path cannot be reviewed by the person approving
//     it. Every comparable engine-integrity condition here refuses (a terminal
//     run, an open dispatch, a ref offered by two roots with different bytes,
//     a `--pin` path that will not read); the conditions that merely WARN are
//     planning omissions and environment facts — an unscoped holder, a missing
//     trust entry, a context bundle over the warn cap — where proceeding is
//     legitimate and often intended. Drift is not one of those.
//
//   - A binding INHERITED from an earlier activation only WARNS. RA2 and F15
//     are explicit that a config file edited mid-run must not reach a run
//     already under way — the pin set is inherited and nothing is re-scanned —
//     and refusing here would break exactly that guarantee in its most ordinary
//     form: bumping a workflow to a new version leaves the OLD version's
//     recorded path holding the new version's bytes, which is the retro loop
//     working as designed, and it would wedge every in-flight run bound to the
//     old version with no remedy but restoring a file.
//
// An UNREADABLE source always warns, however the binding was made. The
// registered bytes are intact and still reproduce — only their provenance is
// gone — and refusing would wedge every activation in a store holding a
// workflow whose file was moved or deleted, a state only the install path can
// resolve.
//
// AND `registration.auto = false` DOWNGRADES DRIFT TO A WARNING TOO. That
// toggle's documented meaning is "I don't want silent version upgrades: bind
// what is REGISTERED, not what the corpus now says" — so a registry lagging the
// files is the state the operator asked for, not a divergence nobody decided,
// and refusing over it would turn the off switch into a wedge the moment the
// corpus moved. With adoption enabled (the default) the same drift means
// something genuinely went unnoticed: the scan would have adopted a bumped
// version, or refused under F9 on changed bytes at an unchanged version, so a
// bound workflow whose file disagrees is a divergence no one has ruled on.
func checkBoundSources(
	runIssues []*db.RunIssue, bindings map[int]*boundDefinition, inherited map[int]bool,
	autoRegister bool,
) ([]SourceWarning, error) {
	// One check per distinct bound workflow, in first-bound order, so the
	// report is deterministic and a workflow bound by six issues is named once.
	// Freshness is folded across ALL of a workflow's bindings first: a workflow
	// bound freshly by one issue and inherited by another is a NEW binding, and
	// the strict disposition applies.
	type bound struct {
		wf    *model.Workflow
		fresh bool
	}
	order := make([]int, 0, len(bindings))
	seen := make(map[int]*bound, len(bindings))
	for _, ri := range runIssues {
		def := bindings[ri.IssueID]
		if def == nil {
			continue
		}
		id := def.workflow.ID
		if b, ok := seen[id]; ok {
			b.fresh = b.fresh || !inherited[ri.IssueID]
			continue
		}
		seen[id] = &bound{wf: def.workflow, fresh: !inherited[ri.IssueID]}
		order = append(order, id)
	}

	var warnings []SourceWarning
	for _, id := range order {
		b := seen[id]
		status := CheckWorkflowSource(b.wf.SourcePath, b.wf.SourceSHA256)
		switch status.State {
		case model.WorkflowSourceMatches, model.WorkflowSourceUnchecked:
			continue
		case model.WorkflowSourceDrifted:
			if b.fresh && autoRegister {
				return nil, driftedSourceErr(b.wf.Ref(), status)
			}
		}
		warnings = append(warnings, SourceWarning{
			Workflow:         b.wf.Ref(),
			State:            string(status.State),
			SourcePath:       status.Path,
			RegisteredSHA256: status.RegisteredSHA256,
			CurrentSHA256:    status.CurrentSHA256,
			Reason:           DescribeWorkflowSource(status),
		})
	}
	return warnings, nil
}

// driftedSourceErr is the refusal, written to be acted on without a second
// command: what disagrees, both hashes, and the two ways out — NEITHER of which
// this engine takes on the operator's behalf, because registering installed
// files is the install path's job.
func driftedSourceErr(ref string, s *model.WorkflowSourceStatus) error {
	return conflictErr(
		"workflow %s has DRIFTED from its registered source: the file at %s no "+
			"longer holds the bytes registered under that name@version\n\n"+
			"  registered  sha256:%s\n"+
			"  on disk     sha256:%s\n\n"+
			"This activation would bind and pin the REGISTERED bytes, so the run "+
			"would execute a definition that is not the one at that path — the "+
			"divergence would be invisible for the life of the run. A registered "+
			"name@version is frozen, so nothing here re-registers or re-hashes it: "+
			"either restore %s to the registered bytes, or register its current "+
			"bytes at a new [pipeline].version through the install path, then "+
			"activate again",
		ref, s.Path, s.RegisteredSHA256, s.CurrentSHA256, s.Path)
}
