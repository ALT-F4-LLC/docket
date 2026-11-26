package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// identityHeaders is what a raw client puts on a request. Docket-Project
// selects the project a verb reads, so a case fixes it; author and cwd are the
// attribution a caller can forge.
type identityHeaders struct{ author, cwd, project string }

// forgedAs returns h with a forged author and cwd and the same project.
func (h identityHeaders) forgedAs(author, cwd string) identityHeaders {
	h.author, h.cwd = author, cwd
	return h
}

// sendAs sends msg through rpc carrying h, and returns the response message
// or the error.
func sendAs[Req, Res any, PRes interface {
	*Res
	proto.Message
}](rpc func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), h identityHeaders, msg *Req) (proto.Message, error) {
	req := connect.NewRequest(msg)
	req.Header().Set(authorHeader, h.author)
	req.Header().Set(cwdHeader, h.cwd)
	if h.project != "" {
		req.Header().Set(projectHeader, h.project)
	}
	res, err := rpc(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return PRes(res.Msg), nil
}

// wireCode names the Connect code a call answered, with "ok" for success,
// which connect.CodeOf cannot tell from an error of unknown code.
func wireCode(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

// forgedIdentityCall sends one phase-8 RPC through a raw client with h.
type forgedIdentityCall func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error)

// The test drives a raw docketv1connect client: the daemon client's
// attribution interceptor overwrites both headers, so it cannot forge them.
func TestForgedIdentityHeaders(t *testing.T) {
	t.Run("phase-8 handlers answer a forged author and cwd as they answer the fixture's own", func(t *testing.T) {
		cases := []struct {
			rpc string
			// arrange builds the store the RPC reads, and returns the project
			// the request names and the call.
			arrange func(t *testing.T) (store.Store, string, forgedIdentityCall)
			// cwdIsInput marks Doctor, which checks the Docket-Cwd directory:
			// its checks may differ, its Connect code may not.
			cwdIsInput bool
		}{
			{rpc: "Board", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st := openAPIStore(t)
				createBoardCard(t, st, "card", model.StatusTodo, nil)
				return st, "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.Board, h, &docketv1.BoardRequest{})
				}
			}},
			{rpc: "Doctor", cwdIsInput: true, arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				return openAPIStore(t), "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.Doctor, h, &docketv1.DoctorRequest{})
				}
			}},
			{rpc: "GateStatus", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				conn, _, gate := activatedClaimDB(t)
				return store.StoreOn(conn), "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.GateStatus, h, &docketv1.GateStatusRequest{Step: model.FormatStepID(gate)})
				}
			}},
			// GuardSpawn denies with a nil error, so only the response
			// comparison below can see a flipped verdict.
			{rpc: "GuardSpawn", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				const caller = "/repos/forged-guard"
				st := openAPIStore(t)
				projectID, err := st.EnsureProject(caller, "forged-guard", 1)
				if err != nil {
					t.Fatalf("EnsureProject: %v", err)
				}
				run, _, err := st.InsertRunWithContextIdempotent(projectID, "", 0, model.NowMS(), db.RunContext{}, "")
				if err != nil {
					t.Fatalf("starting a run: %v", err)
				}
				return st, caller, func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.GuardSpawn, h, &docketv1.GuardSpawnRequest{
						Run: model.FormatRunID(run.ID), Rows: []*docketv1.StepRow{{Step: "STEP-1"}},
					})
				}
			}},
			{rpc: "Next", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st := openAPIStore(t)
				createBoardCard(t, st, "ready", model.StatusTodo, nil)
				return st, "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.Next, h, &docketv1.NextRequest{})
				}
			}},
			{rpc: "PinShow", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st, run := pinShowStore(t)
				return st, "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.PinShow, h, &docketv1.PinShowRequest{Run: run, Path: "policy.toml"})
				}
			}},
			{rpc: "Plan", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st := openAPIStore(t)
				projectID, err := st.EnsureProject(planCallerIdentity, "plan-caller", 1)
				if err != nil {
					t.Fatalf("EnsureProject: %v", err)
				}
				createPlannedIssue(t, st, projectID, "planned")
				return st, planCallerIdentity, func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.Plan, h, &docketv1.PlanRequest{})
				}
			}},
			{rpc: "PolicyResolve", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st := openProjectStore(t)
				run := policyRun(t, st)
				return st, "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.PolicyResolve, h, &docketv1.PolicyResolveRequest{Run: run, Seats: []string{"seat-a"}})
				}
			}},
			{rpc: "ReportExecutors", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				return openAPIStore(t), "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.ReportExecutors, h, &docketv1.ReportExecutorsRequest{AllProjects: true})
				}
			}},
			{rpc: "Stats", arrange: func(t *testing.T) (store.Store, string, forgedIdentityCall) {
				st := openProjectStore(t)
				createStatsIssue(t, st, model.StatusTodo, model.PriorityHigh, nil, []string{"bug"})
				return st, "", func(c docketv1connect.DocketServiceClient, h identityHeaders) (proto.Message, error) {
					return sendAs(c.Stats, h, &docketv1.StatsRequest{})
				}
			}},
		}
		for _, tc := range cases {
			t.Run(tc.rpc, func(t *testing.T) {
				st, project, call := tc.arrange(t)
				server := serve(t, NewService(st, "test"))
				raw := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
				own := identityHeaders{author: authorSentinel, cwd: cwdSentinel, project: project}
				forged := own.forgedAs("mallory", "/forged/cwd")

				ownRes, ownErr := call(raw, own)
				// A served baseline keeps the comparison from passing on two
				// refusals that would hide a decision.
				requireServed(t, tc.rpc+" with the fixture's own headers", ownErr)
				forgedRes, forgedErr := call(raw, forged)

				if got, want := wireCode(forgedErr), wireCode(ownErr); got != want {
					t.Fatalf("%s with a forged author and cwd answered %s (err %v), want %s as with the fixture's own", tc.rpc, got, forgedErr, want)
				}
				if !tc.cwdIsInput && !proto.Equal(forgedRes, ownRes) {
					t.Errorf("%s with a forged author and cwd answered\n%v\nwant the fixture's own answer\n%v", tc.rpc, forgedRes, ownRes)
				}
			})
		}
	})

	t.Run("a forged author matching the seated conductor does not stand in for its token", func(t *testing.T) {
		f := newStepFixture(t)
		target := f.boundRunTarget(t)
		server := serve(t, NewService(f.st, "test"))
		raw := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
		seated := identityHeaders{author: "alice", cwd: f.execRoot}

		conducted, err := sendAs(raw.RunConduct, seated, &docketv1.RunConductRequest{Run: target.run})
		requireServed(t, "RunConduct as alice", err)
		fresh := conducted.(*docketv1.RunConductResponse).GetToken()
		if fresh == "" || fresh == target.conductor {
			t.Fatalf("premise: RunConduct returned %q, want a token other than the activation's", fresh)
		}
		addNote := func(token string) error {
			_, err := sendAs(raw.RunNoteAdd, seated, &docketv1.RunNoteAddRequest{
				Run: target.run, Authority: engine.AuthorityOperator, Text: "forged ruling", Token: token,
			})
			return err
		}

		for _, wrong := range []struct{ name, token string }{
			{"the token RunConduct retired", target.conductor},
			{"a token never minted", "not-a-capability"},
		} {
			before := f.runState(t, target)

			err := addNote(wrong.token)

			requireRefusal(t, "RunNoteAdd as alice with "+wrong.name, err, connect.CodePermissionDenied, output.ErrAuth)
			if after := f.runState(t, target); after != before {
				t.Errorf("RunNoteAdd as alice with %s: run %+v, want %+v unchanged", wrong.name, after, before)
			}
		}

		// The same request with the token RunConduct returned is a ruling, so
		// the refusals above are the token's doing, not the request's.
		requireServed(t, "RunNoteAdd as alice with the token RunConduct returned", addNote(fresh))
	})
}
