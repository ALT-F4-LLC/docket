package engine

import (
	"os/exec"
	"strconv"
	"strings"
)

// IntegratedRange is what IntegrationRangeFn establishes about a commit that
// is said to carry a writer's work (DKT-4269).
type IntegratedRange struct {
	// Known is false when git could not answer; the caller refuses rather than
	// record an integration nobody verified.
	Known bool
	// Carried reports that the tip holds a patch-equivalent of every commit of
	// the writer's chain, or holds the chain itself.
	Carried bool
	// From is the commit the integrated range starts after: the first parent of
	// the earliest integrated commit. The re-pinned diff runs From..tip, so a
	// multi-commit writer's diff covers every integrated commit, not the last.
	From string
	// Missing names the writer commits (short ids) the tip carries no
	// equivalent of, when Carried is false.
	Missing []string
}

// integrationWalkLimit bounds how far down the tip's first-parent line the
// equivalence walk looks for a writer commit's twin.
const integrationWalkLimit = 1000

// gitIntegratedRange is IntegrationRangeFn's real implementation: does `tip`
// carry the writer chain base..head, and where does the integrated range start?
//
// Three outcomes, in order:
//   - head is an ancestor of tip (a merge or a fast-forward carried the chain
//     itself): carried, and the range starts at the writer's own base.
//   - every zero-context patch-id of the chain's commits appears on tip's
//     first-parent line (a cherry-pick of the range, conflict-resolved or not,
//     stays patch-equivalent only if no hunk was edited): carried, and the
//     range starts at the first parent of the earliest such commit.
//   - the chain's combined diff equals one commit's patch on that line (a
//     squash): carried, from that commit's parent.
//
// Anything else is `Carried = false` with the unmatched writer commits named.
// An empty base asks about the head commit alone, the contract an annotation
// had before it named a chain.
func gitIntegratedRange(execRoot, tip, head, base string) IntegratedRange {
	if execRoot == "" || tip == "" || head == "" {
		return IntegratedRange{}
	}
	if base == "" {
		base = head + "^"
	}
	chain, ok := gitPatchIDPairs(execRoot, base+".."+head)
	if !ok {
		return IntegratedRange{}
	}
	if exec.Command("git", gitDirArgs(execRoot, "merge-base", "--is-ancestor", head, tip)...).Run() == nil {
		return IntegratedRange{Known: true, Carried: true, From: gitRevParse(execRoot, base)}
	}
	if len(chain) == 0 {
		return IntegratedRange{} // no patch content to match
	}

	line, ok := gitPatchIDPairs(execRoot, "--first-parent", "-n", strconv.Itoa(integrationWalkLimit), tip)
	if !ok {
		return IntegratedRange{}
	}
	wanted := make(map[string]int, len(chain))
	for _, c := range chain {
		wanted[c.id]++
	}
	earliest := -1
	for i, c := range line {
		if wanted[c.id] > 0 {
			wanted[c.id]--
			earliest = i
		}
	}
	outstanding := 0
	for _, n := range wanted {
		outstanding += n
	}
	if outstanding == 0 && earliest >= 0 {
		return IntegratedRange{Known: true, Carried: true, From: gitRevParse(execRoot, line[earliest].commit+"^")}
	}

	if squash, ok := gitZeroContextRangePatchID(execRoot, base+".."+head); ok {
		for _, c := range line {
			if c.id == squash {
				return IntegratedRange{Known: true, Carried: true, From: gitRevParse(execRoot, c.commit+"^")}
			}
		}
	}

	var missing []string
	have := make(map[string]bool, len(line))
	for _, c := range line {
		have[c.id] = true
	}
	for _, c := range chain {
		if !have[c.id] {
			missing = append(missing, shortSHA(c.commit))
		}
	}
	return IntegratedRange{Known: true, Missing: missing}
}

type patchIDPair struct{ id, commit string }

// gitPatchIDPairs lists (zero-context patch-id, commit) for the non-merge
// commits `git log` selects with logArgs, newest first.
func gitPatchIDPairs(execRoot string, logArgs ...string) ([]patchIDPair, bool) {
	args := append([]string{"log", "--no-color", "--format=commit %H", "-p", "-U0",
		"--inter-hunk-context=0", "--no-merges", "--no-renames"}, logArgs...)
	out, err := exec.Command("git", gitDirArgs(execRoot, args...)...).Output()
	if err != nil {
		return nil, false
	}
	ids, ok := gitPatchIDLines(execRoot, out)
	if !ok {
		return nil, false
	}
	return ids, true
}

// gitPatchIDLines feeds patch text through `git patch-id --stable` and returns
// its "<patch-id> <commit>" lines in order.
func gitPatchIDLines(execRoot string, patch []byte) ([]patchIDPair, bool) {
	cmd := exec.Command("git", gitDirArgs(execRoot, "patch-id", "--stable")...)
	cmd.Stdin = strings.NewReader(string(patch))
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var pairs []patchIDPair
	for _, line := range strings.Split(string(out), "\n") {
		id, commit, found := strings.Cut(line, " ")
		if found && id != "" && commit != "" {
			pairs = append(pairs, patchIDPair{id: id, commit: strings.TrimSpace(commit)})
		}
	}
	return pairs, true
}

func gitRevParse(execRoot, rev string) string {
	out, err := exec.Command("git", gitDirArgs(execRoot, "rev-parse", "--verify", "--quiet", rev)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
