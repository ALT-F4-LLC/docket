package api_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/daemon"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// gatedWorkflow is one executor step, so an activated run has a pending step
// to hold.
const gatedWorkflow = `
[pipeline]
name = "api-gated"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

// gatedCall sends one conductor-gated RPC at a run, its pending step, or a
// proposal its vote step owns, presenting token as the conductor capability.
type gatedCall func(c docketv1connect.DocketServiceClient, target gatedTarget, token string) error

type gatedTarget struct {
	run, step, proposal string
}

// gatedCalls is every RPC whose api decoder hands the verb a conductor
// token, each in the form that reaches the verb's conductor check.
var gatedCalls = map[string]gatedCall{
	"DispatchAbandon": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.DispatchAbandon(context.Background(), connect.NewRequest(&docketv1.DispatchAbandonRequest{Run: t.run, Token: token}))
		return err
	},
	"DispatchClose": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.DispatchClose(context.Background(), connect.NewRequest(&docketv1.DispatchCloseRequest{
			Run: t.run, SkipIntegrationCheck: "the branch moved", Token: token,
		}))
		return err
	},
	"DispatchWaiveTarget": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.DispatchWaiveTarget(context.Background(), connect.NewRequest(&docketv1.DispatchWaiveTargetRequest{Run: t.run, Token: token}))
		return err
	},
	"RunAbandon": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunAbandon(context.Background(), connect.NewRequest(&docketv1.RunAbandonRequest{
			Run: t.run, Reason: "done with it", Authority: engine.AuthorityOperator, Token: token,
		}))
		return err
	},
	"RunFactAdd": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunFactAdd(context.Background(), connect.NewRequest(&docketv1.RunFactAddRequest{Run: t.run, Token: token}))
		return err
	},
	"RunNoteAdd": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunNoteAdd(context.Background(), connect.NewRequest(&docketv1.RunNoteAddRequest{
			Run: t.run, Authority: engine.AuthorityOperator, Text: "a ruling", Token: token,
		}))
		return err
	},
	"RunPause": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunPause(context.Background(), connect.NewRequest(&docketv1.RunPauseRequest{
			Run: t.run, Authority: engine.AuthorityOperator, Token: token,
		}))
		return err
	},
	"RunRefreshBody": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunRefreshBody(context.Background(), connect.NewRequest(&docketv1.RunRefreshBodyRequest{Run: t.run, Token: token}))
		return err
	},
	"RunRefreshFiles": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunRefreshFiles(context.Background(), connect.NewRequest(&docketv1.RunRefreshFilesRequest{Run: t.run, Token: token}))
		return err
	},
	"RunRefreshScope": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunRefreshScope(context.Background(), connect.NewRequest(&docketv1.RunRefreshScopeRequest{Run: t.run, Token: token}))
		return err
	},
	"RunRepin": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunRepin(context.Background(), connect.NewRequest(&docketv1.RunRepinRequest{Run: t.run, Token: token}))
		return err
	},
	"RunResume": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.RunResume(context.Background(), connect.NewRequest(&docketv1.RunResumeRequest{Run: t.run, Token: token}))
		return err
	},
	"StepAnnotate": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepAnnotate(context.Background(), connect.NewRequest(&docketv1.StepAnnotateRequest{
			Step: t.step, IntegratedSha: strings.Repeat("a", 40), Token: token,
		}))
		return err
	},
	"StepApprove": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepApprove(context.Background(), connect.NewRequest(&docketv1.StepApproveRequest{
			Step: t.step, Authority: engine.AuthorityOperator, Token: token,
		}))
		return err
	},
	"StepHold": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepHold(context.Background(), connect.NewRequest(&docketv1.StepHoldRequest{
			Step: t.step, Reason: "for the operator", Token: token,
		}))
		return err
	},
	"StepReap": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepReap(context.Background(), connect.NewRequest(&docketv1.StepReapRequest{Step: t.step, Token: token}))
		return err
	},
	"StepReject": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepReject(context.Background(), connect.NewRequest(&docketv1.StepRejectRequest{
			Step: t.step, Authority: engine.AuthorityOperator, Token: token,
		}))
		return err
	},
	"StepResolve": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.StepResolve(context.Background(), connect.NewRequest(&docketv1.StepResolveRequest{
			Step: t.step, As: "skip", Authority: engine.AuthorityOperator, Token: token,
		}))
		return err
	},
	"VoteCommit": func(c docketv1connect.DocketServiceClient, t gatedTarget, token string) error {
		_, err := c.VoteCommit(context.Background(), connect.NewRequest(&docketv1.VoteCommitRequest{
			Proposal: t.proposal, Outcome: "committed", Token: token,
		}))
		return err
	},
}

// TestConductorGatedVerbsRequireConductOverHTTP: over the daemon's HTTP
// listener a conductor-gated RPC on a run that holds no conductor capability
// is refused with AUTH_ERROR naming `docket run conduct`, and changes
// nothing; over the Unix socket the same request is accepted as before; and
// once the run is conducted over HTTP, the token it returns rules there.
func TestConductorGatedVerbsRequireConductOverHTTP(t *testing.T) {
	// Activation reads config beside the store; an empty one keeps the
	// operator's own config out of the run.
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	st := openTransportStore(t)
	insertGatedWorkflow(t, st)
	logger := slog.New(slog.DiscardHandler)
	path, handler := api.NewHandler(daemon.NewService(st, "test"), logger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	overSocket := serveSocket(t, mux, logger)
	overHTTP := serveLoopbackHTTP(t, mux, logger)

	t.Run("HTTP refuses a hold on an unbound run", func(t *testing.T) {
		target, stepID := unboundActiveRun(t, st)
		err := gatedCalls["StepHold"](overHTTP, target, "")
		requireConductRefusal(t, err, target.run)
		if status := stepStatus(t, st, stepID); status != store.StepPending {
			t.Errorf("step status after the refused hold = %s, want %s", status, store.StepPending)
		}
	})

	t.Run("socket holds on an unbound run", func(t *testing.T) {
		target, stepID := unboundActiveRun(t, st)
		if err := gatedCalls["StepHold"](overSocket, target, ""); err != nil {
			t.Fatalf("StepHold over the socket on an unbound run: %v", err)
		}
		if status := stepStatus(t, st, stepID); status != store.StepWaitingHuman {
			t.Errorf("step status after the socket hold = %s, want %s", status, store.StepWaitingHuman)
		}
	})

	t.Run("HTTP holds once it conducts the run", func(t *testing.T) {
		target, stepID := unboundActiveRun(t, st)
		res, err := overHTTP.RunConduct(context.Background(), connect.NewRequest(&docketv1.RunConductRequest{Run: target.run}))
		if err != nil {
			t.Fatalf("RunConduct over HTTP: %v", err)
		}
		if err := gatedCalls["StepHold"](overHTTP, target, res.Msg.GetToken()); err != nil {
			t.Fatalf("StepHold over HTTP with the conducted token: %v", err)
		}
		if status := stepStatus(t, st, stepID); status != store.StepWaitingHuman {
			t.Errorf("step status after the conducted hold = %s, want %s", status, store.StepWaitingHuman)
		}
	})

	t.Run("HTTP refuses every gated RPC on an unbound run", func(t *testing.T) {
		for _, name := range slices.Sorted(maps.Keys(gatedCalls)) {
			t.Run(name, func(t *testing.T) {
				target, stepID := unboundActiveRun(t, st)
				target.proposal = voteStepProposal(t, st, target.run)
				requireConductRefusal(t, gatedCalls[name](overHTTP, target, ""), target.run)
				if status := stepStatus(t, st, stepID); status != store.StepPending {
					t.Errorf("step status after the refused ruling = %s, want %s", status, store.StepPending)
				}
				if status := runStatus(t, st, target.run); status != model.RunActive {
					t.Errorf("run status after the refused ruling = %s, want %s", status, model.RunActive)
				}
			})
		}
	})

	t.Run("HTTP passes the ungated forms through", func(t *testing.T) {
		target, _ := unboundActiveRun(t, st)
		ungated := map[string]error{}
		_, ungated["StepAnnotate without an integrated sha"] = overHTTP.StepAnnotate(context.Background(),
			connect.NewRequest(&docketv1.StepAnnotateRequest{Step: target.step, MetadataJson: `{"k":"v"}`}))
		_, ungated["DispatchClose without an override"] = overHTTP.DispatchClose(context.Background(),
			connect.NewRequest(&docketv1.DispatchCloseRequest{Run: target.run}))
		for form, err := range ungated {
			if output.ErrorCodeFromConnectError(err) == output.ErrAuth {
				t.Errorf("%s was refused as a conductor ruling: %v", form, err)
			}
		}
	})

	t.Run("HTTP refuses a planning run until it is conducted", func(t *testing.T) {
		_, run := unboundRun(t, st)
		target := gatedTarget{run: run}
		requireConductRefusal(t, gatedCalls["RunNoteAdd"](overHTTP, target, ""), run)
		res, err := overHTTP.RunConduct(context.Background(), connect.NewRequest(&docketv1.RunConductRequest{Run: run}))
		if err != nil {
			t.Fatalf("RunConduct over HTTP on a planning run: %v", err)
		}
		if err := gatedCalls["RunNoteAdd"](overHTTP, target, res.Msg.GetToken()); err != nil {
			t.Errorf("RunNoteAdd over HTTP with the conducted token: %v", err)
		}
	})
}

// TestGatedCallsCoverEveryConductorTokenDecoder: gatedCalls lists exactly
// the RPCs whose api decoder sets ConductorToken, so a newly gated RPC cannot
// go unchecked by the HTTP refusal test above.
func TestGatedCallsCoverEveryConductorTokenDecoder(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the api package: %v", err)
	}
	var decoders []string
	for _, file := range pkgs["api"].Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "decode") {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ConductorToken" {
					decoders = append(decoders, strings.TrimPrefix(fn.Name.Name, "decode"))
					return false
				}
				return true
			})
		}
	}
	slices.Sort(decoders)
	if listed := slices.Sorted(maps.Keys(gatedCalls)); !slices.Equal(decoders, listed) {
		t.Errorf("RPCs whose decoder sets ConductorToken = %v, gatedCalls lists %v", decoders, listed)
	}
}

func insertGatedWorkflow(t *testing.T, st store.Store) {
	t.Helper()
	def, err := workflow.Load([]byte(gatedWorkflow))
	if err != nil {
		t.Fatalf("workflow.Load: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		t.Fatalf("workflow.Canonical: %v", err)
	}
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: store.DefaultProjectID,
		Name:      def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(gatedWorkflow)),
		Body:         gatedWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
}

// unboundActiveRun activates a run of gatedWorkflow and clears the conductor
// capability activation bound it to, as a run activated before capabilities
// existed holds none. It returns the run and its pending step.
func unboundActiveRun(t *testing.T, st store.Store) (gatedTarget, int) {
	t.Helper()
	runID, run := unboundRun(t, st)
	if _, err := engine.ActivateStore(st, runID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	conn, err := store.SQLDB(st)
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	if _, err := conn.Exec(`UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, runID); err != nil {
		t.Fatalf("clearing the conductor capability: %v", err)
	}
	steps, err := st.ListRunSteps(runID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("ListRunSteps = %d steps, %v; want the one pending step", len(steps), err)
	}
	return gatedTarget{run: run, step: model.FormatStepID(steps[0].ID)}, steps[0].ID
}

