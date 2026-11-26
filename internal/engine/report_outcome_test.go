package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-258 — the report's status column erased what actually happened.
//
// A status is one word over a set of outcomes that need opposite responses.
// `failed-routed` covers a step that was measured and came back bad AND one
// cascade-terminated by an issue-abandon without ever being claimed;
// `skipped` covers a tribunal that never convened AND one whose panel
// deliberated before an operator disposed of it. The engine knows the
// difference — the report never carried it.

// DKT-403 — the report carried no ISSUE-level terminal ruling.
//
// `run abandon --issue` terminalizes every remaining step of an issue WITHOUT
// touching `routing`, so a step parked with "loop 4 would exceed
// max_fix_loops = 3; `docket step resolve --as fix-round` authorizes one more
// round" went `failed-routed` still carrying that question — and the operator's
// answer lived only in an `issue-abandoned` event no section of the report
// read. RUN-32 rendered two ruled-on gates as open; the next session read the
// report, said the gates were "never resolved" and the run's `done` rollup
// "looks like an engine reporting anomaly", and re-asked both decisions.

// TestReportCarriesTheOperatorsAbandonRuling is the run-level verb's shape —
// the one that leaves the stale park text standing.
func TestReportCarriesTheOperatorsAbandonRuling(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	// Park `implement`, exactly as a fix-loop bound would.
	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q, want %q", got, db.StepWaitingHuman)
	}

	const ruling = "operator selected: Stop the issue, re-plan it later — " +
		"findings preserved as a follow-up"
	_, err = AbandonIssueInRun(conn, run.ID, issue, ruling, nowMS+1)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS+2)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.Issues) != 1 {
		t.Fatalf("report carries %d issue disposition(s), want 1; an operator "+
			"ruling recorded at issue-abandoned appears nowhere in the report",
			len(report.Issues))
	}
	got := report.Issues[0]
	if got.Issue != model.FormatID(issue) {
		t.Errorf("disposition names %q, want %q", got.Issue, model.FormatID(issue))
	}
	if got.Disposition != DispositionAbandoned {
		t.Errorf("disposition = %q, want %q", got.Disposition, DispositionAbandoned)
	}
	if got.Reason != ruling {
		t.Errorf("disposition reason = %q, want the recorded ruling %q",
			got.Reason, ruling)
	}
	// No step decided this one, so none may be named as having.
	if got.By != "" {
		t.Errorf("disposition attributes the abandon to step %q; `run abandon "+
			"--issue` is an operator acting from outside the graph", got.By)
	}

	// The other half of the bug, still true and now answerable: the parked
	// step's own routing was NEVER rewritten, so its row alone still reads as
	// an open question. The disposition is what a reader joins onto it, and
	// the join key has to be present.
	var parked *StepAttempt
	for i, a := range report.Attempts {
		if a.Instance == "implement@0" {
			parked = &report.Attempts[i]
		}
	}
	if parked == nil {
		t.Fatal("implement@0 is missing from the report's attempts")
	}
	if !strings.Contains(parked.Routing, "waiting-human") {
		t.Fatalf("implement@0 routed %q; this test is about a step whose park "+
			"text outlived the park", parked.Routing)
	}
	if parked.Issue != model.FormatID(issue) {
		t.Errorf("implement@0 carries issue %q, want %q — without it a reader "+
			"cannot tell which disposition resolved this step's park",
			parked.Issue, model.FormatID(issue))
	}
}

