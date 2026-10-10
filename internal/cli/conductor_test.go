package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-2465 at the CLI boundary: the seven operator verbs, and `step annotate
// --integrated-sha`, READ the conductor capability from DOCKET_TOKEN, and
// never touch stdin on a run that holds none.
//
// The engine test proves the refusal matrix for every writer; what a CLI
// test adds is the transport — the token reaches the engine from the
// invocation, and a legacy run's ruling cannot hang on a stdin pipe that
// never closes (the DKT-124 hazard `issue close` already guards against).

func assertCmdCode(t *testing.T, err error, want output.ErrorCode, verb string) {
	t.Helper()
	var ce *CmdError
	if !errors.As(err, &ce) {
		t.Fatalf("%s: err = %v, want a *CmdError with %s", verb, err, want)
	}
	if ce.Code != want {
		t.Fatalf("%s: code = %s (%v), want %s", verb, ce.Code, err, want)
	}
}

// TestConductorTokenReadsNothingOnAnUnboundRun pins the stdin hazard: a run
// with no capability must not drain stdin, and a bound one still may.
func TestConductorTokenReadsNothingOnAnUnboundRun(t *testing.T) {
	t.Setenv(TokenEnvVar, "")
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)
	t.Setenv(TokenEnvVar, "")
	gate := stepIDNamed(t, conn, "second@0")

	t.Run("bound run reads stdin", func(t *testing.T) {
		if tok := conductorToken(conn, runID, strings.NewReader("piped\n")); tok != "piped" {
			t.Errorf("token = %q, want piped", tok)
		}
		if tok := stepConductorToken(conn, gate, strings.NewReader("piped\n")); tok != "piped" {
			t.Errorf("step token = %q, want piped", tok)
		}
	})

	t.Run("unbound run never touches stdin", func(t *testing.T) {
		_, err := conn.Exec(`UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, runID)
		testsupport.Must(t, err, "unbinding: %v", err)
		if tok := conductorToken(conn, runID, tripwireReader{t}); tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
		if tok := stepConductorToken(conn, gate, tripwireReader{t}); tok != "" {
			t.Errorf("step token = %q, want empty", tok)
		}
	})

	t.Run("a missing step reads nothing", func(t *testing.T) {
		if tok := stepConductorToken(conn, 999, tripwireReader{t}); tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
	})
}

// TestOperatorVerbsReadTheCapabilityFromTheEnvironment: each verb, on a bound
// run, refuses with the environment empty, refuses AUTH_ERROR with a wrong
// value, and succeeds with the token activation returned.
func TestOperatorVerbsReadTheCapabilityFromTheEnvironment(t *testing.T) {
	type verb struct {
		name string
		// setup readies the fixture; the returned closure runs the verb.
		setup func(t *testing.T) func() error
		// refused, when set, asserts what a refusal must leave behind.
		refused func(t *testing.T, err error)
		// onStdin, when set, readies a second fixture and returns the verb
		// run with the held capability piped on stdin instead.
		onStdin func(t *testing.T, token string) func() error
	}
	// The `dispatch abandon` row's fixture, shared by its setup and hooks.
	var abandonConn *sql.DB
	var abandonRun int
	runAbandon := func(stdin string) error {
		cmd := cmdWithDB(abandonConn)
		cmd.Flags().String("run", model.FormatRunID(abandonRun), "")
		cmd.Flags().String("reason", "", "")
		cmd.SetIn(strings.NewReader(stdin))
		w, _ := bufWriter(true)
		return runDispatchAbandon(cmd, w)
	}
	dispatchStatus := func(t *testing.T) string {
		t.Helper()
		var status string
		err := abandonConn.QueryRow(`SELECT status FROM dispatches WHERE run_id = ? ORDER BY id DESC LIMIT 1`,
			abandonRun).Scan(&status)
		testsupport.Must(t, err, "reading the dispatch row: %v", err)
		return status
	}
	openAbandonable := func(t *testing.T) {
		t.Helper()
		_, err := engine.NewEngine().OpenDispatch(abandonConn, abandonRun, 0, nil, model.NowMS())
		testsupport.Must(t, err, "dispatch open: %v", err)
	}
	verbs := []verb{
		{name: "step approve", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, true, w)
			}
		}},
		{name: "step reject", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, false, w)
			}
		}},
		{name: "step resolve", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			id := parkedStep(t, conn)
			return func() error {
				cmd := resolveCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
				w, _ := bufWriter(true)
				return runStepResolve(cmd, []string{model.FormatStepID(id)}, w)
			}
		}},
		{name: "step reap", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			activatedRunForNext(t, conn)
			first := stepIDNamed(t, conn, "first@0")
			_, err := engine.ClaimStep(conn, first, engine.ClaimOptions{Owner: "doomed", NowMS: model.NowMS()})
			testsupport.Must(t, err, "claim: %v", err)
			return func() error {
				cmd := reapCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("reason", "spawn died"), "set --reason: %v", nil)
				w, _ := bufWriter(true)
				return runStepReap(cmd, []string{model.FormatStepID(first)}, w)
			}
		}},
		{name: "run pause", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			runID := activatedRunForNext(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return moveRun(runMoveCmdWithDB(conn, "pause"), model.FormatRunID(runID), runMove{
					to: model.RunWaitingHuman, from: []model.RunStatus{model.RunActive}, verb: "Paused",
				}, w)
			}
		}},
		{name: "run abandon --issue", setup: func(t *testing.T) func() error {
			conn := newTestDB(t)
			runID := activatedRunForNext(t, conn)
			var issueID int
			err := conn.QueryRow(`SELECT issue_id FROM run_issues WHERE run_id = ?`, runID).Scan(&issueID)
			testsupport.Must(t, err, "run issue: %v", err)
			return func() error {
				cmd := runMoveCmdWithDB(conn, "abandon")
				cmd.Flags().String("issue", "", "")
				testsupport.Must(t, cmd.Flags().Set("reason", "mis-routed"), "set --reason: %v", nil)
				return abandonIssueInRun(cmd, model.FormatRunID(runID), model.FormatID(issueID))
			}
		}},
		{
			name: "dispatch abandon",
			setup: func(t *testing.T) func() error {
				abandonConn = newTestDB(t)
				abandonRun = activatedRunForNext(t, abandonConn)
				openAbandonable(t)
				return func() error { return runAbandon("") }
			},
			refused: func(t *testing.T, err error) {
				var ce *CmdError
				if errors.As(err, &ce) && ce.Code == output.ErrValidation {
					for _, want := range []string{TokenEnvVar, "stdin", "run conduct"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal %q does not name %q", err, want)
						}
					}
				}
				if got := dispatchStatus(t); got != db.DispatchOpen {
					t.Errorf("dispatch status after a refusal = %q, want %q", got, db.DispatchOpen)
				}
			},
			onStdin: func(t *testing.T, token string) func() error {
				if got := dispatchStatus(t); got != db.DispatchAbandoned {
					t.Fatalf("dispatch status after the held token = %q, want %q", got, db.DispatchAbandoned)
				}
				openAbandonable(t)
				return func() error {
					if err := runAbandon(token + "\n"); err != nil {
						return err
					}
					if got := dispatchStatus(t); got != db.DispatchAbandoned {
						t.Errorf("dispatch status after the piped token = %q, want %q", got, db.DispatchAbandoned)
					}
					return nil
				}
			},
		},
	}

	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			run := v.setup(t)
			token := envToken(t)

			t.Setenv(TokenEnvVar, "")
			err := run()
			assertCmdCode(t, err, output.ErrValidation, v.name+" with no token")
			if v.refused != nil {
				v.refused(t, err)
			}
			t.Setenv(TokenEnvVar, "deadbeef")
			err = run()
			assertCmdCode(t, err, output.ErrAuth, v.name+" with a wrong token")
			if v.refused != nil {
				v.refused(t, err)
			}
			t.Setenv(TokenEnvVar, token)
			testsupport.Must(t, run(), "%s with the held capability: %v", v.name, nil)

			if v.onStdin != nil {
				piped := v.onStdin(t, token)
				t.Setenv(TokenEnvVar, "")
				err = piped()
				testsupport.Must(t, err, "%s with the capability on stdin: %v", v.name, err)
			}
		})
	}
}

// envToken reads the capability the fixture put in the environment, so a
// test can clear it and restore it.
func envToken(t *testing.T) string {
	t.Helper()
	tok := strings.TrimSpace(os.Getenv(TokenEnvVar))
	if tok == "" {
		t.Fatal("the fixture did not hold a conductor token")
	}
	return tok
}

// TestRunConductRotatesAndPrintsTheTokenOnce: the verb's two renderings and
// the rotation they report.
func TestRunConductRotatesAndPrintsTheTokenOnce(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)
	first := envToken(t)

	t.Run("json", func(t *testing.T) {
		w, buf := bufWriter(true)
		err := runRunConduct(cmdWithDB(conn), []string{model.FormatRunID(runID)}, w)
		testsupport.Must(t, err, "run conduct: %v\n%s", err, buf.String())
		var env struct {
			Data conductResponse `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal(buf.Bytes(), &env), "decoding: %v\n%s", nil, buf.String())
		if !env.Data.Rotated || env.Data.Run != model.FormatRunID(runID) {
			t.Errorf("data = %+v, want rotated on %s", env.Data, model.FormatRunID(runID))
		}
		if len(env.Data.Token) != 2*model.TokenBytes || env.Data.Token == first {
			t.Errorf("token = %q, want a fresh %d-char token", env.Data.Token, 2*model.TokenBytes)
		}
		hash, err := db.RunConductorHash(conn, runID)
		testsupport.Must(t, err, "RunConductorHash: %v", err)
		if !model.TokenMatches(env.Data.Token, hash) {
			t.Error("the printed token is not the one the run is bound to")
		}
		// The displaced conductor is refused; the seat's holder is not.
		pause := func() error {
			w, _ := bufWriter(true)
			return moveRun(runMoveCmdWithDB(conn, "pause"), model.FormatRunID(runID), runMove{
				to: model.RunWaitingHuman, from: []model.RunStatus{model.RunActive}, verb: "Paused",
			}, w)
		}
		t.Setenv(TokenEnvVar, first)
		assertCmdCode(t, pause(), output.ErrAuth, "run pause with the retired token")
		t.Setenv(TokenEnvVar, env.Data.Token)
		testsupport.Must(t, pause(), "run pause with the new token: %v", nil)
	})

	t.Run("human mode prints the token on its own line", func(t *testing.T) {
		w, buf := bufWriter(false)
		err := runRunConduct(cmdWithDB(conn), []string{model.FormatRunID(runID)}, w)
		testsupport.Must(t, err, "run conduct: %v", err)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) < 2 {
			t.Fatalf("output = %q, want a message line and a token line", buf.String())
		}
		last := lines[len(lines)-1]
		if len(last) != 2*model.TokenBytes {
			t.Errorf("last line = %q, want the bare token", last)
		}
		if strings.Contains(lines[0], last) {
			t.Error("the message line carries the token; it must stand alone")
		}
		if !strings.Contains(buf.String(), "retired") {
			t.Errorf("output %q does not say the previous capability was retired", buf.String())
		}
	})
}

