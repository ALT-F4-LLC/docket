package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Doctor runs the environment checks against the caller's cwd.
func (s *Service) Doctor(ctx context.Context, req *connect.Request[docketv1.DoctorRequest]) (*connect.Response[docketv1.DoctorResponse], error) {
	return unary(ctx, s, req, decodeDoctor, verb.Doctor, encodeDoctor)
}

// decodeDoctor leaves DBPath empty: the Service holds an open store but no
// database path, so the store check reports that no path was resolved.
func decodeDoctor(req *docketv1.DoctorRequest) (verb.DoctorInput, error) {
	return verb.DoctorInput{Run: req.GetRun(), Source: req.GetSource()}, nil
}

func encodeDoctor(out verb.DoctorResult) *docketv1.DoctorResponse {
	res := &docketv1.DoctorResponse{Ok: out.Clean}
	for _, c := range out.Checks {
		res.Checks = append(res.Checks, &docketv1.DoctorCheck{
			Name: c.Check, Status: string(c.Verdict), Detail: c.Detail,
		})
	}
	return res
}
