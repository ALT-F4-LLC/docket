package engine

import (
	"os"
	"path"
	"path/filepath"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// PACKET-CLOSURE PINNING (DKT-581).
//
// Activation used to pin EVERY file the config scan walked. That made the pin
// set the whole corpus, so a corpus install mid-run drifted pins for files the
// run could never read — 7 of 18 terminal runs in the measured week were
// abandoned over exactly that, and `verify-pins` reported drift on contracts
// the bound workflows never render.
//
// The pin set is now the CLOSURE the bound workflows actually reach:
//
//   - every step's declared `packet` entries, with `{executor}` substituted
//     the way expansion substitutes it — per fanout hint for a fanout step,
//     the declared executor otherwise. EVERY declared step contributes,
//     including `loop = true` bodies (they instantiate at loop entry) and
//     steps a `when` will skip (expansion still requires their entries
//     pinned);
//   - the files those entries' `packet_includes` frontmatter declares,
//     walked transitively — a fragment naming a fragment stays pinned even
//     if the resolver later deepens past its current one include level;
//   - `policy.toml`, the instance's policy surface, which is read by the
//     harness rather than by a step and so appears in no `packet` list.
//
// Registered-object pins (workflows, schemas) were already narrow — one pin
// per BOUND workflow and per schema those workflows' steps declare — so this
// brings the file pins to the same standard: a run pins what it can read,
// and a corpus edit anywhere else is a non-event for `verify-pins`.
//
// `--pin PATH` files are deliberately NOT filtered: they are the operator's
// explicit additions, and the operator saying "pin this" is the closure for
// that file.

// policyPinRef is the config-relative ref of the instance's policy file. Core
// never reads its content — it is pinned because the harness resolves policy
// from it and a run must be able to say which policy it ran under.
const policyPinRef = "policy.toml"

// packetClosurePins filters the config scan's pins to the packet closure the
// bound workflows reach. Order is preserved from scan.pins, which is already
// sorted by ref, so the recorded set stays deterministic.
func packetClosurePins(
	scan *configScan, runIssues []*db.RunIssue, bindings map[int]*boundDefinition,
) []db.Pin {
	needed := packetClosureRefs(scan, runIssues, bindings)
	out := make([]db.Pin, 0, len(needed))
	for _, p := range scan.pins {
		if needed[path.Clean(filepath.ToSlash(p.Ref))] {
			out = append(out, p)
		}
	}
	return out
}

// packetClosureRefs computes the set of config-relative refs the bound
// workflows can reach, as described at the top of this file.
func packetClosureRefs(
	scan *configScan, runIssues []*db.RunIssue, bindings map[int]*boundDefinition,
) map[string]bool {
	needed := map[string]bool{policyPinRef: true}

	// The worklist carries every ref whose `packet_includes` still needs
	// walking. `needed` doubles as the seen-set, so an include cycle
	// terminates and a diamond is walked once.
	var queue []string
	add := func(ref string) {
		ref = path.Clean(filepath.ToSlash(ref))
		if ref == "" || ref == "." || needed[ref] {
			return
		}
		needed[ref] = true
		queue = append(queue, ref)
	}

	// One walk per bound DEFINITION, not per issue: substitution depends only
	// on the step's own executor/fanout declarations, never on the issue's
	// subject, so two issues bound to one workflow reach one closure.
	seenWorkflow := make(map[int]bool, len(bindings))
	for _, ri := range runIssues {
		bound := bindings[ri.IssueID]
		if bound == nil || bound.definition == nil {
			continue
		}
		if seenWorkflow[bound.workflow.ID] {
			continue
		}
		seenWorkflow[bound.workflow.ID] = true

		for _, step := range bound.definition.Steps {
			for _, entry := range step.Packet {
				if len(step.Fanout) > 0 {
					// PER HINT, exactly as expansion substitutes per sibling:
					// the reachable contracts of a fanout step are its hints'.
					for _, hint := range step.Fanout {
						add(workflow.SubstitutePacketEntry(entry, hint))
					}
					continue
				}
				add(workflow.SubstitutePacketEntry(entry, step.Executor))
			}
		}
	}

	for i := 0; i < len(queue); i++ {
		for _, include := range packetIncludesOf(scan.roots, queue[i]) {
			add(include)
		}
	}
	return needed
}

// packetIncludesOf reads one closure file's `packet_includes`, best-effort.
//
// BEST-EFFORT IS DELIBERATE. The strict ladder — pinned/unpinned, hash match,
// malformed frontmatter — belongs to resolution (readPinnedPacketFile,
// parsePacketFrontmatter), which refuses at claim/render time with the precise
// message. Refusing here would move that failure into activation for a file
// activation otherwise never opens; skipping here changes nothing the strict
// path would not catch, because a file whose includes could not be read here
// is a file whose render will refuse on the same defect. A ref no root holds
// simply contributes no includes — expansion's unpinned-entry refusal already
// owns that case.
func packetIncludesOf(roots []string, ref string) []string {
	for _, root := range roots {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref)))
		if err != nil {
			continue
		}
		// FIRST ROOT THAT HOLDS IT WINS, matching readPinnedPacketFile — and
		// the scan has already refused a ref two roots offer with different
		// bytes, so the choice cannot change the answer.
		includes, _, perr := parsePacketFrontmatter(ref, string(content))
		if perr != nil {
			return nil
		}
		return includes
	}
	return nil
}

// withoutAlreadyPinned drops closure pins whose ref an earlier activation
// already recorded — in the config-relative form, or as a legacy absolute
// walked path that maps onto the same ref.
//
// It exists for RA2: a re-activation inherits the original pin set, and the
// closure recomputed here must only ADD refs a newly-bound workflow needs
// (RA3), never re-list an inherited ref. Re-listing one would carry the
// current file size into the declared-packet index where the inherited pin
// deliberately resolves as present-with-unknown-size — and a re-activation
// must not reject a run that was legal when it started.
func withoutAlreadyPinned(closure []db.Pin, existing []db.Pin, roots []string) []db.Pin {
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		if p.Kind != db.PinKindFile {
			continue
		}
		ref := p.Ref
		if filepath.IsAbs(ref) {
			for _, root := range roots {
				if rel, err := filepath.Rel(root, ref); err == nil &&
					!isOutsideRoot(rel) {
					ref = rel
					break
				}
			}
		}
		have[path.Clean(filepath.ToSlash(ref))] = true
	}

	kept := make([]db.Pin, 0, len(closure))
	for _, p := range closure {
		if !have[path.Clean(filepath.ToSlash(p.Ref))] {
			kept = append(kept, p)
		}
	}
	return kept
}

// isOutsideRoot reports whether a Rel result escaped the root it was taken
// against.
func isOutsideRoot(rel string) bool {
	return rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}
