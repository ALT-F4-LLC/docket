package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// BUMP REASON AND ISSUE SCOPE ON STAGED ROWS.
//
// A manifest's only cross-issue facts were the stage numbers, so a relay
// reading it could not tell a stage gap that encodes a genuine writer conflict
// (two issues whose scopes intersect, which it must serialize) from one that
// encodes cohort packing (a bounded class that was full, which it may ignore
// once a slot frees). `bump` says which, `bump_issue` names the other issue on
// a scope bump, and `scope` carries the row's own globs so the relay can test
// intersection itself for pairs the engine never co-staged.

// bumpHeadroomWorkflowSrc packs a bounded class: `a` and `b` are both
// claimable-now candidates of a class capped at one, and `c` sits behind `a`.
// The cap admits `a` alone, `b` stages at 1, and `c` — leveled to 1 by its own
// dependency — finds stage 1's single slot already spent and bumps for
// HEADROOM, with no scope anywhere in the fixture.
const bumpHeadroomWorkflowSrc = `
[pipeline]
name = "bump-headroom-fixture"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "a"
executor = "w"
class = "write"
emits = "change-summary"
after = []

[[step]]
name = "b"
executor = "w"
class = "write"
emits = "change-summary"
after = []

[[step]]
name = "c"
executor = "w"
class = "write"
emits = "change-summary"
after = ["a"]
`

// bumpScopeWorkflowSrc has no class bound at all, so the only thing that can
// move a row off its dependency level is scope disjointness within a stage.
const bumpScopeWorkflowSrc = `
[pipeline]
name = "bump-scope-fixture"
version = 1

[match]
kind = ["task"]

[[step]]
name = "a"
executor = "w"
emits = "change-summary"
after = []

[[step]]
name = "b"
executor = "w"
emits = "change-summary"
after = ["a"]
`

// bumpRow is one offered row's bump facts, keyed by instance-and-issue so the
// two-issue fixture's repeated instance names stay distinguishable.
type bumpRow struct {
	stage int
	bump  string
	issue string
	scope []string
}

// offeredBumps indexes an offer by `<issue> <instance>`.
func offeredBumps(rows []model.StepRow) map[string]bumpRow {
	out := make(map[string]bumpRow, len(rows))
	for _, r := range rows {
		out[r.Issue+" "+r.Instance] = bumpRow{r.Stage, r.Bump, r.BumpIssue, r.Scope}
	}
	return out
}