// TestActivateReturnsTheConductorTokenOnce: the first activation's envelope
// carries `conductor_token` and human mode prints it on its own line; a dry
// run carries no key.
func TestActivateReturnsTheConductorTokenOnce(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedRun(t, conn)
		w, buf := bufWriter(true)
		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		testsupport.Must(t, err, "activate: %v", err)
		var env struct {
			Data struct {
				ConductorToken string `json:"conductor_token"`
			} `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal(buf.Bytes(), &env), "decoding: %v", nil)
		if len(env.Data.ConductorToken) != 2*model.TokenBytes {
			t.Fatalf("conductor_token = %q, want %d hex chars", env.Data.ConductorToken, 2*model.TokenBytes)
		}
		hash, err := db.RunConductorHash(conn, runID)
		testsupport.Must(t, err, "RunConductorHash: %v", err)
		if !model.TokenMatches(env.Data.ConductorToken, hash) {
			t.Error("the envelope's token is not the one the run is bound to")
		}
	})

	t.Run("dry run carries no key", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedRun(t, conn)
		w, buf := bufWriter(true)
		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID), "--dry-run")
		testsupport.Must(t, err, "dry run: %v", err)
		if strings.Contains(buf.String(), "conductor_token") {
			t.Errorf("a dry run's envelope carries conductor_token: %s", buf.String())
		}
	})

	t.Run("human mode prints it on its own line", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := seedRun(t, conn)
		w, buf := bufWriter(false)
		err := runActivateWithWriter(t, conn, w, model.FormatRunID(runID))
		testsupport.Must(t, err, "activate: %v", err)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		last := lines[len(lines)-1]
		if len(last) != 2*model.TokenBytes {
			t.Errorf("last line = %q, want the bare token", last)
		}
		hash, _ := db.RunConductorHash(conn, runID)
		if !model.TokenMatches(last, hash) {
			t.Error("the printed line is not the token the run is bound to")
		}
	})
}

// annotateCmdWithDB builds a `step annotate` command with its real flag names.
func annotateCmdWithDB(conn *sql.DB, sha string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("metadata", "", "")
	cmd.Flags().String("integrated-sha", sha, "")
	return cmd
}

// annotatedEffects is everything `step annotate --integrated-sha` writes: the
// step's newest issue.diff, its metadata, and the two events it logs.
type annotatedEffects struct {
	latestDiff int
	metadata   string
	events     int
}

func readAnnotatedEffects(t *testing.T, conn *sql.DB, stepID int) annotatedEffects {
	t.Helper()
	var got annotatedEffects
	err := conn.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, engine.ArtifactKindIssueDiff).Scan(&got.latestDiff)
	testsupport.Must(t, err, "reading the latest issue.diff: %v", err)
	err = conn.QueryRow(`SELECT COALESCE(metadata, '') FROM steps WHERE id = ?`, stepID).Scan(&got.metadata)
	testsupport.Must(t, err, "reading metadata: %v", err)
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE step_id = ? AND kind IN (?, ?)`,
		stepID, engine.EventStepAnnotated, engine.EventIssueDiffRepinned).Scan(&got.events)
	testsupport.Must(t, err, "counting events: %v", err)
	return got
}

