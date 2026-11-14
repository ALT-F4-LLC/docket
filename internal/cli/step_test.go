package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// The step refusal matrix AT THE CLI BOUNDARY.
//
// internal/engine and internal/db already test the behavior; what these assert
// is the MAPPING — that each engine or lease failure reaches the operator as
// the right error code. An error taxonomy that degrades to "general error" at
// the boundary is one no script can branch on, and that degradation is
// invisible to a test that only checks the engine returned an error.

// mappingBy is the attribution the mapping cases below rule under; the
// refusal being mapped fires after it, so its value never matters.
var mappingBy = engine.Attribution{Actor: "tester", Cwd: "/repo"}

// mappingUnder is the authority those cases rule under, supplied for the same
// reason and with as little consequence: the refusal being mapped fires after
// it too.
var mappingUnder = engine.Authority{Kind: engine.AuthorityOperator}

// TestStepErrMapping walks the refusal matrix's sentinel errors through
// stepErr and asserts each lands on its specified code.
//
// It is table-driven over the SENTINELS rather than over verb invocations
// because every step verb funnels through this one function: testing the
// funnel once proves the mapping for all of them, and a verb that bypassed it
// would be the bug, which the coverage assertion below catches.
func TestStepErrMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want output.ErrorCode
		row  string
	}{
		{
			name: "unclaimed or wrong token", err: db.ErrNotHolder,
			want: output.ErrAuth, row: "R2/R3",
		},
		{
			name: "correct token, lease expired", err: db.ErrLeaseExpired,
			want: output.ErrStaleLease, row: "R4",
		},
		{
			name: "claim against a live lease", err: db.ErrLeaseHeld,
			want: output.ErrConflict, row: "R5/R6",
		},
		{
			name: "step does not exist", err: db.ErrStepNotFound,
			want: output.ErrNotFound, row: "—",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := stepErr(tc.err, "step STEP-1")
			var cmdError *CmdError
			if !asCmdError(err, &cmdError) {
				t.Fatalf("%s: stepErr returned %T, not a *CmdError — the code "+
					"never reaches the envelope", tc.row, err)
			}
			if cmdError.Code != tc.want {
				t.Errorf("%s: code = %v, want %v", tc.row, cmdError.Code, tc.want)
			}
		})
	}
}

// TestStepErrMapsEngineCodes covers the other half: engine.Error values carry
// their own taxonomy code, and stepErr must honor it rather than flattening
// everything a lease sentinel did not match.
func TestStepErrMapsEngineCodes(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)
	_ = runID

	cases := []struct {
		name string
		call func() error
		want output.ErrorCode
		row  string
	}{
		{
			name: "R8 claim a step that is not ready",
			call: func() error {
				// STEP-2 is the human gate, downstream of an unfinished root.
				_, err := engine.ClaimStep(conn, 2,
					engine.ClaimOptions{Owner: "w", NowMS: model.NowMS()})
				return err
			},
			want: output.ErrConflict, row: "R8",
		},
		{
			name: "unknown step",
			call: func() error {
				_, err := engine.LoadStepView(conn, 9999, model.NowMS())
				return err
			},
			want: output.ErrNotFound, row: "—",
		},
		{
			name: "R10 approve a non-human step",
			call: func() error {
				e := engine.NewEngine()
				return e.DecideStepWith(conn, 1, engine.DecideOptions{
					Approve: true, By: mappingBy, Under: mappingUnder, NowMS: model.NowMS(),
				})
			},
			want: output.ErrValidation, row: "R10",
		},
		{
			name: "R11 resolve an unparked step",
			call: func() error {
				e := engine.NewEngine()
				_, err := e.ResolveStepWith(conn, 1, engine.ResolveOptions{
					As: engine.ResolveSkip, By: mappingBy, Under: mappingUnder, NowMS: model.NowMS(),
				})
				return err
			},
			want: output.ErrValidation, row: "R11",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s: the call succeeded; there is no refusal to map", tc.row)
			}
			mapped := stepErr(err, "step STEP-1")

			var cmdError *CmdError
			if !asCmdError(mapped, &cmdError) {
				t.Fatalf("%s: stepErr returned %T, not a *CmdError", tc.row, mapped)
			}
			if cmdError.Code != tc.want {
				t.Errorf("%s: code = %v, want %v — an engine failure that reaches "+
					"the operator as the wrong code is one no script can branch on",
					tc.row, cmdError.Code, tc.want)
			}
		})
	}
}

