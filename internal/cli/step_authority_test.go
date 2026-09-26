package cli

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-1899: a resolution says under WHAT AUTHORITY it was made, not only who
// typed it. The conductor policy distinguishes an operator's own decision, a
// standing authorization being applied, and the conductor acting on its own
// reproduction — three cases that were tellable only by reading a free-text
// note, so no surface could count them.
//
// The value is REQUIRED and CLOSED at the CLI, because a default would make
// the commonest case indistinguishable from an unanswered one, which is the
// state this exists to end.

// authorityStepCmd is a ruling verb's command with both authority flags
// registered, as the real commands carry them.
func authorityStepCmd(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("note", "", "")
	cmd.Flags().String("value", "", "")
	cmd.Flags().String("as", "", "")
	cmd.Flags().Bool("batch", false, "")
	cmd.Flags().Bool("drop-interposed", false, "")
	cmd.Flags().String("worktree", "", "")
	// Registered UNANSWERED, unlike the shared helpers: these cases are about
	// what happens when the question goes unanswered.
	addAuthorityFlags(cmd)
	return cmd
}

// authorityRunCmd is a lifecycle verb's command with the flags likewise
// registered but unanswered.
func authorityRunCmd(conn *sql.DB, name string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Use = name
	cmd.Flags().String("reason", "", "")
	addAuthorityFlags(cmd)
	return cmd
}

// setAuthorityFlags applies a flag map, failing the test on an unknown flag.
func setAuthorityFlags(t *testing.T, cmd *cobra.Command, flags map[string]string) {
	t.Helper()
	for name, value := range flags {
		testsupport.Must(t, cmd.Flags().Set(name, value), "set --%s: %v", name, nil)
	}
}

// activeRunForAuthority seeds an activated run with its conductor seated, the
// state `run pause` and `run abandon` apply to.
func activeRunForAuthority(t *testing.T, conn *sql.DB) int {
	t.Helper()
	runID, _ := seedRun(t, conn)
	w, _ := bufWriter(true)
	testsupport.Must(t, runActivateWithWriter(t, conn, w, model.FormatRunID(runID)),
		"activate: %v", nil)
	seatConductor(t, conn, runID)
	return runID
}

// pauseMove and abandonMove are the two lifecycle transitions AC1 names. The
// resume edge is deliberately absent: returning a run to work asserts no
// authority over a resolution, and it keeps working without the flag.
var pauseMove = runMove{
	to:   model.RunWaitingHuman,
	from: []model.RunStatus{model.RunActive},
	verb: "Paused",
}

var abandonMove = runMove{
	to:             model.RunAbandoned,
	from:           []model.RunStatus{model.RunPlanning, model.RunActive, model.RunWaitingHuman},
	verb:           "Abandoned",
	requiresReason: true,
}

// invokeUnderAuthority runs one of the five verbs against a fresh fixture with
// the given authority flags, and returns the verb's error.
func invokeUnderAuthority(t *testing.T, verb string, flags map[string]string) error {
	t.Helper()
	conn := newTestDB(t)
	w, _ := bufWriter(true)

	switch verb {
	case "approve", "reject":
		gate := readyGate(t, conn)
		cmd := authorityStepCmd(conn)
		setAuthorityFlags(t, cmd, flags)
		return runDecide(cmd, []string{model.FormatStepID(gate)}, verb == "approve", w)
	case "resolve":
		id := parkedStep(t, conn)
		cmd := authorityStepCmd(conn)
		setAuthorityFlags(t, cmd, flags)
		testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
		return runStepResolve(cmd, []string{model.FormatStepID(id)}, w)
	default:
		runID := activeRunForAuthority(t, conn)
		cmd := authorityRunCmd(conn, verb)
		setAuthorityFlags(t, cmd, flags)
		move := pauseMove
		if verb == "abandon" {
			testsupport.Must(t, cmd.Flags().Set("reason", "done with it"), "set --reason: %v", nil)
			move = abandonMove
		}
		return moveRun(cmd, model.FormatRunID(runID), move, w)
	}
}

