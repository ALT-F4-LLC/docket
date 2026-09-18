package engine

import (
	"database/sql"
	"reflect"
	"sort"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-868 — a run's tier routing was unaggregatable.
//
// `run report` rolled step metadata up as key -> distinct-values-with-counts,
// which answers a run-level question and destroys a per-step one: grouping BY
// KEY is exactly what discards which values two keys took TOGETHER on one step.
// A bag whose keys are a REQUEST and its RESOLUTION therefore had no reader.
// RUN-51's rollup published one key with no `low` value and its partner with
// one — a real mismatch, on exactly one step, that the document could not name
// — and recovering it meant `docket step show` per step: the audit that found
// this ran ~90 of them across 19 runs.
//
// THE DISPOSITION, and why not the other one. The issue offers an alternative:
// an `escalated` / `variant-resolved` event kind emitted when resolution
// differs from the request. That remedy is not available to core and must not
// be made available. Deciding that two values "differ" in a way worth an event
// requires core to know WHICH keys are the request and the resolution, which is
// R7's line (db.MetadataRollup's comment, TestMetadataRollupReadsNoKey) and
// docs/design/genericity.md's whole subject — and the vocabulary it would carry
// is the vocabulary scripts/qa/genericity.sh bans from core surface by name.
// The event set is closed besides (event.go), and admits kinds for TRANSITIONS
// CORE PERFORMS; a dispatcher resolving a step to a variant is not one core
// observes at all — DKT-867 had to have the dispatcher DECLARE its cost scaling
// for precisely that reason.
//
// So the remedy is the read half: publish the bag the rollup collapsed, per
// step, joined to that step's status. Core still reads no key; the consumer
// makes the comparison, in one `run report` instead of an N-step-show sweep.
//
// The keys below are the corpus's own (`*_requested` / `*_resolved`) in the
// escalation cases and a deliberately unrelated vocabulary in the genericity
// case, because core must not be able to tell the two apart.

// tierAudit is the retro sweep this issue exists to make possible, written the
// way a consumer would write it against one `run report`: find every step whose
// resolution disagrees with its request.
//
// It is a TEST-SIDE function on purpose. Core publishes the pairs and never
// makes this comparison — that is the genericity line, and a helper living here
// rather than in the engine is what keeps the test honest about which side of
// it the knowledge lives on.
func tierAudit(report *RunReport, requested, resolved string) []string {
	var drifted []string
	for _, a := range report.Attempts {
		want, hasWant := a.Metadata[requested]
		got, hasGot := a.Metadata[resolved]
		if hasWant && hasGot && want != got {
			drifted = append(drifted, a.Instance)
		}
	}
	sort.Strings(drifted)
	return drifted
}

// setStepBags writes one bag per instance directly to the column, which is
// where every writer — definition, claim, completion, fail, annotate — lands
// after its own merge. The subject here is the READ, so the fixture states the
// stored shape rather than driving four verbs to produce it.
func setStepBags(t *testing.T, conn *sql.DB, bags map[string]string) {
	t.Helper()
	for instance, bag := range bags {
		execSQL(t, conn, `UPDATE steps SET metadata = ? WHERE instance = ?`,
			bag, instance)
	}
}

// TestReportPairsRequestedWithResolvedPerStep is the defect, fixed: one read of
// one run names the drifted step, and names only it.
func TestReportPairsRequestedWithResolvedPerStep(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	steps := runSteps(t, conn, run.ID)
	if len(steps) < 3 {
		t.Fatalf("the fixture expanded %d steps; this case needs three", len(steps))
	}
	// Two steps served the tier they were dispatched at, one did not — RUN-51's
	// shape, where `effort_resolved` held a `low` that `effort_requested` never
	// showed.
	setStepBags(t, conn, map[string]string{
		steps[0].Instance: `{"effort_requested":"high","effort_resolved":"high"}`,
		steps[1].Instance: `{"effort_requested":"high","effort_resolved":"low"}`,
		steps[2].Instance: `{"effort_requested":"high","effort_resolved":"high"}`,
	})

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	// THE ROLLUP ALONE CANNOT DO IT — the half of the document that existed
	// before. It publishes the anomaly (a value under `effort_resolved` that
	// `effort_requested` never took) and carries no step identity anywhere, so
	// the reader who sees it has nowhere to go but `step show`.
	for _, key := range report.Metadata {
		for _, v := range key.Values {
			for _, s := range steps {
				if v.Value == s.Instance {
					t.Fatalf("the rollup names a step (%q); this test's premise "+
						"is that it cannot", v.Value)
				}
			}
		}
	}

	drifted := tierAudit(report, "effort_requested", "effort_resolved")
	if len(drifted) != 1 || drifted[0] != steps[1].Instance {
		t.Fatalf("one read of the report found drift on %v, want exactly [%s] — "+
			"the pairing the rollup collapses is what makes a retro an "+
			"N-step-show sweep (DKT-868)", drifted, steps[1].Instance)
	}

	// And the row that names it carries what a reader needs to act: the step
	// id, the issue, and the status the step ended in.
	var row StepAttempt
	for _, a := range report.Attempts {
		if a.Instance == steps[1].Instance {
			row = a
		}
	}
	if row.Step == "" || row.Status == "" {
		t.Errorf("the drifted row is %+v; a per-step fact with no step id or "+
			"status is not aimable", row)
	}
}

// TestFailedStepCarriesItsDispatchBag is the consequence the issue calls out as
// invisible entirely: drift concentrated in FAILURES.
//
// The write half already exists — `step claim --metadata` (DKT-592) lands the
// dispatcher's bag before the work runs, and no failure path touches the
// column. What was missing was a reader that kept the half-bag attached to the
// status explaining why it is a half: in the rollup, a step that recorded a
// request and never a resolution is indistinguishable from a completed step
// whose two keys happened to agree.
func TestFailedStepCarriesItsDispatchBag(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{
		Owner: "worker", NowMS: nowMS,
		Metadata: `{"effort_requested":"high"}`,
	})
	testsupport.Must(t, err, "claim: %v", err)

	// The executor dies before it can report what it resolved to.
	testsupport.Must(t, e.FailStep(conn, id, claim.Token, "crashed", "", nowMS+1),
		"fail: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS+2)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	var row StepAttempt
	for _, a := range report.Attempts {
		if a.Instance == "implement@0" {
			row = a
		}
	}
	if row.Metadata["effort_requested"] != "high" {
		t.Fatalf("the failed step's row carries metadata %v; the dispatcher's "+
			"claim-time bag must survive a failure (DKT-592) and reach the "+
			"report (DKT-868)", row.Metadata)
	}
	if _, ok := row.Metadata["effort_resolved"]; ok {
		t.Fatalf("the fixture's failed step somehow reported a resolution: %v",
			row.Metadata)
	}
	// The status is what makes the missing half readable as an absence rather
	// than as agreement.
	if row.Status == db.StepDone {
		t.Errorf("the row reports %q; a half-bag on a step that did not "+
			"complete must be attributable to the failure", row.Status)
	}
}

// TestUnescalatedRunReportsAsBefore is the regression half: a run where every
// step served the tier it was asked for is unchanged in every way that was ever
// published.
func TestUnescalatedRunReportsAsBefore(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	steps := runSteps(t, conn, run.ID)
	if len(steps) < 2 {
		t.Fatalf("the fixture expanded %d steps; this case needs two", len(steps))
	}
	for _, s := range steps[:2] {
		execSQL(t, conn, `UPDATE steps SET metadata = ? WHERE id = ?`,
			`{"effort_requested":"high","effort_resolved":"high"}`, s.ID)
	}

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	// The rollup is byte-for-byte what it always was.
	want := []db.MetadataKeyRollup{
		{Key: "effort_requested", Values: []db.MetadataValueCount{
			{Value: "high", Count: 2}}},
		{Key: "effort_resolved", Values: []db.MetadataValueCount{
			{Value: "high", Count: 2}}},
	}
	if !reflect.DeepEqual(report.Metadata, want) {
		t.Errorf("rollup = %+v, want %+v", report.Metadata, want)
	}
	if drifted := tierAudit(report, "effort_requested", "effort_resolved"); len(drifted) > 0 {
		t.Errorf("an unescalated run reports drift on %v", drifted)
	}
	// A step that carried no bag carries no key on the wire either — nil, so
	// `omitempty` elides it exactly as before. A `{}` on every step of every
	// report in the store would be this change leaking into runs it has nothing
	// to say about.
	for _, a := range report.Attempts {
		if a.Instance == steps[0].Instance || a.Instance == steps[1].Instance {
			continue
		}
		if a.Metadata != nil || a.MetadataUnreadable {
			t.Errorf("%s reports metadata %v (unreadable=%v) and never had a bag",
				a.Instance, a.Metadata, a.MetadataUnreadable)
		}
	}
}

// TestStepMetadataIsVerbatimAndUninterpreted is R7 held through the new
// section: two unrelated vocabularies arrive identically, because core does not
// know either of them, and a non-string value is neither coerced nor dropped.
func TestStepMetadataIsVerbatimAndUninterpreted(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	steps := runSteps(t, conn, run.ID)
	if len(steps) < 2 {
		t.Fatalf("the fixture expanded %d steps; this case needs two", len(steps))
	}
	setStepBags(t, conn, map[string]string{
		steps[0].Instance: `{"desk_requested":"front","desk_resolved":"back"}`,
		steps[1].Instance: `{"sirens":3,"nested":{"a":1}}`,
	})

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	// A vocabulary nobody would use pairs exactly as the corpus's own does.
	if drifted := tierAudit(report, "desk_requested", "desk_resolved"); len(drifted) != 1 {
		t.Errorf("the audit found %v on a `desk` bag; the report must not be "+
			"able to tell one opaque vocabulary from another", drifted)
	}

	var bag map[string]any
	for _, a := range report.Attempts {
		if a.Instance == steps[1].Instance {
			bag = a.Metadata
		}
	}
	if bag["sirens"] != float64(3) {
		t.Errorf("a numeric value came through as %#v, want the number verbatim",
			bag["sirens"])
	}
	if _, ok := bag["nested"].(map[string]any); !ok {
		t.Errorf("a nested object came through as %#v, want it verbatim",
			bag["nested"])
	}
}

// TestUnreadableStepBagIsNotSilence is R10's tolerance, made legible. A stored
// bag that does not decode must not fail the report — a read verb that refused
// over one odd cell is useless during exactly the run an operator wants to
// inspect — and must not vanish either, because on a PER-STEP row an absent bag
// reads as "the dispatcher recorded nothing", which is the comfortable claim
// and the wrong one.
func TestUnreadableStepBagIsNotSilence(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	steps := runSteps(t, conn, run.ID)
	execSQL(t, conn, `UPDATE steps SET metadata = ? WHERE id = ?`,
		`["front"]`, steps[0].ID)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	for _, a := range report.Attempts {
		if a.Instance != steps[0].Instance {
			continue
		}
		if !a.MetadataUnreadable {
			t.Fatalf("%s stores a non-object bag and its row says nothing about "+
				"it (metadata=%v)", a.Instance, a.Metadata)
		}
		if a.Metadata != nil {
			t.Errorf("%s reports a decoded bag %v from bytes that do not decode",
				a.Instance, a.Metadata)
		}
	}
}
