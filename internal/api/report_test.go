package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// reportClient serves a Service over a fresh store and returns a Connect
// client for it.
func reportClient(t *testing.T) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, NewService(openAPIStore(t), "dev"))
	return docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
}

// reportRequest carries the sentinel attribution headers and a project
// identity the fresh store has never registered, so the caller belongs to no
// project.
func reportRequest(msg *docketv1.ReportExecutorsRequest) *connect.Request[docketv1.ReportExecutorsRequest] {
	req := connect.NewRequest(msg)
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, cwdSentinel)
	req.Header().Set(projectHeader, projectSentinel)
	return req
}

func TestReportHandlersBindVerbLayer(t *testing.T) {
	c := reportClient(t)

	res, err := c.ReportExecutors(context.Background(), reportRequest(&docketv1.ReportExecutorsRequest{AllProjects: true}))

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("ReportExecutors answered unimplemented: no *Service method serves it")
	}
	if err != nil {
		t.Fatalf("ReportExecutors: %v", err)
	}
	if len(res.Msg.GetExecutors()) != 0 || len(res.Msg.GetVoters()) != 0 {
		t.Errorf("response = %v, want an empty ledger from an empty store", res.Msg)
	}
}

// Since and AllProjects reach the verb: a malformed since and a
// project-scoped read from a caller no project holds are both refused.
func TestReportExecutorsForwardsTheWindow(t *testing.T) {
	c := reportClient(t)
	for name, msg := range map[string]*docketv1.ReportExecutorsRequest{
		"malformed since":       {Since: "yesterday", AllProjects: true},
		"unbound project scope": {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.ReportExecutors(context.Background(), reportRequest(msg))

			if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
			}
		})
	}
}

func TestReportExecutorsEncodesTheLedgerRows(t *testing.T) {
	out := verb.ReportExecutorsResult{Ledger: &engine.ExecutorLedger{
		Executors: []engine.ExecutorLedgerRow{{
			Executor: "opus", FixLoopRoutes: 1, OverridePasses: 2, Reaps: 3, CorroboratedClusters: 4,
		}},
		Voters: []engine.VoterLedgerRow{{Voter: "judge", FixLoopRoutes: 5, OverridePasses: 6}},
	}}

	res := encodeReportExecutors(out)

	wantExecutor := &docketv1.ExecutorRollup{Executor: "opus", Routings: 1, Rulings: 2, Reaps: 3, CorroboratedClusters: 4}
	wantVoter := &docketv1.ExecutorRollup{Executor: "judge", Routings: 5, Rulings: 6}
	if got := res.GetExecutors(); len(got) != 1 || !proto.Equal(got[0], wantExecutor) {
		t.Errorf("executors = %v, want [%v]", got, wantExecutor)
	}
	if got := res.GetVoters(); len(got) != 1 || !proto.Equal(got[0], wantVoter) {
		t.Errorf("voters = %v, want [%v]", got, wantVoter)
	}
}