// TestResolutionAuthorityIsRequiredAndClosed is AC1: each of the five verbs
// refuses a missing value, an unknown value, and a `standing-grant` with no
// reference — every one a VALIDATION_ERROR, before anything is written.
func TestResolutionAuthorityIsRequiredAndClosed(t *testing.T) {
	// A MISSING value is refused on EVERY verb. No default: an unanswered
	// question must not record itself as the commonest answer.
	for _, verb := range []string{"resolve", "approve", "reject", "pause", "abandon"} {
		t.Run("missing/"+verb, func(t *testing.T) {
			err := invokeUnderAuthority(t, verb, nil)
			if err == nil {
				t.Fatalf("%s was accepted with no --authority", verb)
			}
			if got := codeOf(t, err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q", got, output.ErrValidation)
			}
			if !strings.Contains(err.Error(), "--authority") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
		})
	}

	// `abandon --issue` narrows the disposition to one issue and takes its own
	// path through the CLI. It answers for its authority too, so the flag
	// cannot be evaded by narrowing what is being abandoned.
	t.Run("missing/abandon --issue", func(t *testing.T) {
		conn := newTestDB(t)
		runID := activeRunForAuthority(t, conn)
		var issueID int
		testsupport.Must(t, conn.QueryRow(
			`SELECT issue_id FROM run_issues WHERE run_id = ?`, runID).Scan(&issueID),
			"run issue: %v", nil)

		cmd := authorityRunCmd(conn, "abandon")
		cmd.Flags().String("issue", "", "")
		testsupport.Must(t, cmd.Flags().Set("reason", "mis-routed"), "set --reason: %v", nil)

		err := abandonIssueInRun(cmd, model.FormatRunID(runID), model.FormatID(issueID))
		if err == nil {
			t.Fatal("abandon --issue was accepted with no --authority")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
	})

	// A REFERENCE without a standing grant is refused rather than silently
	// discarded: the other two authorities name no authorization, so a ref
	// supplied beside them is a caller who believes something is being
	// recorded that is not.
	t.Run("stray-ref", func(t *testing.T) {
		err := invokeUnderAuthority(t, "resolve", map[string]string{
			"authority": engine.AuthorityOperator, "authority-ref": "RUN NOTE 54",
		})
		if err == nil {
			t.Fatal("a reference beside --authority operator was accepted")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
		if !strings.Contains(err.Error(), "--authority-ref") {
			t.Errorf("the refusal does not name the flag it is about: %v", err)
		}
	})

	// An UNKNOWN value is refused: the set is closed, so a reader counting
	// authorities never meets a value the policy does not define.
	t.Run("unknown", func(t *testing.T) {
		err := invokeUnderAuthority(t, "resolve", map[string]string{"authority": "chairman"})
		if err == nil {
			t.Fatal("an unknown --authority was accepted")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
	})

	// `standing-grant` WITHOUT a reference is refused: a standing authorization
	// that names nothing is exactly the unattributable note this replaces.
	t.Run("standing-grant-without-reference", func(t *testing.T) {
		err := invokeUnderAuthority(t, "resolve",
			map[string]string{"authority": engine.AuthorityStandingGrant})
		if err == nil {
			t.Fatal("standing-grant was accepted with no --authority-ref")
		}
		if got := codeOf(t, err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q", got, output.ErrValidation)
		}
		if !strings.Contains(err.Error(), "--authority-ref") {
			t.Errorf("the refusal does not name the reference flag: %v", err)
		}
	})

	// The three values are ACCEPTED, so the refusals above close a set rather
	// than blocking the verb.
	for _, authority := range []string{
		engine.AuthorityOperator, engine.AuthorityStandingGrant, engine.AuthorityConductor,
	} {
		t.Run("accepted/"+authority, func(t *testing.T) {
			flags := map[string]string{"authority": authority}
			if authority == engine.AuthorityStandingGrant {
				flags["authority-ref"] = "RUN NOTE 54"
			}
			testsupport.Must(t, invokeUnderAuthority(t, "resolve", flags),
				"resolve under %s: %v", authority, nil)
		})
	}
}

// parkedStepsForAuthorityReport builds ONE run over `n` issues of the
// single-step parking workflow and drives every step to `waiting-human`, so
// the run holds several resolutions to count rather than one.
func parkedStepsForAuthorityReport(t *testing.T, conn *sql.DB, n int) int {
	t.Helper()
	registerForRun(t, conn, `
[pipeline]
name = "parks"
version = 1
[match]
kind = ["task"]
[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"
`)
	run, err := db.InsertRun(conn, 1, "", 0, model.NowMS())
	testsupport.Must(t, err, "starting run: %v", err)
	for i := range n {
		issueID, err := db.CreateIssue(conn, &model.Issue{
			Title: "park me " + strconv.Itoa(i), Description: "a body",
			Status: model.StatusBacklog, Priority: model.PriorityNone,
			Kind: model.IssueKindTask,
		}, nil, nil)
		testsupport.Must(t, err, "creating issue: %v", err)
		testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue: %v", nil)
	}
	activateHolding(t, conn, run.ID, model.NowMS())

	for _, id := range parkedStepIDs(t, conn, run.ID) {
		claim, err := engine.ClaimStep(conn, id,
			engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
		testsupport.Must(t, err, "claim: %v", err)
		testsupport.Must(t,
			engine.NewEngine().FailStep(conn, id, claim.Token, "gave up", "", model.NowMS()),
			"fail: %v", nil)
	}
	return run.ID
}

// parkedStepIDs lists the run's step ids in a stable order.
func parkedStepIDs(t *testing.T, conn *sql.DB, runID int) []int {
	t.Helper()
	rows, err := conn.Query(`SELECT id FROM steps WHERE run_id = ? ORDER BY id`, runID)
	testsupport.Must(t, err, "listing the run's steps: %v", err)
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		testsupport.Must(t, rows.Scan(&id), "scanning a step id: %v", nil)
		ids = append(ids, id)
	}
	testsupport.Must(t, rows.Err(), "listing the run's steps: %v", nil)
	return ids
}

// resolveUnder parks a fresh step in `conn`'s run and resolves it under the
// given authority, so a run accumulates a MIX of resolutions to count.
func resolveUnder(t *testing.T, conn *sql.DB, stepID int, authority, ref string) {
	t.Helper()
	cmd := authorityStepCmd(conn)
	flags := map[string]string{"authority": authority}
	if ref != "" {
		flags["authority-ref"] = ref
	}
	setAuthorityFlags(t, cmd, flags)
	testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
	w, buf := bufWriter(true)
	testsupport.Must(t, runStepResolve(cmd, []string{model.FormatStepID(stepID)}, w),
		"resolve under %s: %s", authority, buf.String())
}

// TestRunReportTalliesAuthority is AC3: `run report --json` counts the run's
// resolutions per authority, which is the question 254 free-text notes could
// not answer.
func TestRunReportTalliesAuthority(t *testing.T) {
	conn := newTestDB(t)
	runID := parkedStepsForAuthorityReport(t, conn, 4)

	// A MIX: two operator decisions, one standing grant, one the conductor
	// made on its own reproduction. Distinct counts, so a tally that collapsed
	// them — or counted every resolution as `operator` — cannot pass.
	steps := parkedStepIDs(t, conn, runID)
	resolveUnder(t, conn, steps[0], engine.AuthorityOperator, "")
	resolveUnder(t, conn, steps[1], engine.AuthorityOperator, "")
	resolveUnder(t, conn, steps[2], engine.AuthorityStandingGrant, "RUN NOTE 54")
	resolveUnder(t, conn, steps[3], engine.AuthorityConductor, "")

	report, err := engine.LoadRunReport(conn, runID, model.NowMS())
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	got := map[string]int{}
	for _, row := range report.Authorities {
		got[row.Authority] = row.Count
	}
	want := map[string]int{
		engine.AuthorityOperator:      2,
		engine.AuthorityStandingGrant: 1,
		engine.AuthorityConductor:     1,
	}
	for authority, count := range want {
		if got[authority] != count {
			t.Errorf("resolutions under %s = %d, want %d — the report must count "+
				"the three cases the conductor policy distinguishes",
				authority, got[authority], count)
		}
	}

	// The rows are in the values' DECLARED order, so two reports of the same
	// run are byte-identical (R9).
	var order []string
	for _, row := range report.Authorities {
		order = append(order, row.Authority)
	}
	wantOrder := []string{
		engine.AuthorityOperator, engine.AuthorityStandingGrant, engine.AuthorityConductor,
	}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("row order = %v, want the declared order %v", order, wantOrder)
	}
}