// TestReportCarriesAResolvedStepsAbandonNote is the routing verb's shape:
// `step resolve --as abandon-issue --note`, where the ruling lands on the
// deciding step's routing and the event payload carries only its instance.
func TestReportCarriesAResolvedStepsAbandonNote(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	const ruling = "not worth another round; re-planning it separately"
	testsupport.Must(t,
		e.ResolveStep(conn, id, ResolveAbandonIssue, ruling, nowMS+1),
		"resolve --as abandon-issue: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS+2)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.Issues) != 1 {
		t.Fatalf("report carries %d issue disposition(s), want 1", len(report.Issues))
	}
	got := report.Issues[0]
	if got.Issue != model.FormatID(issue) {
		t.Errorf("disposition names %q, want %q", got.Issue, model.FormatID(issue))
	}
	if got.Reason != ruling {
		t.Errorf("disposition reason = %q, want %q", got.Reason, ruling)
	}
	// Here a step DID decide it, and saying which is what makes the row
	// followable — the cascade routing on every other step names the same one.
	if got.By != "implement@0" {
		t.Errorf("disposition attributes the abandon to %q, want implement@0",
			got.By)
	}
}

// TestReportHasNoDispositionsWithoutAnAbandon is the falsifier.
//
// A section that appeared on every run would say nothing. The run here ends
// its issue the ordinary way, and the report must carry no disposition at all
// rather than an empty-reasoned one.
func TestReportHasNoDispositionsWithoutAnAbandon(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.Issues) != 0 {
		t.Errorf("a run with no abandon reports %d disposition(s): %+v",
			len(report.Issues), report.Issues)
	}
}

// TestCascadeRoutingNamesTheAbandonedIssue: the reason has to be actionable.
//
// "cascade" alone tells a reader the step was not measured and leaves them to
// work out why. Naming the issue and the step that abandoned it is what turns
// the row into something they can follow.
func TestCascadeRoutingNamesTheAbandonedIssue(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	err = e.ResolveStep(conn, id, ResolveAbandonIssue, "", nowMS+1)
	testsupport.Must(t, err, "resolve: %v", err)

	var routing string
	err = conn.QueryRow(
		`SELECT COALESCE(routing, '') FROM steps
		  WHERE status = ? AND id != ? LIMIT 1`,
		db.StepFailedRouted, id).Scan(&routing)
	testsupport.Must(t, err, "reading a cascaded step: %v", err)

	for _, needle := range []string{"abandon-issue", "cascade", "implement@0"} {
		if !strings.Contains(routing, needle) {
			t.Errorf("the cascade routing %q does not name %q", routing, needle)
		}
	}
}

// DKT-404 — an issue's own surfaces gave no hint that a run had stopped.
//
// The run report now carries the ruling (DKT-403), but a reader holding an
// ISSUE is not holding a run: `abandon-issue` deliberately leaves the tracker
// status alone, so RUN-14's four abandoned issues have sat at `todo` ever
// since, indistinguishable from work nobody has started, and the disposition
// was reachable only through `events list`.

// TestLatestIssueDispositionNamesTheRunThatStopped is the run-level verb's
// shape, keyed by issue instead of by run.
func TestLatestIssueDispositionNamesTheRunThatStopped(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	const ruling = "operator selected: Stop the issue, re-plan it later — " +
		"findings preserved as a follow-up"
	_, err := AbandonIssueInRun(conn, run.ID, issue, ruling, nowMS+1)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	got, err := LatestIssueDisposition(conn, issue)
	testsupport.Must(t, err, "LatestIssueDisposition: %v", err)
	if got == nil {
		t.Fatal("an abandoned issue reports no disposition; its reader is sent " +
			"back to `events list`, which is the whole defect")
	}
	if got.RunID != run.ID {
		t.Errorf("disposition names run %d, want %d", got.RunID, run.ID)
	}
	if got.Disposition != DispositionAbandoned {
		t.Errorf("disposition = %q, want %q", got.Disposition, DispositionAbandoned)
	}
	if got.Reason != ruling {
		t.Errorf("reason = %q, want the recorded ruling %q", got.Reason, ruling)
	}
	// No step decided this one, so none may be named as having.
	if got.By != "" {
		t.Errorf("disposition attributes the abandon to step %q; `run abandon "+
			"--issue` is an operator acting from outside the graph", got.By)
	}
	if got.AtMS != nowMS+1 {
		t.Errorf("recorded at %d, want the event's own %d", got.AtMS, nowMS+1)
	}

	// The other half of the defect, unchanged and now explained: the issue is
	// STILL OPEN at whatever status it had reached. That is deliberate, and it
	// is exactly why the disposition has to be published separately.
	var status string
	err = conn.QueryRow(`SELECT status FROM issues WHERE id = ?`, issue).Scan(&status)
	testsupport.Must(t, err, "reading the issue status: %v", err)
	if model.Status(status) == model.StatusDone {
		t.Fatalf("the abandoned issue went %q; this test is about the frozen "+
			"non-terminal status a reader misreads as still-pending", status)
	}
}

