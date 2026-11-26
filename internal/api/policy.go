package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// PolicyResolve resolves each requested seat's model, effort and variant from
// the named run's pinned policy.toml.
func (s *Service) PolicyResolve(ctx context.Context, req *connect.Request[docketv1.PolicyResolveRequest]) (*connect.Response[docketv1.PolicyResolveResponse], error) {
	return unary(ctx, s, req, decodePolicyResolve, verb.PolicyResolve, encodePolicyResolve)
}

func decodePolicyResolve(req *docketv1.PolicyResolveRequest) (verb.PolicyResolveInput, error) {
	return verb.PolicyResolveInput{
		Run:    req.GetRun(),
		Seats:  req.GetSeats(),
		Size:   req.GetSize(),
		Labels: req.GetLabel(),
	}, nil
}

func encodePolicyResolve(res verb.PolicyResolveResult) *docketv1.PolicyResolveResponse {
	seats := make([]*docketv1.VoterAssignment, 0, len(res.Seats))
	for _, a := range res.Seats {
		seats = append(seats, &docketv1.VoterAssignment{
			Voter: a.Voter, Model: a.Model, Effort: a.Effort, Variant: a.Variant,
		})
	}
	return &docketv1.PolicyResolveResponse{Seats: seats}
}
