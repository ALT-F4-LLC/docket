package api

import (
	"context"
	"strconv"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// GateStatus reads one human or vote gate step's decision state.
func (s *Service) GateStatus(ctx context.Context, req *connect.Request[docketv1.GateStatusRequest]) (*connect.Response[docketv1.GateStatusResponse], error) {
	return unary(ctx, s, req, decodeGateStatus, verb.GateStatus, encodeGateStatus)
}

func decodeGateStatus(req *docketv1.GateStatusRequest) (verb.GateStatusInput, error) {
	return verb.GateStatusInput{Step: req.GetStep()}, nil
}

// encodeGateStatus puts the gate step's effective status in step, the field
// the CLI renders as "step: <status>".
func encodeGateStatus(res verb.GateStatusResult) *docketv1.GateStatusResponse {
	return &docketv1.GateStatusResponse{
		Step:         res.StepStatus,
		Outcome:      string(res.Outcome),
		Tally:        gateTally(res),
		MissingSeats: res.MissingSeats,
	}
}

// gateTally renders res's tally as "<weighted_score>/<threshold>", with
// "unscored" for a proposal no vote has scored yet, and as the empty string
// for a gate with no proposal.
func gateTally(res verb.GateStatusResult) string {
	if res.Tally == nil {
		return ""
	}
	score := "unscored"
	if res.Tally.WeightedScore != nil {
		score = strconv.FormatFloat(*res.Tally.WeightedScore, 'g', -1, 64)
	}
	return score + "/" + strconv.FormatFloat(res.Tally.Threshold, 'g', -1, 64)
}
