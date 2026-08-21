package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-255 — activation says which declared gates cannot run here.
//
// The fence report already answered this for HARVESTED commands. Nothing asked
// it of a DECLARED gate, so the gap was found mid-run, one gate at a time, when
// the gate fired and found nothing to run. All 34 gate-unmatched events of the
// epoch were missing-entry cases — not moved repos, not argv drift, not prefix
// mismatches. Every one was knowable before the run started.

// preflightDefs builds a definition map with the gates a case needs.
func preflightDefs(gates ...workflow.Gate) map[int]*workflow.Definition {
	return map[int]*workflow.Definition{
		1: {
			Pipeline: workflow.Pipeline{Name: "unit-run", Version: 1},
			Steps:    []*workflow.Step{{Name: "implement", Gates: gates}},
		},
	}
}

func preflightRow(rows []GatePreflight, gate string) (GatePreflight, bool) {
	for _, r := range rows {
		if r.Gate == gate {
			return r, true
		}
	}
	return GatePreflight{}, false
}

// TestGatePreflightNamesTheGatesWithNoEntry is the whole point: the operator
// learns in seconds rather than 20 minutes in.
func TestGatePreflightNamesTheGatesWithNoEntry(t *testing.T) {
	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	load := sandboxTrust(t, trust.Entry{
		Name: "build", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv), Global: true,
	})

	rows, err := BuildGatePreflight(preflightDefs(
		workflow.Gate{Name: "build"},
		workflow.Gate{Name: "secret-scan"},
		workflow.Gate{Name: "ac-commands"},
	), load, repoRoot)
	if err != nil {
		t.Fatalf("BuildGatePreflight: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	if got, _ := preflightRow(rows, "build"); !got.Matched {
		t.Errorf("`build` has an entry and reports unmatched: %+v", got)
	}
	for _, name := range []string{"secret-scan", "ac-commands"} {
		got, _ := preflightRow(rows, name)
		if got.Matched {
			t.Errorf("%s has no entry and reports matched", name)
		}
		if got.Reason == "" {
			t.Errorf("%s reports unmatched with no reason; the preflight and the "+
				"mid-run diagnostic must say the same thing", name)
		}
		// Naming the declaring workflow is what turns "add an entry" into
		// "add an entry and this is what it unblocks".
		if len(got.Workflows) == 0 || !strings.Contains(got.Workflows[0], "unit-run") {
			t.Errorf("%s does not name the workflow declaring it: %v",
				name, got.Workflows)
		}
	}
}

// TestGatePreflightExcludesFenceGates: they belong to the fence report.
//
// A fence gate's command comes from an issue body and is resolved against the
// exact argv harvested. Reporting it here would double-count it and, worse,
// report it unmatched whenever its body carried no fenced block at all.
func TestGatePreflightExcludesFenceGates(t *testing.T) {
	repoRoot := t.TempDir()
	rows, err := BuildGatePreflight(preflightDefs(
		workflow.Gate{Name: "ac-commands", Source: "fence:acceptance"},
		workflow.Gate{Name: "build"},
	), sandboxTrust(t), repoRoot)
	if err != nil {
		t.Fatalf("BuildGatePreflight: %v", err)
	}
	if _, found := preflightRow(rows, "ac-commands"); found {
		t.Error("a fence gate appears in the gate preflight; its command is the " +
			"fence report's subject and it has no argv here to resolve")
	}
	if _, found := preflightRow(rows, "build"); !found {
		t.Error("a named gate is missing from the preflight")
	}
}

// TestGatePreflightDeduplicatesAcrossDeclarationSites.
//
// One gate is commonly declared by several steps of several workflows. An
// operator wants one row per MISSING ENTRY — the thing they will act on — not
// one per declaration site.
func TestGatePreflightDeduplicatesAcrossDeclarationSites(t *testing.T) {
	repoRoot := t.TempDir()
	defs := map[int]*workflow.Definition{
		1: {
			Pipeline: workflow.Pipeline{Name: "unit-run", Version: 1},
			Steps: []*workflow.Step{
				{Name: "implement", Gates: []workflow.Gate{{Name: "build"}}},
				{Name: "fix", Gates: []workflow.Gate{{Name: "build"}}},
			},
		},
		2: {
			Pipeline: workflow.Pipeline{Name: "docs-only", Version: 3},
			Steps: []*workflow.Step{
				{Name: "author", Gates: []workflow.Gate{{Name: "build"}}},
			},
		},
	}

	rows, err := BuildGatePreflight(defs, sandboxTrust(t), repoRoot)
	if err != nil {
		t.Fatalf("BuildGatePreflight: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for one gate declared three times, want 1: %+v",
			len(rows), rows)
	}
	// Both workflows are named, sorted, so two activations render identically.
	want := []string{"docs-only@3", "unit-run@1"}
	if len(rows[0].Workflows) != 2 ||
		rows[0].Workflows[0] != want[0] || rows[0].Workflows[1] != want[1] {
		t.Errorf("workflows = %v, want %v (sorted)", rows[0].Workflows, want)
	}
}

