package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// PENDING PACKET CLOSURE (DKT-582).
//
// DKT-581 narrowed the pin set at ACTIVATION to the closure the bound workflows
// reach. This is the same walk asked of a run MID-FLIGHT, and about a strictly
// smaller set: not "what could this run ever read" but "what can this run still
// read from here" — the packet closure of its NON-TERMINAL steps.
//
// It exists for repin's drop disposition. A ref that no longer resolves at all
// has no bytes to adopt, so the only safe question is whether anything left to
// run would ever open it; if nothing would, retiring the pin costs the run
// nothing, and refusing costs it every remaining step. RUN-42 and RUN-36 both
// died on refs deleted from the corpus that no pending step named.
//
// THE WALK IS THE SAME ONE, SOURCED DIFFERENTLY. Activation walks declared
// steps because no step rows exist yet; here the step ROWS exist and carry the
// executor hint expansion already substituted, so a fanout sibling contributes
// its own hint's contract rather than all of them — exactly what stepPacketFiles
// re-derives at render time, and for the same reason (one fact, one source).
//
// TWO THINGS ARE DELIBERATELY OVER-COUNTED, because a false "still referenced"
// only refuses a drop while a false "unreferenced" would wedge a later render:
//
//   - an issue whose phase has NOT been expanded contributes its bound
//     definition's WHOLE declared closure, per DKT-581's substitution rules —
//     its step rows do not exist yet, so there is nothing narrower to ask;
//   - `policy.toml` is always referenced. The harness resolves policy from it
//     rather than any step's packet, so it appears in no `packet` list and no
//     step-sourced walk could ever find it.
type pendingClosure map[string]*closureReach

// closureReach is what reaches one ref: the pending steps (and unexpanded
// phases) whose packets can still open it, and the closure FILES whose
// `packet_includes` name it.
//
// The two are kept apart because they answer different questions. Repin's drop
// disposition asks "would anything left to run open this", which only the steps
// answer. DKT-821's closure check asks "who wrote the reference", and a fragment
// reached three includes deep is named by no step's packet — the file that
// includes it is the thing an operator edits or re-pins.
type closureReach struct {
	steps    []string
	includes []string
}

// referencedBy returns the pending steps (and unexpanded phases) that can still
// reach a ref, in a stable order, or nil when nothing can.
func (c pendingClosure) referencedBy(ref string) []string {
	return sortedCopy(c[normalizePinRef(ref)].stepsOf())
}

// includedBy returns the closure files whose `packet_includes` name a ref, in a
// stable order, or nil when a step's own `packet` entry is what reaches it.
func (c pendingClosure) includedBy(ref string) []string {
	return sortedCopy(c[normalizePinRef(ref)].includesOf())
}

func (r *closureReach) stepsOf() []string {
	if r == nil {
		return nil
	}
	return r.steps
}

func (r *closureReach) includesOf() []string {
	if r == nil {
		return nil
	}
	return r.includes
}

// sortedCopy is the stable-order form every reacher list is handed out in —
// nil for empty, so a caller can gate on length alone.
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// normalizePinRef is the one spelling refs are compared in — the config-relative
// slash form packet entries, pin rows, and an operator's `--drop` argument all
// have to agree on.
func normalizePinRef(ref string) string {
	return path.Clean(filepath.ToSlash(ref))
}

// pendingPacketClosure computes the refs a run's non-terminal work can still
// read, each mapped to what reaches it.
func pendingPacketClosure(conn *sql.DB, runID int, roots []string) (pendingClosure, error) {
	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return nil, err
	}
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	runIssues, err := db.ListRunIssues(conn, runID)
	if err != nil {
		return nil, err
	}

	closure := pendingClosure{}
	// The worklist carries every ref whose `packet_includes` still needs
	// walking; membership in `closure` doubles as the seen-set, so an include
	// cycle terminates and a diamond is walked once.
	var queue []string
	add := func(ref, by string) *closureReach {
		ref = normalizePinRef(ref)
		if ref == "" || ref == "." {
			return nil
		}
		reach, seen := closure[ref]
		if !seen {
			reach = &closureReach{}
			closure[ref] = reach
			queue = append(queue, ref)
		}
		reach.steps = appendOnce(reach.steps, by)
		return reach
	}

	add(policyPinRef, "the harness (policy)")

	for _, s := range steps {
		if db.StepTerminal(s.Status) {
			continue
		}
		spec := stepSpec(defs, s, holdTally{})
		if spec == nil {
			continue
		}
		for _, entry := range spec.Packet {
			add(workflow.SubstitutePacketEntry(entry, s.Executor), s.Instance)
		}
	}

	for _, ri := range runIssues {
		if ri.Expanded() || ri.WorkflowID == nil {
			continue
		}
		def := defs[*ri.WorkflowID]
		if def == nil {
			continue
		}
		by := fmt.Sprintf("issue %d's unexpanded phase", ri.IssueID)
		for _, step := range def.Steps {
			for _, entry := range step.Packet {
				if len(step.Fanout) > 0 {
					for _, hint := range step.Fanout {
						add(workflow.SubstitutePacketEntry(entry, hint), by)
					}
					continue
				}
				add(workflow.SubstitutePacketEntry(entry, step.Executor), by)
			}
		}
	}

	for i := 0; i < len(queue); i++ {
		ref := queue[i]
		// Snapshot the reachers before walking: `add` appends to the map, and
		// a self-include would otherwise range a slice it is extending.
		reachers := append([]string(nil), closure[ref].steps...)
		for _, include := range packetIncludesOf(roots, ref) {
			for _, by := range reachers {
				// The INCLUDE EDGE is recorded beside the step that inherits
				// it: the step says whether the ref still matters, the file
				// says who asked for it, and DKT-821 needs both in one
				// sentence.
				if reach := add(include, by); reach != nil {
					reach.includes = appendOnce(reach.includes, ref)
				}
			}
		}
	}
	return closure, nil
}