// TestStepErrDoesNotSwallowUnknownErrors pins the default: something with no
// mapping must still reach the envelope as an error, not as success.
func TestStepErrDoesNotSwallowUnknownErrors(t *testing.T) {
	err := stepErr(sql.ErrTxDone, "step STEP-1")
	if err == nil {
		t.Fatal("an unmapped error vanished")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) {
		t.Fatalf("stepErr returned %T, not a *CmdError", err)
	}
	if cmdError.Code != output.ErrGeneral {
		t.Errorf("code = %v, want GENERAL_ERROR for an unmapped cause", cmdError.Code)
	}
}

// TestEveryStepVerbIsRegistered guards the specified step surface. A verb
// missing from the tree is a documented capability that does not exist, and
// SKILL.md's table would be describing something no operator can run.
func TestEveryStepVerbIsRegistered(t *testing.T) {
	want := []string{
		"claim", "heartbeat", "complete", "fail",
		"approve", "reject", "resolve",
		// `reap` (DKT-83): the forced-liveness verb — a relay that established
		// its executor is dead clears the claim instead of waiting out the TTL.
		"reap",
		// `hold` (DKT-3287/DKT-3305): the conductor parks one ready step for
		// the operator without abandoning the rest of its issue.
		"hold",
		"show", "context", "render",
		// The OUTPUT side of the read surface. `context` re-emits
		// what a step consumed; these re-emit what it produced, which nothing
		// did before — an action step's verdict was reachable only through
		// raw sqlite.
		"artifacts", "artifact",
		// `gates` (DKT-104): the recorded gate_results rows — verdict, exit,
		// duration, output, reason — which were stored complete but had no
		// read surface at all.
		"gates",
		// `list` (DKT-54): the run-scoped inventory — id, instance, effective
		// status, cost — that a budget projection reads. Step ids are one
		// store-wide sequence, so nothing else can enumerate a run.
		"list",
		// `annotate` (DKT-35): the post-completion channel for facts that
		// become true only after a step records — the durable commit id an
		// integration mints, most of all. Opaque KV merged onto the finished
		// step's metadata, event-logged.
		"annotate",
		// `integrate`: the engine's one git write — cherry-pick a finished
		// writer's recorded chain onto the shared branch with -x, then annotate
		// the step with the resulting HEAD, as one operation that stops on a
		// failed pick instead of the hand sequence that did not.
		"integrate",
		// `pregate`: the detached pre-gate run the
		// engine launches as a re-execution of this binary when a step's
		// target is first recorded. HIDDEN — it is the engine's argv, not an
		// operator's verb — but it is surface this binary answers to, so it is
		// counted here rather than exempted by its flag.
		"pregate",
		// `resume`: the detached completion saga `step record`
		// launches so the gates run outside the recording turn. HIDDEN for
		// the same reason as `pregate`.
		"resume",
	}

	have := make(map[string]bool)
	for _, sub := range stepCmd.Commands() {
		have[sub.Name()] = true
	}

	for _, verb := range want {
		if !have[verb] {
			t.Errorf("`docket step %s` is not registered, but the step surface requires it", verb)
		}
	}
	if len(have) != len(want) {
		t.Errorf("the step surface has %d verbs, want %d — an extra "+
			"verb is surface nobody documented", len(have), len(want))
	}
}

// TestGuardVerbsAreRegistered is the same guard for `docket guard`, and it is
// now the COMPLETE guard surface.
//
// This test once also pinned that `record`/`spawn` were ABSENT: guard verbs
// land with their underlying features, and shipping those two early would
// have frozen a shape their stage was meant to define. That guard did its job;
// the two verbs landed with the dispatch mechanics they are predicates over,
// and the absence assertion is replaced by the presence one.
//
// The exact-count check is what keeps this a boundary rather than a checklist:
// the surface names FOUR guards, so a fifth is surface nobody documented.
func TestGuardVerbsAreRegistered(t *testing.T) {
	want := []string{"stop", "gate", "record", "spawn"}

	have := make(map[string]bool)
	for _, sub := range guardCmd.Commands() {
		have[sub.Name()] = true
	}

	for _, verb := range want {
		if !have[verb] {
			t.Errorf("`docket guard %s` is not registered, but the guard surface requires it", verb)
		}
	}
	if len(have) != len(want) {
		t.Errorf("the guard surface has %d verbs, want %d — an extra "+
			"guard is surface nobody documented", len(have), len(want))
	}
}

