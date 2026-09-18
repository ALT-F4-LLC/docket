package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// THE ROUTING SWEEP — the committed check for workflow routing coverage.
//
// It answers one question over the SHIPPED `config/workflows/*.toml` corpus —
// the same one `docket` itself binds against — for every issue state an
// operator can create, does exactly one workflow bind? Zero means the issue
// cannot activate at all; more than one means activation refuses with the
// exactly-one-match error.
//
// WHY THIS IS A GO TEST AND NOT A SHELL GATE (AC-3). The sweep must
// evaluate the SAME predicate binding evaluates. `Matches` conjoins four terms
// (match.go:43-63) and the original scratch sweep reimplemented three of them,
// silently omitting `labels_all` — harmless only because no workflow used it,
// and undetectable because nothing cross-checked the two implementations. A
// shell gate would have to reimplement the predicate again and inherit exactly
// that failure mode. Here `Parse` and `Matches` are the production functions,
// so the check cannot drift from the behaviour it is checking.
//
// WHY IT READS THE RESOLVED INSTANCE CONFIG. Every other test in the corpus
// builds workflows in a `t.TempDir`; this one deliberately reaches out of the
// package to the same directories `config.Config.InstanceConfigDirs` names —
// the shared store's `config/workflows` and, when present, the repository's
// own `.docket/config/workflows` — because that union is what the engine
// actually binds against (autoregister.go). Sweeping anything else would
// certify a corpus no invocation uses.
//
// WHAT "EVERY ISSUE STATE" EXCLUDES. A corpus may RESERVE a label: every
// workflow declines it in `unless_labels` and none selects on it, so an issue
// carrying it binds nothing in every state — on purpose. The shared store did
// exactly that when it dropped its never-run `release` and `retro` pipelines
// and kept each sibling's exclusion "so a stray label fails activation loudly
// instead of silently binding standard-change". That refusal is routing
// working as designed, not a gap, so the sweep sets reserved labels aside —
// derived from the clauses, asserted through the production predicate, and
// logged by name (partitionLabels, routableVocabulary) — and demands
// exactly-one-match over everything that remains.

