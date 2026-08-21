package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// The §6.9 refusal matrix AT THE CLI BOUNDARY.
//
// internal/engine and internal/db already test the behavior; what these assert
// is the MAPPING — that each engine or lease failure reaches the operator as
// the right error code. An error taxonomy that degrades to "general error" at
// the boundary is one no script can branch on, and that degradation is
// invisible to a test that only checks the engine returned an error.

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
				return e.DecideStep(conn, 1, true, "", model.NowMS())
			},
			want: output.ErrValidation, row: "R10",
		},
		{
			name: "R11 resolve an unparked step",
			call: func() error {
				e := engine.NewEngine()
				return e.ResolveStep(conn, 1, engine.ResolveSkip, "", model.NowMS())
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

// TestEveryStepVerbIsRegistered guards the surface §6.10 specifies. A verb
// missing from the tree is a documented capability that does not exist, and
// SKILL.md's table would be describing something no operator can run.
func TestEveryStepVerbIsRegistered(t *testing.T) {
	want := []string{
		"claim", "heartbeat", "complete", "fail",
		"approve", "reject", "resolve",
		// `reap` (DKT-83): the forced-liveness verb — a relay that established
		// its executor is dead clears the claim instead of waiting out the TTL.
		"reap",
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
	}

	have := make(map[string]bool)
	for _, sub := range stepCmd.Commands() {
		have[sub.Name()] = true
	}

	for _, verb := range want {
		if !have[verb] {
			t.Errorf("`docket step %s` is not registered, but §6.10 specifies it", verb)
		}
	}
	if len(have) != len(want) {
		t.Errorf("the step surface has %d verbs, §6.10 specifies %d — an extra "+
			"verb is surface nobody documented", len(have), len(want))
	}
}

// TestGuardVerbsAreRegistered is the same guard for §6.12, and it is now the
// COMPLETE guard surface.
//
// Until S6 this test additionally pinned that `record`/`spawn` were ABSENT:
// engine-spec §10 assigns them to stage 6 ("Guard verbs land with their
// underlying features — `stop`/`gate` at stage 3, `record`/`spawn` at stage 6"),
// and shipping them early would have frozen a shape that stage was meant to
// define. That guard did its job; the two verbs landed with the dispatch
// mechanics they are predicates over, and the absence assertion is replaced by
// the presence one.
//
// The exact-count check is what keeps this a boundary rather than a checklist:
// §2's surface names FOUR guards, so a fifth is surface nobody documented.
func TestGuardVerbsAreRegistered(t *testing.T) {
	want := []string{"stop", "gate", "record", "spawn"}

	have := make(map[string]bool)
	for _, sub := range guardCmd.Commands() {
		have[sub.Name()] = true
	}

	for _, verb := range want {
		if !have[verb] {
			t.Errorf("`docket guard %s` is not registered, but §2 specifies it", verb)
		}
	}
	if len(have) != len(want) {
		t.Errorf("the guard surface has %d verbs, §2 specifies %d — an extra "+
			"guard is surface nobody documented", len(have), len(want))
	}
}

// TestNoTokenFlagOnStepVerbs extends the S2 guard to the new surface.
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