// TestNoTokenFlagOnStepVerbs extends TestNoTokenFlag to the step surface.
//
// Tokens pass via env or stdin, NEVER argv: argv is world-readable through
// `ps` on a shared host, so a --token flag would defeat the capability model
// at the transport layer regardless of how carefully the rest is built.
func TestNoTokenFlagOnStepVerbs(t *testing.T) {
	for _, sub := range stepCmd.Commands() {
		if sub.Flags().Lookup("token") != nil {
			t.Errorf("`docket step %s` has a --token flag; argv is world-readable "+
				"through `ps` and tokens must pass via env or stdin", sub.Name())
		}
	}
}

// TestStepRecordIsCompleteAlias is DKT-107: a worktree-isolated agent's shell
// guard misparses the bare word "complete" as the `complete` builtin and
// refuses the whole command line before docket ever sees it. `step record` is
// an identical alias that avoids the collision — this pins that cobra
// resolves it to the SAME command object as `step complete`, not a
// look-alike with its own drifted flags or behavior.
func TestStepRecordIsCompleteAlias(t *testing.T) {
	found, _, err := stepCmd.Find([]string{"record", "STEP-1"})
	if err != nil {
		t.Fatalf("`docket step record` did not resolve: %v", err)
	}
	if found != stepCompleteCmd {
		t.Errorf("`docket step record` resolved to %p, want the same command "+
			"object as `docket step complete` (%p)", found, stepCompleteCmd)
	}
}

// DKT-982 — `step record`'s stdout when a completion gate failed.
//
// The saga runs the completion gates synchronously and parks the step when one
// fails, and the line the recording verb printed was `✔ Completed STEP-N
// (waiting-human)`: no gate, no verdict, a success glyph on a park. RUN-63's
// executor read that line, answered "Record succeeded", and reported the wave
// green over a real cargo-fmt failure. These drive emitRecordState — the whole
// of what the RunE does after the verb returns — over recorded gate rows.

// emitRecorded reads the step's row and failed gates as verb.StepComplete
// returns them after an inline saga, and prints them with emitRecordState.
func emitRecorded(t *testing.T, w *output.Writer, conn *sql.DB, stepID int) {
	t.Helper()
	failed, err := engine.FailedGates(conn, stepID)
	testsupport.Must(t, err, "FailedGates: %v", err)
	view, err := engine.LoadStepView(conn, stepID, model.NowMS())
	testsupport.Must(t, err, "LoadStepView: %v", err)
	emitRecordState(w, view.Row, failed, nil)
}

// recordFixture activates a run and returns the connection and the executor
// step every case here records against.
func recordFixture(t *testing.T, conn *sql.DB) int {
	t.Helper()
	activatedRunForNext(t, conn)
	var id int
	err := conn.QueryRow(`SELECT id FROM steps WHERE instance = 'first@0'`).Scan(&id)
	testsupport.Must(t, err, "finding the executor step: %v", err)
	return id
}

// seedGateRows records gate results against a step and sets the status the
// routing would have left it in — the state the printer reads.
func seedGateRows(
	t *testing.T, conn *sql.DB, stepID int, status string, rows ...db.GateResultRow,
) {
	t.Helper()
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		r.RunID, r.StepID, r.CreatedAtMS = step.RunID, stepID, model.NowMS()
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "inserting a gate row: %v", err)
	}
	testsupport.Must(t,
		db.SetStepStatusTx(tx, stepID, status, model.NowMS(), model.NowMS()),
		"setting the step status: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)
}

