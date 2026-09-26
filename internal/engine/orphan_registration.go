package engine

import (
	"fmt"
	"os"
	"sync"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ORPHANED REGISTRATIONS (DKT-609): a registered workflow NAME that no file in
// any instance-config root declares any more.
//
// The state that produced this: a corpus commit renamed
// `security-load-bearing` to `security-change`, and the four registered
// versions of the old name stayed live in the store — registration is a row,
// not a file, and nothing removes a row (TestRegistrationSurvivesTOMLDeletion
// pins that on purpose). The next issue carrying the old label then failed
// activation with "matches 2 workflows: security-change@17,
// security-load-bearing@12", a refusal that named both candidates and said
// nothing about the fact that ONE OF THEM NO LONGER EXISTS ON DISK. The
// remedy — `docket workflow deprecate` — was obvious only after a git
// archaeology session established which of the two names the corpus had
// dropped.
//
// This file computes that missing half: for a registered name, whether a fresh
// scan of the configured roots finds a file declaring it.
//
// IT IS A SIBLING OF source_drift.go, NOT A DUPLICATE. CheckWorkflowSource
// re-reads ONE row's recorded `source_path` and compares bytes — the drift
// case, where the same file changed. A rename leaves no file to re-read: the
// old path is gone and the new file declares a different name, so the question
// has to be asked of the ROOTS rather than of a path. The same rules apply as
// they do there: the verdict is NEVER an error, it is computed on demand, and
// NOTHING IS EVER REPAIRED — deprecating a stranded registration is an
// operator's decision, taken with a verb, never a side effect of reporting.
//
// THE DISCOVERY IS THE SCAN'S, NOT A SECOND ONE. The names come from
// scanConfigDirs (the walk activation already runs) and from
// `[pipeline].name` (the identity registerConfigWorkflowTx already registers
// under), so this cannot disagree with registration about what a root contains
// or what name a file declares.

// WorkflowOriginIndex answers, for a registered workflow name, whether a file
// in the instance-config roots still declares it.
//
// It is built LAZILY. Activation constructs one per activation and consults it
// only on a binding refusal, so the ordinary path pays nothing: reading and
// parsing every workflow file in the corpus a second time, on every
// activation, to answer a question nobody asked would be a real cost for a
// diagnostic.
type WorkflowOriginIndex struct {
	// scan is the discovery this index reads. A nil scan means NO ROOT EXISTS
	// (scanConfigDirs' F17 dormancy), which is `unchecked` and never
	// `orphaned`.
	scan *configScan

	once sync.Once
	// declaredBy maps a declared `[pipeline].name` to the first file declaring
	// it, in the scan's own precedence order.
	declaredBy map[string]string
	// err is the failure that stopped the build. Every verdict then reports
	// `unchecked` carrying it: a half-read root can only produce false
	// orphans, and a false orphan is exactly the claim that costs an operator
	// an investigation.
	err error
}

// ScanWorkflowOrigins scans the instance-config roots for the read verbs
// (`workflow list --orphans`).
//
// The error is the SCAN's own refusal — a root that is a regular file, a
// dangling symlink — surfaced rather than swallowed, because those are the
// states in which every registration would otherwise be reported orphaned on
// the strength of having looked nowhere.
func ScanWorkflowOrigins() (*WorkflowOriginIndex, error) {
	scan, err := scanConfigDirs(resolvePaths().InstanceConfigDirs())
	if err != nil {
		return nil, err
	}
	return newWorkflowOriginIndex(scan), nil
}

// newWorkflowOriginIndex wraps a scan an activation already performed.
func newWorkflowOriginIndex(scan *configScan) *WorkflowOriginIndex {
	return &WorkflowOriginIndex{scan: scan}
}

// Scanned reports whether any instance-config root existed to look in. It asks
// nothing of the files themselves — a build failure is Err's to report — so
// the two refusals a caller owes an operator stay separate facts.
//
// A false answer means every verdict this index gives is `unchecked`, and a
// caller whose whole purpose is orphan detection should say so rather than
// render an empty result that reads as "nothing is orphaned".
func (i *WorkflowOriginIndex) Scanned() bool {
	return i != nil && i.scan != nil && len(i.scan.roots) > 0
}

// Roots are the canonicalized roots that were scanned, in precedence order.
func (i *WorkflowOriginIndex) Roots() []string {
	if i == nil || i.scan == nil {
		return nil
	}
	return i.scan.roots
}

// Err is the failure that stopped the build, if any.
func (i *WorkflowOriginIndex) Err() error {
	if i == nil {
		return nil
	}
	i.build()
	return i.err
}

// Status is the verdict for one registered workflow NAME.
//
// A nil index is `unchecked` rather than a panic: the annotation call sites sit
// on refusal paths, where the only thing worse than no annotation is a crash
// while reporting someone else's error.
func (i *WorkflowOriginIndex) Status(name string) *model.WorkflowOriginStatus {
	if i == nil || i.scan == nil || len(i.scan.roots) == 0 {
		return &model.WorkflowOriginStatus{
			State: model.WorkflowOriginUnchecked,
			Reason: "no instance-config root exists to scan, so nothing here " +
				"can say whether a registered name still has a definition",
		}
	}

	i.build()
	if i.err != nil {
		return &model.WorkflowOriginStatus{
			State:  model.WorkflowOriginUnchecked,
			Roots:  i.scan.roots,
			Reason: i.err.Error(),
		}
	}

	if path, ok := i.declaredBy[name]; ok {
		return &model.WorkflowOriginStatus{
			State: model.WorkflowOriginPresent,
			Roots: i.scan.roots,
			Path:  path,
		}
	}
	return &model.WorkflowOriginStatus{
		State: model.WorkflowOriginOrphaned,
		Roots: i.scan.roots,
	}
}

// Orphaned is Status reduced to the one bit the refusal annotations need.
func (i *WorkflowOriginIndex) Orphaned(name string) bool {
	return i.Status(name).Orphaned()
}

// build reads and parses every workflow file the scan found, ONCE.
//
// A file that DOES NOT PARSE declares no name — registryIdentityKey's rule,
// for its reason: the refusal for an unparseable definition belongs to
// registerConfigWorkflowTx, which words it precisely, and a second opinion
// here would be the same failure reported twice and worse. The consequence is
// worth stating: while a workflow file in a root is unparseable, the name it
// WOULD declare reads as orphaned — but that same file already refuses the
// next activation outright while adoption is on, so the repo is in a state an
// operator is being told about from a louder place.
//
// A file that cannot be READ is different, and fails the build: it is an
// environment fault rather than an authoring one, and every verdict falls back
// to `unchecked` rather than manufacturing orphans out of a permission error.
func (i *WorkflowOriginIndex) build() {
	i.once.Do(func() {
		i.declaredBy = make(map[string]string)
		if i.scan == nil {
			return
		}
		for _, path := range i.scan.paths {
			if isSchemaConfigPath(path) {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				i.err = fmt.Errorf(
					"reading the instance-config workflow %s: %w", path, err)
				i.declaredBy = nil
				return
			}
			def, perr := workflow.Parse(src)
			if perr != nil {
				continue
			}
			// FIRST DECLARATION WINS, matching the roots' precedence order.
			// Two roots declaring one name with different bytes is already
			// refused at registration (crossRootConflictErr); here the
			// question is only "does a file declare it", so the first is as
			// good an answer as the last.
			if _, dup := i.declaredBy[def.Pipeline.Name]; !dup {
				i.declaredBy[def.Pipeline.Name] = path
			}
		}
	})
}

// DescribeWorkflowOrigin renders one verdict as a human-readable clause, so
// every reader of this fact says the same thing about the same state — the
// shape DescribeWorkflowSource established.
func DescribeWorkflowOrigin(s *model.WorkflowOriginStatus) string {
	if s == nil {
		return ""
	}
	switch s.State {
	case model.WorkflowOriginPresent:
		return fmt.Sprintf(
			"present — %s in the instance config still declares this name", s.Path)
	case model.WorkflowOriginOrphaned:
		return "ORPHANED — no file in any instance-config root declares this " +
			"name any more; the registration outlives its definition (a rename " +
			"or a deletion), and it still binds until it is deprecated"
	default:
		return fmt.Sprintf("unchecked — %s", s.Reason)
	}
}

// orphanAnnotation is the clause refList appends to a candidate whose name has
// no definition left on disk.
//
// It is appended ONLY to orphans, never to the healthy side. The refusal it
// decorates already lists both candidates; annotating the one that is
// anomalous is what makes the pair distinguishable at a glance, whereas
// annotating both would double the length of every ordinary two-name refusal
// to say "normal" twice.
const orphanAnnotation = " (no source on disk — orphaned registration, deprecation candidate)"

// orphanRefusalHint is the sentence appended to a binding refusal in which at
// least one candidate is orphaned: what the state IS, and the one verb that
// clears it.
//
// IT PRESCRIBES, IT DOES NOT ACT. Deprecating a name is a decision about which
// definition the corpus meant to keep, and the engine holds no opinion on
// that — it only knows that one of the two names it is refusing over has
// nothing behind it.
const orphanRefusalHint = "\n\nAn ORPHANED registration is a name no file in any " +
	"instance-config root declares any more — usually the residue of a rename, " +
	"since registering the new name never retires the old one. Retire it with " +
	"`docket workflow deprecate <name>@<version>` (every registered version of " +
	"that name — see `docket workflow list --orphans`), or restore the file " +
	"that declared it."