// TestStagedRowsCarryBumpReason is the whole carrier, one case per value. A
// stage number alone cannot distinguish a gap that encodes a writer conflict
// from one that encodes cohort packing, so a relay reading only stages had to
// treat every pair the engine never co-staged as conflicting.
func TestStagedRowsCarryBumpReason(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		conn := mustDB(t)
		run, _ := activatedRun(t, conn)

		answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
		testsupport.Must(t, err, "next: %v", err)

		// The standard fixture bounds no class and declares no scope, so every
		// row sits exactly at its dependency level: the ready row and the whole
		// staged closure behind it all report `none`.
		if len(answer.Steps) == 0 {
			t.Fatal("premise: the offer is empty")
		}
		sawStaged := false
		for _, r := range answer.Steps {
			if r.Status == db.StepStaged {
				sawStaged = true
			}
			if r.Bump != model.BumpNone {
				t.Errorf("%s bump = %q, want %q — nothing in this fixture can "+
					"pack a cohort", r.Instance, r.Bump, model.BumpNone)
			}
			if r.BumpIssue != "" {
				t.Errorf("%s carries bump_issue %q without a scope bump",
					r.Instance, r.BumpIssue)
			}
		}
		if !sawStaged {
			t.Error("premise: no staged row in the offer, so the `none` case " +
				"never reached a bumpable row")
		}
	})

	t.Run("headroom", func(t *testing.T) {
		conn := mustDB(t)
		registerSource(t, conn, []byte(bumpHeadroomWorkflowSrc), "bump-headroom.toml")
		issue := createIssue(t, conn, "packed", "a body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
		testsupport.Must(t, err, "next: %v", err)
		got := offeredBumps(answer.Steps)
		id := model.FormatID(issue)

		// `a` claims the one write slot now; `b` takes stage 1's; `c`, whose
		// own dependency levels it to 1, finds that slot spent and moves to 2.
		if row := got[id+" a@0"]; row.stage != 0 || row.bump != model.BumpNone {
			t.Errorf("a@0 = %+v, want stage 0 bump none", row)
		}
		if row := got[id+" b@0"]; row.stage != 1 || row.bump != model.BumpNone {
			t.Errorf("b@0 = %+v, want stage 1 bump none — its level is its "+
				"own, not a bump", row)
		}
		row := got[id+" c@0"]
		if row.stage != 2 || row.bump != model.BumpHeadroom {
			t.Errorf("c@0 = %+v, want stage 2 bump %q", row, model.BumpHeadroom)
		}
		if row.issue != "" {
			t.Errorf("c@0 bump_issue = %q; a headroom bump names no issue — "+
				"nothing about the two rows conflicts once a slot frees", row.issue)
		}
	})

	t.Run("scope", func(t *testing.T) {
		conn := mustDB(t)
		registerSource(t, conn, []byte(bumpScopeWorkflowSrc), "bump-scope.toml")
		first := createIssue(t, conn, "holds the tree", "a body", "task", nil)
		second := createIssue(t, conn, "wants the tree", "a body", "task", nil)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, first, `["x/**"]`),
			"declaring the first scope")
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, second, `["x/a"]`),
			"declaring the second scope")
		run := startRun(t, conn, first, second)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
		testsupport.Must(t, err, "next: %v", err)
		got := offeredBumps(answer.Steps)
		firstID, secondID := model.FormatID(first), model.FormatID(second)

		// The fixture bounds no class, so the ONLY thing that can move a row
		// is the intersection `x/a` has with `x/**`: the second issue's `a`
		// cannot share stage 1 with the first issue's `b`.
		row := got[secondID+" a@0"]
		if row.bump != model.BumpScope {
			t.Errorf("%s a@0 bump = %q, want %q", secondID, row.bump, model.BumpScope)
		}
		if row.issue != firstID {
			t.Errorf("%s a@0 bump_issue = %q, want %q — a relay must stay "+
				"serialized behind the issue it actually conflicts with",
				secondID, row.issue, firstID)
		}
		if row.stage <= got[firstID+" b@0"].stage {
			t.Errorf("%s a@0 at stage %d does not sit after the holder at "+
				"stage %d", secondID, row.stage, got[firstID+" b@0"].stage)
		}

		// AC2: every row carries its own issue's declared globs, so a reader
		// can test intersection for pairs this offer never placed in one
		// cohort — the bump reason only ever answers the pairs it did.
		for key, want := range map[string][]string{
			firstID + " a@0":  {"x/**"},
			firstID + " b@0":  {"x/**"},
			secondID + " a@0": {"x/a"},
			secondID + " b@0": {"x/a"},
		} {
			gotScope := got[key].scope
			if len(gotScope) != len(want) || gotScope[0] != want[0] {
				t.Errorf("%s scope = %v, want the issue's declared %v",
					key, gotScope, want)
			}
		}
	})
}