// integratedRun is parkedRepinRun with implement@0 finished by an
// override-pass, so its recorded head (the executor's commit) differs from the
// commit the shared branch carries for it: the exec root's HEAD, the
// cherry-picked patch. It returns that commit and the run's conductor token.
func integratedRun(t *testing.T, conn *sql.DB) (*repinRun, string, string) {
	t.Helper()
	r := parkedRepinRun(t, conn)
	token := envToken(t)
	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	w, _ := bufWriter(true)
	err := runStepResolve(cmd, []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "finishing implement@0: %v", err)
	return r, repinGit(t, r.execRoot, "rev-parse", "HEAD"), token
}

// TestAnnotateIntegratedSHARequiresTheConductor: on a bound run the verb
// refuses a missing token VALIDATION_ERROR and a wrong one AUTH_ERROR, before
// anything is written or any git question is asked, and the run's own token
// re-records issue.diff.
func TestAnnotateIntegratedSHARequiresTheConductor(t *testing.T) {
	conn := newTestDB(t)
	r, integrated, token := integratedRun(t, conn)
	step := []string{model.FormatStepID(r.implementID)}
	annotate := func(sha string) error {
		w, _ := bufWriter(true)
		return runStepAnnotate(annotateCmdWithDB(conn, sha), step, w)
	}
	before := readAnnotatedEffects(t, conn, r.implementID)

	refusals := []struct {
		name, token, sha string
		want             output.ErrorCode
	}{
		{"no token", "", integrated, output.ErrValidation},
		{"no token and a sha the branch does not carry", "", strings.Repeat("ab", 20), output.ErrValidation},
		{"a wrong token", "deadbeef", integrated, output.ErrAuth},
	}
	for _, tc := range refusals {
		t.Setenv(TokenEnvVar, tc.token)
		assertCmdCode(t, annotate(tc.sha), tc.want, "step annotate --integrated-sha with "+tc.name)
		if after := readAnnotatedEffects(t, conn, r.implementID); after != before {
			t.Errorf("%s: the refused annotation wrote %+v -> %+v", tc.name, before, after)
		}
	}

	t.Setenv(TokenEnvVar, token)
	err := annotate(integrated)
	testsupport.Must(t, err, "step annotate with the run's token: %v", err)
	after := readAnnotatedEffects(t, conn, r.implementID)
	if after.latestDiff == before.latestDiff || after.events != before.events+2 ||
		!strings.Contains(after.metadata, integrated) {
		t.Errorf("effects %+v -> %+v, want a new issue.diff, two events, and integrated_sha %s",
			before, after, integrated)
	}
}

// TestAnnotateIntegratedSHAOnAnUnboundRunNeedsNoConductor: a run with no
// capability minted is annotated without a token.
func TestAnnotateIntegratedSHAOnAnUnboundRunNeedsNoConductor(t *testing.T) {
	conn := newTestDB(t)
	r, integrated, _ := integratedRun(t, conn)
	_, err := conn.Exec(`UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, r.runID)
	testsupport.Must(t, err, "unbinding: %v", err)
	t.Setenv(TokenEnvVar, "")
	before := readAnnotatedEffects(t, conn, r.implementID)

	w, _ := bufWriter(true)
	err = runStepAnnotate(annotateCmdWithDB(conn, integrated), []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step annotate on an unbound run: %v", err)
	if after := readAnnotatedEffects(t, conn, r.implementID); after.latestDiff == before.latestDiff {
		t.Errorf("effects %+v -> %+v, want a new issue.diff", before, after)
	}
}
