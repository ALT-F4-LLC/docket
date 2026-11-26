package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// ReportExecutors rolls up the executor hints and voter names across the runs
// in the request's window.
func (s *Service) ReportExecutors(ctx context.Context, req *connect.Request[docketv1.ReportExecutorsRequest]) (*connect.Response[docketv1.ReportExecutorsResponse], error) {
	return unary(ctx, s, req, decodeReportExecutors, verb.ReportExecutors, encodeReportExecutors)
}

func decodeReportExecutors(msg *docketv1.ReportExecutorsRequest) (verb.ReportExecutorsInput, error) {
	return verb.ReportExecutorsInput{Since: msg.GetSince(), AllProjects: msg.GetAllProjects()}, nil
}

// encodeReportExecutors maps each ledger row onto the wire's rollup: routings
// are fix-loop routes and rulings are override passes. A voter row carries no
// reaps or clusters, so those fields stay zero for it.
func encodeReportExecutors(out verb.ReportExecutorsResult) *docketv1.ReportExecutorsResponse {
	res := &docketv1.ReportExecutorsResponse{}
	if out.Ledger == nil {
		return res
	}
	for _, row := range out.Ledger.Executors {
		res.Executors = append(res.Executors, &docketv1.ExecutorRollup{
			Executor:             row.Executor,
			Routings:             int32(row.FixLoopRoutes),
			Rulings:              int32(row.OverridePasses),
			Reaps:                int32(row.Reaps),
			CorroboratedClusters: int32(row.CorroboratedClusters),
		})
	}
	for _, row := range out.Ledger.Voters {
		res.Voters = append(res.Voters, &docketv1.ExecutorRollup{
			Executor: row.Voter,
			Routings: int32(row.FixLoopRoutes),
			Rulings:  int32(row.OverridePasses),
		})
	}
	return res
}