// TestDispatchVerifyNormalizesBump: the new fields are OPEN-TIME facts, hashed
// into the manifest at open and recomputed by verify against a world that has
// moved. A bump is a fact about which rows shared a cohort then, so the batch
// working as scheduled — a cohort-mate recording, freeing the slot the bump
// paid for — recomputes to a different answer. Normalizing them the way Stage
// and Conditional are normalized is what keeps that progress from reading as
// drift.
func TestDispatchVerifyNormalizesBump(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(bumpHeadroomWorkflowSrc), "bump-headroom.toml")
	issue := createIssue(t, conn, "packed", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	stored := offeredBumps(manifest.Rows)
	id := model.FormatID(issue)
	if row := stored[id+" c@0"]; row.bump != model.BumpHeadroom {
		t.Fatalf("premise: c@0 opened with bump %q, want %q — this test would "+
			"pass vacuously", row.bump, model.BumpHeadroom)
	}

	// Verify with nothing moved: the recomputation is identical, so this half
	// would pass with or without the normalization. It is the control.
	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify before any progress: %v", err)
	if !result.Verified {
		t.Fatalf("an untouched manifest failed to verify: %+v / %+v",
			result, mismatch)
	}

	// Now the batch works: `a` records, freeing the write slot its cohort held.
	// `c` recomputes at a lower stage with bump `none` where the manifest
	// stored `headroom` — the drift this normalization exists to absorb.
	stepID := stepIDByInstance(t, conn, "a@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claiming a@0: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "recording a@0: %v", err)

	result, mismatch, err = e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify after the batch progressed: %v", err)
	if !result.Verified {
		t.Errorf("verify reported drift after a cohort-mate recorded: %+v\n%+v\n"+
			"a bump is an open-time fact about the offer's own shape; the "+
			"batch working as scheduled is not a conflict", result, mismatch)
	}
}

// bumpDoubleWorkflowSrc refuses one staged row by BOTH rules on its way up,
// once in each order. Run over two issues whose scopes intersect (`x/**`
// admitted first, `x/a` second), `a` is the only row outside the write class,
// and the first issue's placements go:
//
//   - `p` takes stage 1's write slot and `q` headroom-bumps to 2, so the second
//     issue's `a` — scope-refused at 1 and 2 — lands at 3, holding `x/a` there
//     with the write slot free.
//   - `s`, leveled to 2 behind `p`, is refused for HEADROOM at 2 (`q`), then
//     for SCOPE at 3 (`a`), and lands at 4.
//   - `u`, leveled to 3 behind `q`, is refused for SCOPE at 3 (`a`), then for
//     HEADROOM at 4 (`s`), and lands at 5.
//
// Headroom is tested before scope at each stage, which is why each refusal
// above is the only one that stage reports.
const bumpDoubleWorkflowSrc = `
[pipeline]
name = "bump-double-fixture"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "a"
executor = "w"
emits = "change-summary"
after = []

[[step]]
name = "p"
executor = "w"
class = "write"
emits = "change-summary"
after = ["a"]

[[step]]
name = "q"
executor = "w"
class = "write"
emits = "change-summary"
after = ["a"]

[[step]]
name = "s"
executor = "w"
class = "write"
emits = "change-summary"
after = ["p"]

[[step]]
name = "u"
executor = "w"
class = "write"
emits = "change-summary"
after = ["q"]
`

// TestStagedRowsBumpScopeBeatsHeadroom: a row refused by both rules reports
// `scope` whichever came first. A headroom refusal retires once a slot frees;
// a scope conflict stays serialized however the cohort empties, so a relay
// told `headroom` would treat a writer conflict as packing it may ignore.
func TestStagedRowsBumpScopeBeatsHeadroom(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(bumpDoubleWorkflowSrc), "bump-double.toml")
	first := createIssue(t, conn, "holds the tree", "a body", "task", nil)
	second := createIssue(t, conn, "wants the tree", "a body", "task", nil)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, first, `["x/**"]`),
		"declaring the first scope")
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, second, `["x/a"]`),
		"declaring the second scope")
	run := startRun(t, conn, first, second)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	got := offeredBumps(answer.Steps)
	firstID, secondID := model.FormatID(first), model.FormatID(second)

	// The two holders every double refusal below depends on: `q` keeps stage
	// 2's write slot, and the second issue's `a` keeps `x/a` at stage 3.
	if row := got[firstID+" q@0"]; row.stage != 2 || row.bump != model.BumpHeadroom {
		t.Fatalf("premise: %s q@0 = %+v, want stage 2 bump %q", firstID, row,
			model.BumpHeadroom)
	}
	if row := got[secondID+" a@0"]; row.stage != 3 || row.bump != model.BumpScope {
		t.Fatalf("premise: %s a@0 = %+v, want stage 3 bump %q", secondID, row,
			model.BumpScope)
	}

	for _, tc := range []struct {
		name, instance string
		stage          int
	}{
		// Scope at 3, then headroom at 4: a last-refusal-wins guard reports
		// headroom.
		{"scope then headroom", "u@0", 5},
		// Headroom at 2, then scope at 3: a first-refusal-wins guard reports
		// headroom.
		{"headroom then scope", "s@0", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := got[firstID+" "+tc.instance]
			if row.bump != model.BumpScope {
				t.Errorf("%s %s bump = %q, want %q — scope beats headroom "+
					"whichever refused first", firstID, tc.instance, row.bump,
					model.BumpScope)
			}
			if row.stage != tc.stage {
				t.Errorf("%s %s stage = %d, want %d", firstID, tc.instance,
					row.stage, tc.stage)
			}
		})
	}
}
