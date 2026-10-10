package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-2070 — `--limit` cuts LANES, not rows, and the manifest says what it cut.
//
// The example fixture gives every issue an eight-row in-offer closure over five
// stages (implement, four reviews, synthesize, reconcile, verify), so a run over
// two issues offers sixteen rows in stage-major order and any row-prefix cut
// between 1 and 15 lands mid-chain. That is the shape RUN-95 measured in the
// large: the deepest stages are the last rows, so a cap kept each issue's
// shallow rows and dropped its tail, and an issue's chain crossed three to five
// dispatches.

// issueRowCounts counts a manifest's rows per issue, in first-appearance order.
func issueRowCounts(rows []model.StepRow) (order []string, counts map[string]int) {
	counts = make(map[string]int, len(rows))
	for _, r := range rows {
		if counts[r.Issue] == 0 {
			order = append(order, r.Issue)
		}
		counts[r.Issue]++
	}
	return order, counts
}

// TestReadyRowsCutIsLaneComplete is AC1 and AC2. With `--limit N` the cut admits
// an issue's whole in-offer closure the first time one of its rows is reached,
// stops before an issue whose closure would exceed N, and never splits an issue;
// when the first issue's closure alone exceeds N it is admitted whole. The wire
// order stays stage-major throughout, because the result is a SUBSEQUENCE of the
// stage-major input rather than a re-sort.
//
// Pre-fix the cut was `entries[:limit]`, so `--limit 12` over two eight-row
// issues returned issue A's whole chain and issue B's first four rows — B's
// synthesize, reconcile and verify dropped, which is the boundary an issue then
// had to cross a later dispatch to get past.
func TestReadyRowsCutIsLaneComplete(t *testing.T) {
	const closure = 8

	cases := []struct {
		name      string
		issues    int
		limit     int
		wantRows  int
		wantLanes int
	}{
		{
			// The limit falls BETWEEN two closures: 12 admits one whole lane
			// and refuses to start a second it cannot finish. The prefix cut
			// returned 12 rows here, splitting the second issue.
			name: "limit between two closures", issues: 2, limit: 12,
			wantRows: closure, wantLanes: 1,
		},
		{
			// Smaller than the first closure: admitted whole anyway, because
			// half a chain is precisely what this cut exists to prevent. The
			// manifest reports the overrun through Total/Truncated.
			name: "limit smaller than the first closure", issues: 2, limit: 3,
			wantRows: closure, wantLanes: 1,
		},
		{
			// Limit 0 is unchanged: the whole offer, every lane.
			name: "limit 0 is unchanged", issues: 2, limit: 0,
			wantRows: 2 * closure, wantLanes: 2,
		},
		{
			// An exact multiple takes both lanes and nothing is dropped.
			name: "limit equal to two closures", issues: 2, limit: 16,
			wantRows: 2 * closure, wantLanes: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			registerFixture(t, conn)
			ids := make([]int, 0, tc.issues)
			for i := range tc.issues {
				ids = append(ids, createIssue(
					t, conn, "issue "+model.FormatID(i+1), "body", "task", nil))
			}
			run := startRun(t, conn, ids...)
			_, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			m := openDispatch(t, conn, run.ID, tc.limit, nowMS)

			order, counts := issueRowCounts(m.Rows)
			if len(m.Rows) != tc.wantRows || len(order) != tc.wantLanes {
				t.Fatalf("limit %d over %d issues offered %d rows across %d issues %v, "+
					"want %d rows across %d issues — the cut must take whole lanes",
					tc.limit, tc.issues, len(m.Rows), len(order), counts,
					tc.wantRows, tc.wantLanes)
			}
			for _, issue := range order {
				if counts[issue] != closure {
					t.Errorf("issue %s carries %d of its %d in-offer rows — "+
						"the cut split an issue", issue, counts[issue], closure)
				}
			}

			// AC2: stage-major order survives the cut.
			for i := 1; i < len(m.Rows); i++ {
				if m.Rows[i].Stage < m.Rows[i-1].Stage {
					t.Fatalf("row %d is stage %d after a stage %d row — the cut "+
						"must preserve stage-major order", i, m.Rows[i].Stage,
						m.Rows[i-1].Stage)
				}
			}
		})
	}
}

