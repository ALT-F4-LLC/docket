package cli

import (
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
)

// DKT-2465 at the CLI boundary: the seven operator verbs READ the conductor
// capability from DOCKET_TOKEN, and never touch stdin on a run that holds
// none.
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
	}
	verbs := []verb{
		{"step approve", func(t *testing.T) func() error {
			conn := newTestDB(t)
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, true, w)
			}
		}},
		{"step reject", func(t *testing.T) func() error {
			conn := newTestDB(t)
			gate := readyGate(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return runDecide(decideCmdWithDB(conn), []string{model.FormatStepID(gate)}, false, w)
			}
		}},
		{"step resolve", func(t *testing.T) func() error {
			conn := newTestDB(t)
			id := parkedStep(t, conn)
			return func() error {
				cmd := resolveCmdWithDB(conn)
				testsupport.Must(t, cmd.Flags().Set("as", engine.ResolveSkip), "set --as: %v", nil)
				w, _ := bufWriter(true)
				return runStepResolve(cmd, []string{model.FormatStepID(id)}, w)
			}
		}},
		{"step reap", func(t *testing.T) func() error {
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
		{"run pause", func(t *testing.T) func() error {
			conn := newTestDB(t)
			runID := activatedRunForNext(t, conn)
			return func() error {
				w, _ := bufWriter(true)
				return moveRun(runMoveCmdWithDB(conn, "pause"), model.FormatRunID(runID), runMove{
					to: model.RunWaitingHuman, from: []model.RunStatus{model.RunActive}, verb: "Paused",
				}, w)
			}
		}},
		{"run abandon --issue", func(t *testing.T) func() error {
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
	}

	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			run := v.setup(t)
			token := envToken(t)

			t.Setenv(TokenEnvVar, "")
			assertCmdCode(t, run(), output.ErrValidation, v.name+" with no token")
			t.Setenv(TokenEnvVar, "deadbeef")
			assertCmdCode(t, run(), output.ErrAuth, v.name+" with a wrong token")
			t.Setenv(TokenEnvVar, token)
			testsupport.Must(t, run(), "%s with the held capability: %v", v.name, nil)
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