// voteStepProposal opens an approved proposal that a vote step of run owns,
// so committing it is a conductor ruling on run.
func voteStepProposal(t *testing.T, st store.Store, run string) string {
	t.Helper()
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	id, err := st.CreateProposalIdempotent(&model.Proposal{
		Description: "a vote step's ballot", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusApproved, RequiredVoters: 1, Threshold: 0.5,
	}, fmt.Sprintf("vote-step:%d:vote@0", runID))
	if err != nil {
		t.Fatalf("CreateProposalIdempotent: %v", err)
	}
	return model.FormatProposalID(id)
}

func stepStatus(t *testing.T, st store.Store, id int) string {
	t.Helper()
	step, err := st.GetStep(id)
	if err != nil {
		t.Fatalf("GetStep(%d): %v", id, err)
	}
	return step.Status
}

func runStatus(t *testing.T, st store.Store, run string) model.RunStatus {
	t.Helper()
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	r, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", run, err)
	}
	return r.Status
}

// requireConductRefusal fails unless err is an AUTH_ERROR that names
// `docket run conduct` for run.
func requireConductRefusal(t *testing.T, err error, run string) {
	t.Helper()
	if err == nil {
		t.Fatal("the request was accepted on an unbound run over HTTP")
	}
	if code := output.ErrorCodeFromConnectError(err); code != output.ErrAuth {
		t.Errorf("refusal code = %q, want %s (%v)", code, output.ErrAuth, err)
	}
	if want := "docket run conduct " + run; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not name %q", err.Error(), want)
	}
}