// colorfulTerminal makes render.ColorsEnabled() true, which is the condition
// under which a glyph is printed at all — the RUN-63 terminal's condition. The
// variable must be genuinely ABSENT: ColorsEnabled uses LookupEnv, so
// NO_COLOR="" would still disable colors.
func colorfulTerminal(t *testing.T) {
	t.Helper()
	if prev, ok := os.LookupEnv("NO_COLOR"); ok {
		testsupport.Must(t, os.Unsetenv("NO_COLOR"), "unsetting NO_COLOR: %v", nil)
		t.Cleanup(func() { _ = os.Setenv("NO_COLOR", prev) })
	}
	t.Setenv("TERM", "xterm-256color")
	if !render.ColorsEnabled() {
		t.Fatal("premise: colors are disabled, so neither glyph would be printed")
	}
}

// TestStepRecordNamesTheFailedGateAndItsExit is acceptance criteria 1 and 2:
// the failed gate's NAME and EXIT CODE are on stdout, and the park is not
// wearing a checkmark.
func TestStepRecordNamesTheFailedGateAndItsExit(t *testing.T) {
	colorfulTerminal(t)

	conn := newTestDB(t)
	stepID := recordFixture(t, conn)
	exit := 2
	seedGateRows(t, conn, stepID, db.StepWaitingHuman,
		db.GateResultRow{Gate: "build", Verdict: db.GateVerdictPass, Exit: intPtr(0)},
		db.GateResultRow{Gate: "tests", Verdict: db.GateVerdictPass, Exit: intPtr(0)},
		db.GateResultRow{Gate: "self-hygiene", Verdict: db.GateVerdictFail, Exit: &exit})

	w, buf := bufWriter(false)
	emitRecorded(t, w, conn, stepID)

	out := buf.String()
	for _, want := range []string{
		"self-hygiene failed (exit 2)",
		"parked waiting-human",
		model.FormatStepID(stepID),
		"✘",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to carry %q", out, want)
		}
	}
	if strings.Contains(out, "✔") {
		t.Errorf("stdout = %q — the park is behind a success glyph, which is the "+
			"misread DKT-982 was filed for", out)
	}
	// The gates that PASSED are not on the line: three of them named at every
	// record is how the one that matters gets skimmed past.
	if strings.Contains(out, "build") || strings.Contains(out, "tests") {
		t.Errorf("stdout = %q, want only the gate that failed", out)
	}
}

// TestStepRecordNamesEveryFailedGate covers the plural case and the gate that
// never ran: `unmatched` has no exit code, and printing `exit 0` for a process
// that did not exist would read as a pass.
func TestStepRecordNamesEveryFailedGate(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	conn := newTestDB(t)
	stepID := recordFixture(t, conn)
	exit := 1
	seedGateRows(t, conn, stepID, db.StepWaitingHuman,
		db.GateResultRow{Gate: "secret-scan", Verdict: db.GateVerdictUnmatched,
			Reason: "no trust entry matched"},
		db.GateResultRow{Gate: "build", Verdict: db.GateVerdictFail, Exit: &exit})

	w, buf := bufWriter(false)
	emitRecorded(t, w, conn, stepID)

	out := buf.String()
	want := "gates build failed (exit 1), secret-scan unmatched (no exit); " +
		model.FormatStepID(stepID) + " parked waiting-human — `docket step gates " +
		model.FormatStepID(stepID) + "` has the captured output\n"
	if out != want {
		t.Errorf("stdout  = %q\nwant      %q", out, want)
	}
}