// TestLatestIssueDispositionCarriesAResolvedStepsNote is the routing verb's
// shape, where the note lives on the deciding step rather than in the payload.
func TestLatestIssueDispositionCarriesAResolvedStepsNote(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	const ruling = "not worth another round; re-planning it separately"
	testsupport.Must(t,
		e.ResolveStep(conn, id, ResolveAbandonIssue, ruling, nowMS+1),
		"resolve --as abandon-issue: %v", err)

	got, err := LatestIssueDisposition(conn, issue)
	testsupport.Must(t, err, "LatestIssueDisposition: %v", err)
	if got == nil {
		t.Fatal("a step-abandoned issue reports no disposition")
	}
	if got.Reason != ruling {
		t.Errorf("reason = %q, want %q — the routing path records it on the "+
			"deciding step, not in the event payload", got.Reason, ruling)
	}
	if got.By != "implement@0" {
		t.Errorf("disposition attributes the abandon to %q, want implement@0", got.By)
	}
}

// TestLatestIssueDispositionIsKeyedByIssueNotRecency is the falsifier for the
// query's shape.
//
// "The latest abandonment" is not "the latest abandonment ANYWHERE": RUN-14
// abandoned four issues whose replacements ran two runs later, so a reader of
// any one of them must get RUN-14's ruling and not whatever the newest event
// in the store happens to say.
func TestLatestIssueDispositionIsKeyedByIssueNotRecency(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	const ruling = "pin drift; re-planned as a replacement issue"
	_, err := AbandonIssueInRun(conn, run.ID, issue, ruling, nowMS+1)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	// A LATER run abandons a DIFFERENT issue. Written directly because what is
	// under test is the read, and driving a second run to the same state would
	// test activation instead.
	other := createIssue(t, conn, "another thing", "a body", "task", nil)
	later, err := db.InsertRun(conn, 0, "the next run", 0, nowMS+2)
	testsupport.Must(t, err, "InsertRun: %v", err)
	execSQL(t, conn,
		`INSERT INTO events (at_ms, kind, run_id, issue_id, data)
		 VALUES (?, ?, ?, ?, ?)`,
		nowMS+3, EventIssueAbandoned, later.ID, other,
		`{"issue":"DKT-2","reason":"a different ruling entirely"}`)

	got, err := LatestIssueDisposition(conn, issue)
	testsupport.Must(t, err, "LatestIssueDisposition: %v", err)
	if got == nil {
		t.Fatal("the abandoned issue reports no disposition")
	}
	if got.RunID != run.ID || got.Reason != ruling {
		t.Errorf("disposition = run %d, reason %q; want run %d, reason %q — the "+
			"read is keyed by ISSUE, not by which event landed last",
			got.RunID, got.Reason, run.ID, ruling)
	}
}

// TestLatestIssueDispositionIsNilWithoutAnAbandon is the other falsifier: a
// line that appeared on every issue would say nothing.
func TestLatestIssueDispositionIsNilWithoutAnAbandon(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)

	got, err := LatestIssueDisposition(conn, issue)
	testsupport.Must(t, err, "LatestIssueDisposition: %v", err)
	if got != nil {
		t.Errorf("an issue no run abandoned reports a disposition: %+v", got)
	}
}