// TestDispatchOpenReportsTruncation is AC3 and AC4: the manifest names the
// pre-cut total, says whether the limit dropped anything, and carries the
// effective per-class `[limits] max` for the classes its rows hold. A relay
// reading only `rows` cannot tell a run whose remaining work fits from one the
// cap is metering out, and it had to infer each class's concurrency from the
// largest same-stage count in the manifest — which a chain-deep manifest with
// few issues per stage under-certifies.
func TestDispatchOpenReportsTruncation(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	m := openDispatch(t, conn, run.ID, 12, nowMS)

	if m.Total != 16 {
		t.Errorf("manifest total = %d, want 16 — the PRE-cut ready count", m.Total)
	}
	if !m.Truncated {
		t.Errorf("manifest truncated = false with %d of %d rows offered",
			len(m.Rows), m.Total)
	}

	// AC4's absence half on this fixture: the example workflow declares no
	// `[limits]` at all, so the manifest must carry no limits map rather than a
	// map of zeros a relay would read as "no concurrency". The presence half
	// needs a workflow that declares one — TestManifestCarriesPerClassLimits.
	if m.Limits != nil {
		t.Errorf("manifest limits = %v over a workflow with no [limits] — "+
			"unbounded must be an absence", m.Limits)
	}

	// Rows are unchanged by the report, so the stored manifest still verifies.
	if _, mismatch, err := NewEngine().VerifyDispatch(conn, run.ID, nowMS); err != nil ||
		mismatch != nil {
		t.Errorf("dispatch verify on a truncated manifest: err=%v mismatch=%+v — "+
			"total/truncated ride beside the rows and must not change a hash",
			err, mismatch)
	}
}

// TestNextSharesTheLaneCompleteCut is AC5. `next --run` and `dispatch open`
// share readyRows, so the lane-complete cut must reach both: a cut applied in
// `dispatch open` alone would make the manifest stop describing the `next`
// answer it freezes (P1), and a relay comparing the two would see rows in one
// and not the other. Asserted by step id at the same limit.
func TestNextSharesTheLaneCompleteCut(t *testing.T) {
	const limit = 12

	conn := mustDB(t)
	registerFixture(t, conn)
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	ready, err := NewEngine().NextSteps(conn, run.ID, limit, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	m := openDispatch(t, conn, run.ID, limit, nowMS)

	if len(ready.Steps) != len(m.Rows) {
		t.Fatalf("next offered %d rows and the manifest %d at limit %d",
			len(ready.Steps), len(m.Rows), limit)
	}
	for i := range m.Rows {
		if ready.Steps[i].Step != m.Rows[i].Step {
			t.Fatalf("row %d: next says %s, the manifest says %s — the cut must "+
				"live in the tail both verbs share", i,
				ready.Steps[i].Step, m.Rows[i].Step)
		}
	}
	if ready.Total != m.Total {
		t.Errorf("next total = %d, manifest total = %d — both report the PRE-cut "+
			"count of the same ready set", ready.Total, m.Total)
	}
}

// TestManifestCarriesPerClassLimits is AC4 against a workflow that actually
// declares `[limits]`: the example fixture declares none, so a manifest over it
// legitimately carries no limits map at all.
func TestManifestCarriesPerClassLimits(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "limits-fixture.toml")
	issue := createIssue(t, conn, "limits fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	m := openDispatch(t, conn, run.ID, 0, nowMS)

	if got, ok := m.Limits["write"]; !ok || got != 1 {
		t.Fatalf("manifest limits = %v, want write=1 from the merged [limits]",
			m.Limits)
	}
	// Every class the rows carry with a finite declared max is present, and the
	// expectation comes from the scheduler's own merged limits rather than from
	// the manifest field under test.
	defs, err := StepDefinitions(conn, run.ID)
	testsupport.Must(t, err, "loading definitions: %v", err)
	sched, err := readySnapshot(conn, run.ID, defs, nowMS)
	testsupport.Must(t, err, "ready snapshot: %v", err)
	for _, r := range m.Rows {
		if r.Class == "" {
			continue
		}
		want := sched.Limit(r.Class).Max
		if want == 0 {
			if _, present := m.Limits[r.Class]; present {
				t.Errorf("manifest names class %q with no declared max — "+
					"unbounded must be an absence, not a zero", r.Class)
			}
			continue
		}
		if m.Limits[r.Class] != want {
			t.Errorf("manifest limit for class %q = %d, want %d",
				r.Class, m.Limits[r.Class], want)
		}
	}
}