// sweepLabels is the label vocabulary the [match] clauses discriminate on:
// every label named in a `labels_any`, `labels_all`, or `unless_labels` across
// the set, plus the ones the pipelines are selected by. partitionLabels then
// splits it into what the sweep enumerates and what the corpus reserves.
//
// It is derived from the parsed clauses rather than hand-listed, so a label
// introduced by a future workflow enters the sweep automatically. A
// hand-maintained list is the same drift this check exists to catch.
func sweepLabels(defs []*Definition) []string {
	seen := make(map[string]struct{})
	for _, d := range defs {
		if d.Match == nil {
			continue
		}
		for _, group := range [][]string{
			d.Match.LabelsAny, d.Match.LabelsAll, d.Match.UnlessLabels,
		} {
			for _, l := range group {
				seen[l] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// labelPartition is sweepLabels split three ways by how the corpus treats
// each label — see partitionLabels.
type labelPartition struct {
	routable []string // enumerated by the sweep: selected by some workflow, or orphaned
	reserved []string // declined by every workflow, selected by none: set aside
	// orphaned maps a label no workflow selects on but only SOME decline to
	// the workflows that do not decline it — an exclusion that lost its
	// owner without becoming a reservation.
	orphaned map[string][]string
}

// partitionLabels splits the vocabulary into the ROUTABLE labels the sweep
// enumerates and the RESERVED labels it deliberately leaves out.
//
// A label is reserved when NO workflow selects on it (it appears in no
// `labels_any` or `labels_all`) and EVERY workflow declines it (each one has a
// `[match]` table naming it in `unless_labels`). Every state carrying such a
// label binds nothing — each clause's exclusion term fires — and that is the
// corpus authors' decision made once per workflow, not an omission; a
// workflow that starts selecting on the label lifts the reservation on its
// own, because "selected by none" stops holding.
//
// Unanimity is what makes it intent. A label that some workflows decline and
// none selects is an ORPHANED exclusion: an issue carrying it routes on its
// other labels through whichever workflows lack the term and binds nothing
// when those are absent. Orphans stay routable so the sweep surfaces their
// states as the gaps they are, and routableVocabulary names the cause
// alongside. A workflow with no `[match]` table admits everything, so its
// presence makes every unselected label an orphan rather than a reservation.
func partitionLabels(defs []*Definition, labels []string) labelPartition {
	selected := make(map[string]struct{})
	for _, d := range defs {
		if d.Match == nil {
			continue
		}
		for _, l := range d.Match.LabelsAny {
			selected[l] = struct{}{}
		}
		for _, l := range d.Match.LabelsAll {
			selected[l] = struct{}{}
		}
	}

	p := labelPartition{orphaned: make(map[string][]string)}
	for _, l := range labels {
		if _, ok := selected[l]; ok {
			p.routable = append(p.routable, l)
			continue
		}
		var admits []string
		for _, d := range defs {
			if d.Match == nil || !slices.Contains(d.Match.UnlessLabels, l) {
				admits = append(admits, d.Pipeline.Name)
			}
		}
		if len(admits) == 0 {
			p.reserved = append(p.reserved, l)
			continue
		}
		p.routable = append(p.routable, l)
		p.orphaned[l] = admits
	}
	return p
}

// routableVocabulary is the prologue the three sweeps share: derive the
// vocabulary, set the reserved labels aside, ASSERT what makes them reserved,
// and enforce the non-vacuity floor on what remains.
//
// The reserved set is asserted rather than merely skipped. partitionLabels
// reads `unless_labels` directly — a re-implementation of one of the four
// terms, which is exactly what this file's header warns against — so every
// reserved label is re-checked through the production predicate: for every
// kind, the bare state must bind NO workflow. The set is then logged by name,
// so the test output states which labels the corpus refuses by design. An
// orphaned exclusion fails here with its cause, on top of the states the
// sweep itself reports.
func routableVocabulary(t *testing.T, defs []*Definition, kinds []string) (routable, reserved []string) {
	t.Helper()
	p := partitionLabels(defs, sweepLabels(defs))

	orphans := make([]string, 0, len(p.orphaned))
	for l := range p.orphaned {
		orphans = append(orphans, l)
	}
	sort.Strings(orphans)
	for _, l := range orphans {
		t.Errorf("label %q is selected by no workflow and declined by only some: %v admit it; "+
			"either every workflow must decline it (reserving it, so the label fails "+
			"activation loudly) or one must select on it", l, p.orphaned[l])
	}

	for _, l := range p.reserved {
		for _, kind := range kinds {
			subject := Subject{Kind: kind, Labels: []string{l}}
			for _, d := range defs {
				if d.Match.Matches(subject) {
					t.Errorf("reserved label %q binds workflow %q at kind=%s; a label every "+
						"clause declines cannot bind, so the partition and the predicate disagree",
						l, d.Pipeline.Name, kind)
				}
			}
		}
	}
	if len(p.reserved) > 0 {
		t.Logf("reserved labels, declined by every workflow and selected by none, so every "+
			"state carrying one binds nothing by design and is left out of the sweep: %v",
			p.reserved)
	}

	requireNonVacuousLabels(t, p.routable)
	return p.routable, p.reserved
}

// minShippedWorkflows and minSweepLabels are non-vacuity floors (AC-5): a
// corpus that shrinks toward "one workflow with a match clause that
// discriminates on nothing" still parses and still sweeps, but sweeps a
// near-empty state space and PASSES — a degenerate corpus of exactly that
// shape was reproduced logging "swept 5 states ... of 0 labels" against 465
// for the real one. Both floors sit well below the shipped corpus (8
// workflows; 11 labels named, 9 routable and 2 reserved, as of this writing)
// so ordinary corpus edits do not trip them, and well above the degenerate
// case so a real shrink does.
const (
	minShippedWorkflows = 6
	minSweepLabels      = 4
)

// workflowDirs returns the ordered directories loadShippedWorkflows reads,
// cheapest-and-most-specific first (AC-4/AC-5):
//
//  1. DOCKET_SWEEP_WORKFLOWS_DIR, if set — the single directory the check is
//     pointed at to prove it can fail against a KNOWN-BAD config. A check
//     nobody has ever seen fail is a check nobody knows is wired up. Product
//     code never reads this variable; only this test does.
//  2. Otherwise, `workflows/` under every root config.Config.InstanceConfigDirs
//     names for the current invocation (env/local: the store's own config;
//     global: the shared store then the repository's own additions). This is
//     the SAME resolution the engine uses to find workflows to bind
//     (autoregister.go), so the sweep can never certify a corpus the engine
//     would never load.
func workflowDirs() ([]string, error) {
	if override := os.Getenv("DOCKET_SWEEP_WORKFLOWS_DIR"); override != "" {
		return []string{override}, nil
	}
	cfg, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, root := range cfg.InstanceConfigDirs() {
		dirs = append(dirs, filepath.Join(root, "workflows"))
	}
	return dirs, nil
}

// loadShippedWorkflows parses every workflow the current invocation would
// bind against (see workflowDirs). A root that does not exist is tolerated —
// a repository that ships no `.docket/config/workflows` additions of its own
// still resolves cleanly to the shared store alone — but any other read
// error, or a parse error, fails the test outright.
//
// KNOWN GAP: when NO resolved root exists (e.g. an unprovisioned checkout —
// including today's CI, which never populates the shared store), this hard
// fails below rather than skipping. That is deliberate, not an oversight: a
// bare skip here would be a hollow green on a guard whose entire purpose is
// to fail when routing regresses, and this file's declared scope does not
// reach the two things that would actually fix it — provisioning the shared
// store in CI, or shipping a corpus this commit owns. See the DKT1-C1/C3 gap
// filed alongside this change for the tracked follow-up.
func loadShippedWorkflows(t *testing.T) []*Definition {
	t.Helper()

	dirs, err := workflowDirs()
	testsupport.Must(t, err, "resolving workflow directories: %v", err)

	var defs []*Definition
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			testsupport.Must(t, err, "reading %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			testsupport.Must(t, err, "reading %s: %v", e.Name(), err)
			def, err := Parse(src)
			testsupport.Must(t, err, "parsing %s: %v", e.Name(), err)
			defs = append(defs, def)
		}
	}
	if len(defs) == 0 {
		t.Fatalf("no workflows found across %v; set DOCKET_SWEEP_WORKFLOWS_DIR to "+
			"point at a config/workflows directory (commonly $HOME/.docket/config/workflows, "+
			"the shared store's default), or provision the shared store", dirs)
	}
	// The floor guards the default (unoverridden) resolution against drift — a
	// DOCKET_SWEEP_WORKFLOWS_DIR fixture is deliberately small (AC-4's
	// known-bad corpus is one or two definitions) and is exempt.
	if os.Getenv("DOCKET_SWEEP_WORKFLOWS_DIR") == "" && len(defs) < minShippedWorkflows {
		t.Fatalf("only %d shipped workflows found across %v, want at least %d; "+
			"the sweep would certify a shrunken corpus", len(defs), dirs, minShippedWorkflows)
	}
	return defs
}

// requireNonVacuousLabels enforces the label half of the non-vacuity floor
// (see minSweepLabels): a corpus whose match clauses stop discriminating on
// labels still sweeps and still passes, over a state space too small to mean
// anything. It is measured over the ROUTABLE vocabulary — a corpus that
// reserved every label it names would sweep nothing and must trip it.
func requireNonVacuousLabels(t *testing.T, labels []string) {
	t.Helper()
	if len(labels) < minSweepLabels {
		t.Fatalf("only %d routable labels discriminated across the swept corpus, want at least %d; "+
			"a corpus whose match clauses stopped discriminating on labels would sweep a "+
			"near-empty state space and pass", len(labels), minSweepLabels)
	}
}

// subsetsUpTo returns every subset of items with size <= max.
func subsetsUpTo(items []string, max int) [][]string {
	out := [][]string{{}}
	var rec func(start int, cur []string)
	rec = func(start int, cur []string) {
		if len(cur) == max {
			return
		}
		for i := start; i < len(items); i++ {
			next := append(append([]string{}, cur...), items[i])
			out = append(out, next)
			rec(i+1, next)
		}
	}
	rec(0, nil)
	return out
}

// sweepResult counts the two failure modes over a set of issue states.
type sweepResult struct {
	zero  []string // human-readable states no workflow binds
	multi []string // states more than one workflow binds
	total int
}

// sweep evaluates every (kind x label-subset) point against the parsed
// clauses, using the production predicate.
func sweep(defs []*Definition, kinds []string, labels []string, maxSubset int) sweepResult {
	var res sweepResult
	for _, subset := range subsetsUpTo(labels, maxSubset) {
		for _, kind := range kinds {
			res.total++
			subject := Subject{Kind: kind, Labels: subset}

			var matched []string
			for _, d := range defs {
				if d.Match.Matches(subject) {
					matched = append(matched, d.Pipeline.Name)
				}
			}

			state := fmt.Sprintf("kind=%s labels=[%s]", kind, strings.Join(subset, " "))
			switch {
			case len(matched) == 0:
				res.zero = append(res.zero, state)
			case len(matched) > 1:
				sort.Strings(matched)
				res.multi = append(res.multi,
					state+" -> "+strings.Join(matched, ", "))
			}
		}
	}
	return res
}

func kindStrings() []string {
	kinds := model.ValidIssueKinds()
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// TestShippedWorkflowsRouteEveryIssueState is AC-1: the committed
// check that re-derives the routing table and fails if either count regresses.
//
// Both directions are asserted. Zero-match means an issue in that state cannot
// activate at all; multi-match means activation refuses with exactly-one-match.
// The multi count was already 0 before this check existed and must stay there —
// a sweep that only watched the zero side would let a precedence bug in.
//
// "Every issue state" is every state over the ROUTABLE vocabulary: a state
// carrying a reserved label binds nothing by the corpus's own design and is
// asserted as such in routableVocabulary rather than counted as a gap here.
func TestShippedWorkflowsRouteEveryIssueState(t *testing.T) {
	defs := loadShippedWorkflows(t)
	labels, reserved := routableVocabulary(t, defs, kindStrings())
	res := sweep(defs, kindStrings(), labels, 3)

	t.Logf("swept %d states over %d kinds x label subsets<=3 of %d routable labels (%d reserved)",
		res.total, len(kindStrings()), len(labels), len(reserved))

	if len(res.zero) > 0 {
		t.Errorf("%d issue states bind NO workflow and cannot activate; first 10:\n  %s",
			len(res.zero), strings.Join(first(res.zero, 10), "\n  "))
	}
	if len(res.multi) > 0 {
		t.Errorf("%d issue states bind MORE THAN ONE workflow, so activation refuses; first 10:\n  %s",
			len(res.multi), strings.Join(first(res.multi, 10), "\n  "))
	}
}

// TestShippedWorkflowsRouteFullPowerset widens the sweep to every label
// combination, not just those of size <= 3. The bounded sweep is the cheap
// everyday check; this one is the honest one — a precedence gap needing four
// simultaneous labels is still a gap.
func TestShippedWorkflowsRouteFullPowerset(t *testing.T) {
	defs := loadShippedWorkflows(t)
	labels, reserved := routableVocabulary(t, defs, kindStrings())
	res := sweep(defs, kindStrings(), labels, len(labels))

	t.Logf("swept %d states over the full 2^%d powerset of routable labels (%d reserved)",
		res.total, len(labels), len(reserved))

	if len(res.zero) > 0 {
		t.Errorf("%d issue states bind NO workflow; first 10:\n  %s",
			len(res.zero), strings.Join(first(res.zero, 10), "\n  "))
	}
	if len(res.multi) > 0 {
		t.Errorf("%d issue states bind MORE THAN ONE workflow; first 10:\n  %s",
			len(res.multi), strings.Join(first(res.multi, 10), "\n  "))
	}
}

// TestSweepExercisesAllFourMatchTerms is AC-2.
//
// The scratch sweep this check replaces implemented three of the four terms
// `Matches` conjoins, omitting `labels_all`. That omission was invisible
// because no workflow used the term — so the guard cannot be "the sweep
// mentions labels_all", it has to be a case where a definition using
// labels_all produces a different verdict than one without it. If a future
// refactor drops the term from `Matches`, this fails.
func TestSweepExercisesAllFourMatchTerms(t *testing.T) {
	cases := []struct {
		name    string
		match   *Match
		subject Subject
		want    bool
	}{
		{"kind excludes", &Match{Kind: []string{"bug"}},
			Subject{Kind: "task"}, false},
		{"kind admits", &Match{Kind: []string{"bug"}},
			Subject{Kind: "bug"}, true},
		{"labels_any requires an intersection", &Match{LabelsAny: []string{"ui"}},
			Subject{Kind: "bug"}, false},
		{"labels_any admits on one", &Match{LabelsAny: []string{"ui"}},
			Subject{Kind: "bug", Labels: []string{"ui"}}, true},
		{"labels_all requires ALL", &Match{LabelsAll: []string{"a", "b"}},
			Subject{Kind: "bug", Labels: []string{"a"}}, false},
		{"labels_all admits on both", &Match{LabelsAll: []string{"a", "b"}},
			Subject{Kind: "bug", Labels: []string{"a", "b"}}, true},
		{"unless_labels excludes", &Match{UnlessLabels: []string{"retro"}},
			Subject{Kind: "bug", Labels: []string{"retro"}}, false},
		{"unless_labels wins over an inclusion",
			&Match{LabelsAny: []string{"ui"}, UnlessLabels: []string{"retro"}},
			Subject{Kind: "bug", Labels: []string{"ui", "retro"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.match.Matches(tc.subject); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNoKindListEnumeratesTheClosedSet is the closed-set AC-2.
//
// A `[match].kind` listing every valid kind decides nothing: `Matches` skips
// the term entirely when the list is empty (match.go:48), so an absent clause
// already admits every kind. The enumerated form is strictly worse — it must
// be maintained in nine places, and the day a sixth kind is added every one of
// them silently stops matching it.
//
// AC-4: a kind list that is genuinely NARROWER than the closed set is the
// clause discriminating, and is left alone. This check fires only on equality
// with the full set.
func TestNoKindListEnumeratesTheClosedSet(t *testing.T) {
	closed := make(map[string]struct{})
	for _, k := range model.ValidIssueKinds() {
		closed[string(k)] = struct{}{}
	}

	for _, d := range loadShippedWorkflows(t) {
		if d.Match == nil || len(d.Match.Kind) == 0 {
			continue // absent already matches every kind — the desired form
		}
		if len(d.Match.Kind) != len(closed) {
			continue // narrower, and therefore load-bearing
		}
		all := true
		for _, k := range d.Match.Kind {
			if _, ok := closed[k]; !ok {
				all = false
				break
			}
		}
		if all {
			t.Errorf("workflow %q enumerates the entire closed kind set %v in "+
				"[match].kind, which decides nothing and goes stale when a "+
				"sixth kind is added; delete the clause instead",
				d.Pipeline.Name, d.Match.Kind)
		}
	}
}

// TestAddingASixthKindKeepsEveryStateRoutable is the closed-set AC-3, expressed as a
// property rather than as a manual "add a kind, then revert" ritual.
//
// A throwaway kind stands in for the sixth kind someone adds later. With the
// inert lists deleted it routes like any other; with them present it binds
// nothing, which is the regression this pins.
func TestAddingASixthKindKeepsEveryStateRoutable(t *testing.T) {
	defs := loadShippedWorkflows(t)
	kinds := append(kindStrings(), "spike")
	labels, _ := routableVocabulary(t, defs, kinds)
	res := sweep(defs, kinds, labels, 3)

	if len(res.zero) > 0 {
		t.Errorf("adding a sixth kind left %d states unroutable, so a new kind "+
			"is silently unroutable across the whole set; first 10:\n  %s",
			len(res.zero), strings.Join(first(res.zero, 10), "\n  "))
	}
	if len(res.multi) > 0 {
		t.Errorf("adding a sixth kind produced %d ambiguous states; first 10:\n  %s",
			len(res.multi), strings.Join(first(res.multi, 10), "\n  "))
	}
}

func first(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// TestWorkflowDirsHonorsOverride pins the locator itself (AC-5): the override
// wins when set, and an unset override falls back to the resolved store —
// the two branches DKT-1's own fix touches, neither of which had a test.
func TestWorkflowDirsHonorsOverride(t *testing.T) {
	t.Run("override wins over the resolved store", func(t *testing.T) {
		override := t.TempDir()
		t.Setenv("DOCKET_SWEEP_WORKFLOWS_DIR", override)

		dirs, err := workflowDirs()
		testsupport.Must(t, err, "workflowDirs: %v", err)
		if len(dirs) != 1 || dirs[0] != override {
			t.Fatalf("workflowDirs() = %v, want [%s]", dirs, override)
		}
	})

	t.Run("empty override falls back to the resolved store", func(t *testing.T) {
		t.Setenv("DOCKET_SWEEP_WORKFLOWS_DIR", "")
		t.Setenv("DOCKET_PATH", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		// A directory outside any git repository and with no local .docket
		// store, so config.Resolve is forced onto the global-store branch —
		// the same branch the shared-store default resolves through.
		t.Chdir(t.TempDir())

		dirs, err := workflowDirs()
		testsupport.Must(t, err, "workflowDirs: %v", err)
		want := filepath.Join(home, ".docket", "config", "workflows")
		found := false
		for _, d := range dirs {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("workflowDirs() = %v, want it to include %s", dirs, want)
		}
	})
}

// TestKnownBadWorkflowsFailTheSweepThroughTheOverride is AC-4, driven
// end-to-end: point DOCKET_SWEEP_WORKFLOWS_DIR at a fixture of two workflows
// that both match everything (no `[match]` table — `Match.Matches` admits
// everything on a nil receiver, match.go:43-46) and confirm the sweep reports
// the resulting multi-match. Before this test, nothing exercised the override
// this file's own comment calls the proof the check is wired up.
func TestKnownBadWorkflowsFailTheSweepThroughTheOverride(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		"a.toml": "[pipeline]\nname = \"known-bad-a\"\nversion = 1\n",
		"b.toml": "[pipeline]\nname = \"known-bad-b\"\nversion = 1\n",
	}
	for name, src := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}
	t.Setenv("DOCKET_SWEEP_WORKFLOWS_DIR", dir)

	defs := loadShippedWorkflows(t)
	if len(defs) != len(fixtures) {
		t.Fatalf("loadShippedWorkflows found %d definitions in the fixture, want %d",
			len(defs), len(fixtures))
	}

	res := sweep(defs, kindStrings(), sweepLabels(defs), 3)
	if len(res.multi) == 0 {
		t.Fatalf("a known-bad corpus of %d workflows that all match everything produced "+
			"no multi-match states; the override path this check depends on is not wired up",
			len(fixtures))
	}
}