// TestStepRecordJSONCarriesTheFailedGates is the machine channel: the envelope
// stays a SUCCESS envelope — the recording did succeed — and the failed gates
// ride beside the step row, so a --json consumer learns the same fact without a
// second command.
func TestStepRecordJSONCarriesTheFailedGates(t *testing.T) {
	conn := newTestDB(t)
	stepID := recordFixture(t, conn)
	exit := 2
	seedGateRows(t, conn, stepID, db.StepWaitingHuman,
		db.GateResultRow{Gate: "self-hygiene", Verdict: db.GateVerdictFail, Exit: &exit})

	w, buf := bufWriter(true)
	emitRecorded(t, w, conn, stepID)

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Step        string              `json:"step"`
			Status      string              `json:"status"`
			FailedGates []engine.FailedGate `json:"failed_gates"`
		} `json:"data"`
		Message string `json:"message"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "unmarshal: %v", nil)

	if !envelope.OK {
		t.Error("ok = false; the recording succeeded — its subject did not")
	}
	if envelope.Data.Step != model.FormatStepID(stepID) {
		t.Errorf("data.step = %q, want the row the completion has always emitted",
			envelope.Data.Step)
	}
	if envelope.Data.Status != db.StepWaitingHuman {
		t.Errorf("data.status = %q, want %q", envelope.Data.Status, db.StepWaitingHuman)
	}
	if len(envelope.Data.FailedGates) != 1 {
		t.Fatalf("data.failed_gates = %v, want the one gate that failed",
			envelope.Data.FailedGates)
	}
	got := envelope.Data.FailedGates[0]
	if got.Gate != "self-hygiene" || got.Exit == nil || *got.Exit != 2 {
		t.Errorf("data.failed_gates[0] = %+v, want self-hygiene at exit 2", got)
	}
	if !strings.Contains(envelope.Message, "self-hygiene failed (exit 2)") {
		t.Errorf("message = %q, want the failed gate named", envelope.Message)
	}
}

// TestStepRecordKeepsItsSuccessLineWhenGatesPass is acceptance criterion 3: a
// clean record is byte-for-byte the line it has always printed, checkmark
// included. Everything above is a NEW branch, and this is the assertion that
// keeps it one.
func TestStepRecordKeepsItsSuccessLineWhenGatesPass(t *testing.T) {
	colorfulTerminal(t)

	conn := newTestDB(t)
	stepID := recordFixture(t, conn)
	seedGateRows(t, conn, stepID, db.StepDone,
		db.GateResultRow{Gate: "build", Verdict: db.GateVerdictPass, Exit: intPtr(0)},
		// A PRE-gate that failed is an input to the step, not a judgment of it
		// — it routed nothing, so it must not turn a clean record into a
		// reported failure.
		db.GateResultRow{Gate: "pre-scan", Verdict: db.GateVerdictFail,
			Exit: intPtr(3), Pre: true})

	w, buf := bufWriter(false)
	emitRecorded(t, w, conn, stepID)

	want := "✔ Completed " + model.FormatStepID(stepID) + " (done)\n"
	if buf.String() != want {
		t.Errorf("stdout = %q, want %q", buf.String(), want)
	}
}

func intPtr(n int) *int { return &n }

// The step lease commands resolve their project through internal/verb, so the
// root hook must skip its own resolution for them.
func TestStepLeaseCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{
		stepClaimCmd, stepHeartbeatCmd, stepCompleteCmd, stepFailCmd,
	} {
		if !verbOwnsProject(cmd) {
			t.Errorf("step %s lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// The step ruling commands resolve their project through internal/verb too.
func TestStepRulingCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{
		stepReapCmd, stepHoldCmd, stepApproveCmd, stepRejectCmd, stepResolveCmd,
	} {
		if !verbOwnsProject(cmd) {
			t.Errorf("step %s lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// TestStepRulingCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before each step ruling
// command resolved its own project.
func TestStepRulingCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}
	cases := map[string]func(t *testing.T, conn *sql.DB, cfg *config.Config){
		"step hold": func(t *testing.T, conn *sql.DB, cfg *config.Config) {
			activatedRunForNext(t, conn)
			testsupport.Must(t, stepHoldCmd.Flags().Set("reason", "for the operator"), "set --reason")
			runLeaseCommand(t, conn, cfg, stepHoldCmd, model.FormatStepID(stepIDNamed(t, conn, "first@0")))
		},
		"step reap": func(t *testing.T, conn *sql.DB, cfg *config.Config) {
			activatedRunForNext(t, conn)
			first := stepIDNamed(t, conn, "first@0")
			_, err := engine.ClaimStep(conn, first, engine.ClaimOptions{Owner: "doomed", NowMS: model.NowMS()})
			testsupport.Must(t, err, "claim: %v", err)
			testsupport.Must(t, stepReapCmd.Flags().Set("reason", "spawn died"), "set --reason")
			runLeaseCommand(t, conn, cfg, stepReapCmd, model.FormatStepID(first))
		},
		"step approve": func(t *testing.T, conn *sql.DB, cfg *config.Config) {
			gate := readyGate(t, conn)
			testsupport.Must(t, stepApproveCmd.Flags().Set("authority", engine.AuthorityOperator), "set --authority")
			runLeaseCommand(t, conn, cfg, stepApproveCmd, model.FormatStepID(gate))
		},
		"step reject": func(t *testing.T, conn *sql.DB, cfg *config.Config) {
			gate := readyGate(t, conn)
			testsupport.Must(t, stepRejectCmd.Flags().Set("authority", engine.AuthorityOperator), "set --authority")
			runLeaseCommand(t, conn, cfg, stepRejectCmd, model.FormatStepID(gate))
		},
		"step resolve": func(t *testing.T, conn *sql.DB, cfg *config.Config) {
			id := parkedStep(t, conn)
			testsupport.Must(t, stepResolveCmd.Flags().Set("as", engine.ResolveSkip), "set --as")
			testsupport.Must(t, stepResolveCmd.Flags().Set("authority", engine.AuthorityOperator), "set --authority")
			runLeaseCommand(t, conn, cfg, stepResolveCmd, model.FormatStepID(id))
		},
	}
	for verb, run := range cases {
		t.Run(verb, func(t *testing.T) {
			conn := newTestDB(t)
			identity := "/repos/" + strings.ReplaceAll(verb, " ", "-")
			run(t, conn, anchored(identity))

			got := registrationEvents(t, conn)
			if len(got) != 1 || got[identity] != verb {
				t.Errorf("registration events = %v, want %s registered by %q", got, identity, verb)
			}
		})
	}
}

// TestStepRulingCommandsReportABadRequestBeforeAnUnreadableCwd: a malformed
// STEP-N, or a resolve with no --as, is reported as the request's
// VALIDATION_ERROR even when the working directory cannot be read.
func TestStepRulingCommandsReportABadRequestBeforeAnUnreadableCwd(t *testing.T) {
	cases := map[string]struct {
		run  func(conn *sql.DB) error
		want string
	}{
		"step hold": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			return runStepHold(reapCmdWithDB(conn), []string{"ISSUE-1"}, w)
		}, want: "invalid step ID"},
		"step reap": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			return runStepReap(reapCmdWithDB(conn), []string{"ISSUE-1"}, w)
		}, want: "invalid step ID"},
		"step approve": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			return runDecide(decideCmdWithDB(conn), []string{"ISSUE-1"}, true, w)
		}, want: "invalid step ID"},
		"step reject": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			return runDecide(decideCmdWithDB(conn), []string{"ISSUE-1"}, false, w)
		}, want: "invalid step ID"},
		"step resolve": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			cmd := resolveCmdWithDB(conn)
			_ = cmd.Flags().Set("as", engine.ResolveSkip)
			return runStepResolve(cmd, []string{"ISSUE-1"}, w)
		}, want: "invalid step ID"},
		"step resolve without --as": {run: func(conn *sql.DB) error {
			w, _ := bufWriter(true)
			return runStepResolve(resolveCmdWithDB(conn), []string{"STEP-1"}, w)
		}, want: "--as is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			conn := newTestDB(t)
			stubGetwdFailure(t)

			err := c.run(conn)
			assertCmdCode(t, err, output.ErrValidation, name)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: error = %v, want it to name %q", name, err, c.want)
			}
		})
	}
}

// TestStepRulingCommandsReadTheCapabilityFromStdin: with DOCKET_TOKEN empty,
// each ruling command on a bound run takes the conductor capability piped on
// stdin and rules.
func TestStepRulingCommandsReadTheCapabilityFromStdin(t *testing.T) {
	cases := map[string]func(t *testing.T, conn *sql.DB) (run func() error, stepID int, event string){
		"step hold": func(t *testing.T, conn *sql.DB) (func() error, int, string) {
			activatedRunForNext(t, conn)
			first := stepIDNamed(t, conn, "first@0")
			return func() error {
				cmd := reapCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("reason", "for the operator"), "set --reason")
				w, _ := bufWriter(true)
				return runStepHold(cmd, []string{model.FormatStepID(first)}, w)
			}, first, engine.EventStepHeld
		},
		"step reap": func(t *testing.T, conn *sql.DB) (func() error, int, string) {
			activatedRunForNext(t, conn)
			first := stepIDNamed(t, conn, "first@0")
			_, err := engine.ClaimStep(conn, first, engine.ClaimOptions{Owner: "doomed", NowMS: model.NowMS()})
			testsupport.Must(t, err, "claim: %v", err)
			return func() error {
				cmd := reapCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("reason", "spawn died"), "set --reason")
				w, _ := bufWriter(true)
				return runStepReap(cmd, []string{model.FormatStepID(first)}, w)
			}, first, engine.EventLeaseReaped
		},
		"step approve": func(t *testing.T, conn *sql.DB) (func() error, int, string) {
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, true, w)
			}, gate, engine.EventStepApproved
		},
		"step reject": func(t *testing.T, conn *sql.DB) (func() error, int, string) {
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, false, w)
			}, gate, engine.EventStepRejected
		},
		"step resolve": func(t *testing.T, conn *sql.DB) (func() error, int, string) {
			id := parkedStep(t, conn)
			return func() error {
				cmd := resolveCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as")
				w, _ := bufWriter(true)
				return runStepResolve(cmd, []string{model.FormatStepID(id)}, w)
			}, id, engine.EventStepResolved
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			conn := newTestDB(t)
			run, id, event := setup(t, conn)
			token := envToken(t)
			t.Setenv(TokenEnvVar, "")
			withStdin(t, token+"\n")

			testsupport.Must(t, run(), "%s with the capability on stdin: %v", name, nil)
			if rulingEvent(t, conn, id, event) == nil {
				t.Errorf("%s recorded no %s event", name, event)
			}
		})
	}
}

// TestStepClaimCommandWritesTheMetadataFlag drives the REAL `step claim` RunE
// over its REAL flag set and reads the step back out of the store (DKT-592).
//
// It exists because this is the exact join DKT-68 lost: a flag that parses, an
// option field that is documented as merged, and nothing in between reading
// it. Everything either side is pinned in internal/engine, so a RunE that
// looked up a flag name nobody registers — or registered one nobody reads —
// would drop every dispatcher's bag with the whole engine suite green.
//
// The flag set comes from `stepClaimCmd` itself rather than being re-declared
// here, so the registration is part of what is under test.
func TestStepClaimCommandWritesTheMetadataFlag(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	_, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)

	var stepID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND step_name = 'first'`, runID,
	).Scan(&stepID)
	testsupport.Must(t, err, "reading the claimable step: %v", err)

	cmd := cmdWithDB(conn)
	cmd.Flags().AddFlagSet(stepClaimCmd.Flags())
	// AddFlagSet shares the flag VALUES with the package-level command, so the
	// two set here are put back before any later test reads them.
	t.Cleanup(func() {
		_ = stepClaimCmd.Flags().Set("owner", "")
		_ = stepClaimCmd.Flags().Set("metadata", "")
	})
	testsupport.Must(t, cmd.Flags().Set("owner", "worker"), "setting --owner: %v", err)
	testsupport.Must(t, cmd.Flags().Set("metadata", `{"tier_requested":"a"}`),
		"setting --metadata: %v", err)

	err = stepClaimCmd.RunE(cmd, []string{model.FormatStepID(stepID)})
	testsupport.Must(t, err, "step claim --metadata: %v", err)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if !strings.Contains(step.Metadata, `"tier_requested":"a"`) {
		t.Errorf("metadata = %q, want the claim's bag — the flag was parsed and dropped",
			step.Metadata)
	}
}

