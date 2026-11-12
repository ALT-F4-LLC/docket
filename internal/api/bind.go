package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

// unary serves one RPC: it decodes the request, runs op, and encodes op's
// result. It is the one place a Service method maps errors: a decode failure
// is VALIDATION_ERROR and an op failure is GENERAL_ERROR, and op does not run
// on a request that failed to decode.
func unary[Req, Res, In, Out any](
	ctx context.Context,
	s *Service,
	req *connect.Request[Req],
	decode func(*Req) (In, error),
	op func(context.Context, *Service, In) (Out, error),
	encode func(Out) *Res,
) (*connect.Response[Res], error) {
	in, err := decode(req.Msg)
	if err != nil {
		return nil, output.NewConnectError(output.ErrValidation, err)
	}
	out, err := op(ctx, s, in)
	if err != nil {
		return nil, output.NewConnectError(output.ErrGeneral, err)
	}
	return connect.NewResponse(encode(out)), nil
}