// TestGatePreflightReportsAStubbedEntry (DKT-265 crossing DKT-255).
//
// A gate that WILL run and will measure nothing is a different answer from one
// that will not run, and both differ from a real check. An operator reading a
// green preflight should not have to open the trust store to learn which one
// this run has.
func TestGatePreflightReportsAStubbedEntry(t *testing.T) {
	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	load := sandboxTrust(t, trust.Entry{
		Name: "secret-scan", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Global: true, Stub: true,
	})

	rows, err := BuildGatePreflight(
		preflightDefs(workflow.Gate{Name: "secret-scan"}), load, repoRoot)
	if err != nil {
		t.Fatalf("BuildGatePreflight: %v", err)
	}
	got, _ := preflightRow(rows, "secret-scan")
	if !got.Matched {
		t.Fatalf("a stub entry must still MATCH: %+v", got)
	}
	if !got.Stub {
		t.Error("a stubbed entry is not reported as a stub; the preflight would " +
			"read green for a gate that measures nothing")
	}
}

// TestRenderGatePreflightIsSilentWhenEveryGateResolves.
//
// Silence on success is the point. An activation already prints a bound-issue
// roster, a pin count, a fence report, and scope warnings; a fifth block saying
// "all 6 gates are fine" on every run is how an operator learns to skip the
// region of the screen where the one that is NOT fine will appear.
func TestRenderGatePreflightIsSilentWhenEveryGateResolves(t *testing.T) {
	var buf strings.Builder
	RenderGatePreflight(&buf, []GatePreflight{
		{Gate: "build", Matched: true, Entry: "build"},
		{Gate: "tests", Matched: true, Entry: "tests"},
	})
	if buf.String() != "" {
		t.Errorf("the preflight printed on a fully-resolved run:\n%s", buf.String())
	}
}

// TestRenderGatePreflightNamesTheRemedy: a warning that does not say what to do
// costs the operator the lookup it was supposed to save.
func TestRenderGatePreflightNamesTheRemedy(t *testing.T) {
	var buf strings.Builder
	RenderGatePreflight(&buf, []GatePreflight{
		{Gate: "secret-scan", Workflows: []string{"unit-run@1"},
			Reason: "no trust entry named secret-scan"},
	})
	out := buf.String()
	for _, needle := range []string{"secret-scan", "unit-run@1", "docket trust add", "on_fail"} {
		if !strings.Contains(out, needle) {
			t.Errorf("the warning does not mention %q:\n%s", needle, out)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-266 — the hold policy is visible before the run
// ---------------------------------------------------------------------------

// TestHoldPolicyRendersOnlyWhenConfigured.
//
// The audit found 52 step-held events, 50 resolved by an operator directly, and
// ZERO hold-panel votes, against config reading `set` in both projects. The
// engine was doing exactly what it was told — the config postdates those runs,
// and TestHeldStepMintsVoteWhenConfigured has always proven the path. What was
// missing is that nothing SAID which policy was in force, so a configured
// surface that is inert and one that is broken looked identical.
func TestHoldPolicyRendersOnlyWhenConfigured(t *testing.T) {
	cases := []struct {
		name   string
		policy HoldPolicy
		want   []string
		silent bool
	}{
		{
			name: "a configured panel says who decides",
			policy: HoldPolicy{
				Rule:   "hold-panel",
				Voters: []string{"tribunal-architecture", "tribunal-security"},
				Panel:  true,
			},
			want: []string{"hold-panel", "tribunal-architecture", "tribunal-security"},
		},
		{
			name:   "the unconfigured default earns no line",
			policy: HoldPolicy{},
			silent: true,
		},
		{
			name:   "a rule with no voters WARNS",
			policy: HoldPolicy{Rule: "hold-panel"},
			want:   []string{"HALF configured", "ONE OPERATOR", "vote.hold.voters"},
		},
		{
			name:   "voters with no rule WARNS",
			policy: HoldPolicy{Voters: []string{"alice", "bob"}},
			want:   []string{"HALF configured", "ONE OPERATOR", "vote.hold.rule"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			RenderHoldPolicy(&buf, tc.policy)
			out := buf.String()

			if tc.silent {
				if out != "" {
					t.Errorf("printed on the unconfigured default:\n%s", out)
				}
				return
			}
			for _, needle := range tc.want {
				if !strings.Contains(out, needle) {
					t.Errorf("output does not mention %q:\n%s", needle, out)
				}
			}
		})
	}
}

// TestHoldPolicyPanelRequiresBothKeys pins the derivation that was invisible.
//
// A half-configured pair silently means "one operator" — the state an operator
// who set only one key would least expect and least easily notice, which is why
// `Panel` is derived once here rather than left for each reader to recompute.
func TestHoldPolicyPanelRequiresBothKeys(t *testing.T) {
	cases := map[string]struct {
		tally holdTally
		want  bool
	}{
		"both set":     {holdTally{rule: "hold-panel", voters: []string{"a"}}, true},
		"no voters":    {holdTally{rule: "hold-panel"}, false},
		"no rule":      {holdTally{voters: []string{"a"}}, false},
		"neither":      {holdTally{}, false},
		"empty voters": {holdTally{rule: "hold-panel", voters: []string{}}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := holdPolicyOf(tc.tally).Panel; got != tc.want {
				t.Errorf("Panel = %v, want %v", got, tc.want)
			}
		})
	}
}