// TestStepClaimRefusesItsFlagsBeforeTheRecoverySecret keeps the refusal a
// claim invalid both ways reports: the flag, as before the command called
// internal/verb, not the group-readable secret file on stdin.
func TestStepClaimRefusesItsFlagsBeforeTheRecoverySecret(t *testing.T) {
	conn, stepID := claimableStep(t)
	readable := secretFile(t, testRecoverySecret, 0o644)

	cases := []struct {
		name  string
		flags map[string]string
		want  string
	}{
		{"no owner", nil, "--owner is required"},
		{"non-positive ttl", map[string]string{"owner": "worker-1", "ttl": "-1s"}, "--ttl must be positive"},
		{"non-positive cost multiplier", map[string]string{"owner": "worker-1", "cost-multiplier": "0"},
			"--cost-multiplier must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for name, value := range c.flags {
				testsupport.Must(t, stepClaimCmd.Flags().Set(name, value), "setting --%s", name)
			}
			stepClaimCmd.SetIn(openStdin(t, readable))
			t.Cleanup(func() { stepClaimCmd.SetIn(nil) })

			_, err := runLeaseCommandOutput(t, conn, nil, stepClaimCmd, model.FormatStepID(stepID))
			if errCode(t, err) != output.ErrValidation || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("error = %v, want a VALIDATION_ERROR starting %q", err, c.want)
			}
			assertUnclaimed(t, conn, stepID)
		})
	}
}

// TestStepLeaseCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before each step lease command
// resolved its own project, including for a command refused after it
// registered the project.
func TestStepLeaseCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	conn, stepID := claimableStep(t)
	ref := model.FormatStepID(stepID)
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, stepClaimCmd.Flags().Set("owner", "worker-1"), "set --owner")
	claimed := runLeaseCommand(t, conn, anchored("/repos/step-claim"), stepClaimCmd, ref)
	token, _ := claimed["token"].(string)
	t.Setenv(TokenEnvVar, token)
	runLeaseCommand(t, conn, anchored("/repos/step-heartbeat"), stepHeartbeatCmd, ref)
	runLeaseCommand(t, conn, anchored("/repos/step-fail"), stepFailCmd, ref)

	// The failure retired the token, so this completion is refused after it
	// registers its project.
	artifact := filepath.Join(t.TempDir(), "artifact.md")
	testsupport.Must(t, os.WriteFile(artifact, []byte("result"), 0o600), "writing the artifact")
	testsupport.Must(t, stepCompleteCmd.Flags().Set("artifact-file", artifact), "set --artifact-file")
	if _, err := runLeaseCommandOutput(t, conn, anchored("/repos/step-complete"), stepCompleteCmd, ref); err == nil {
		t.Fatal("step complete with a retired token succeeded, want AUTH_ERROR")
	}

	want := map[string]string{
		"/repos/step-claim":     "step claim",
		"/repos/step-heartbeat": "step heartbeat",
		"/repos/step-fail":      "step fail",
		"/repos/step-complete":  "step complete",
	}
	got := registrationEvents(t, conn)
	if len(got) != len(want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
	for identity, verb := range want {
		if got[identity] != verb {
			t.Errorf("%s registered by %q, want %q", identity, got[identity], verb)
		}
	}
}