// appendOnce keeps a reacher list a set without paying for a map per ref —
// these lists are single-digit in every real closure.
func appendOnce(list []string, add string) []string {
	if add == "" {
		return list
	}
	for _, have := range list {
		if have == add {
			return list
		}
	}
	return append(list, add)
}

// UNPINNED CLOSURE REFS — the one walk two verbs ask (DKT-805, DKT-821).
//
// Repin asks it to decide what to CREATE when it adopts new bytes; verify-pins
// asks it to decide what to REPORT when it is handed a run whose pins all
// match. Both questions are "what does this closure reach that this pin set
// does not hold", and a second implementation of it would be a second answer:
// RUN-59 is exactly the run where the repin verb and the read verb disagreed,
// one pinning nothing and the other calling the result healthy.
type unpinnedRef struct {
	ref        string
	requiredBy []string
	includedBy []string
	// path is the first config root that holds the file, empty when none does.
	path string
	// sha256 is the file's current disk bytes, empty when it does not resolve.
	sha256 string
	// readErr is set when the file IS there and could not be read — a
	// permission to fix, never an absence.
	readErr error
}

// unpinnedClosureRefs returns every ref the pending closure reaches that the
// run's pin set does not cover, in ref order, each resolved against the roots.
//
// `policy.toml` is excluded, for the reason repin's additions exclude it: the
// HARNESS reads it, no step's packet ever renders it, so its absence from a pin
// set cannot make a step unrenderable — and treating it as a hole would have
// every run that chose not to pin policy report a hole it does not have.
func unpinnedClosureRefs(
	pins []PinVerdict, closure pendingClosure, roots []string,
) []unpinnedRef {
	// The run's held file refs, in the one spelling refs are compared in. A
	// legacy pin that recorded the full walked path maps onto its
	// config-relative ref, the same way withoutAlreadyPinned maps it for RA3 —
	// treating an inherited ref as unheld is the mistake both walks avoid.
	have := make(map[string]bool, len(pins))
	for _, v := range pins {
		if v.Kind != db.PinKindFile {
			continue
		}
		ref := v.Ref
		if filepath.IsAbs(ref) {
			for _, root := range roots {
				if rel, err := filepath.Rel(root, ref); err == nil && !isOutsideRoot(rel) {
					ref = rel
					break
				}
			}
		}
		have[normalizePinRef(ref)] = true
	}

	// Sorted, so the pins repin adds and the rows verify-pins prints land in a
	// deterministic order — the same golden-stability discipline the pin report
	// follows.
	refs := make([]string, 0, len(closure))
	for ref := range closure {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	var out []unpinnedRef
	for _, ref := range refs {
		if have[ref] || ref == policyPinRef {
			continue
		}
		u := unpinnedRef{
			ref:        ref,
			requiredBy: closure.referencedBy(ref),
			includedBy: closure.includedBy(ref),
		}
		for _, root := range roots {
			full := filepath.Join(root, filepath.FromSlash(ref))
			content, err := os.ReadFile(full)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// FIRST ROOT THAT HOLDS IT WINS, matching readPinnedPacketFile —
			// falling through to a later root here would describe a stale copy
			// of the file the closure actually resolves to.
			u.path = full
			if err != nil {
				u.readErr = err
			} else {
				u.sha256 = workflow.SHA256(content)
			}
			break
		}
		out = append(out, u)
	}
	return out
}