// TestStepClaimCommandDeliversTheTokenOfAnIncompleteClaim drives a claim
// whose lease commits and whose authoritative render then fails: the template
// indexes past the packet's files only once the step reads claimed. The
// command exits 0 with the live token and the failure in claim_error.
func TestStepClaimCommandDeliversTheTokenOfAnIncompleteClaim(t *testing.T) {
	conn, stepID := claimableStep(t)
	template := filepath.Join(t.TempDir(), "packet.tmpl")
	testsupport.Must(t, os.WriteFile(template, []byte(
		`{{.Step.Step}}{{if eq .Step.Status "claimed"}}{{index .Files 99}}{{end}}`), 0o600),
		"writing the template")

	testsupport.Must(t, stepClaimCmd.Flags().Set("owner", "worker-1"), "set --owner")
	testsupport.Must(t, stepClaimCmd.Flags().Set("render", "true"), "set --render")
	testsupport.Must(t, stepClaimCmd.Flags().Set("template", template), "set --template")
	out, err := runLeaseCommandOutput(t, conn, nil, stepClaimCmd, model.FormatStepID(stepID))
	if err != nil {
		t.Fatalf("a claim whose lease committed was reported as a failure: %v", err)
	}

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Token      string `json:"token"`
			ClaimError string `json:"claim_error"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	if !envelope.OK || !strings.Contains(envelope.Data.ClaimError, "index out of range") {
		t.Errorf("envelope = %s, want ok with the render failure in claim_error", out)
	}
	if err := db.AuthorizeStepRead(conn, stepID, envelope.Data.Token, model.NowMS()); err != nil {
		t.Errorf("the delivered token does not hold the committed lease: %v", err)
	}
}
