package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// attributionHeaders are the request headers the daemon records, verbatim and
// unverified, as the author and working directory of every write. The client
// sets both on every request; the daemon never supplies either itself.
var attributionHeaders = []string{"Docket-Author", "Docket-Cwd"}

// unary serves one RPC: it refuses a request missing an attribution header,
// decodes the request, runs op, and encodes op's result. It is the one place
// a Service method maps errors: a missing header or a decode failure is
// VALIDATION_ERROR and op does not run; a *verb.Error carries its own code,
// and any other op failure is GENERAL_ERROR.
func unary[Req, Res, In, Out any](
	ctx context.Context,
	s *Service,
	req *connect.Request[Req],
	decode func(*Req) (In, error),
	op func(context.Context, *Service, In) (Out, error),
	encode func(Out) *Res,
) (*connect.Response[Res], error) {
	// Handshake is exempt because a client calls it before anything else; the
	// exemption keys on the routed procedure, which the caller cannot choose
	// for another RPC.
	if req.Spec().Procedure != docketv1connect.DocketServiceHandshakeProcedure {
		for _, name := range attributionHeaders {
			if req.Header().Get(name) == "" {
				return nil, output.NewConnectError(output.ErrValidation, fmt.Errorf(
					"the request has no %s header; the daemon records it on every request but Handshake", name))
			}
		}
	}
	in, err := decode(req.Msg)
	if err != nil {
		return nil, output.NewConnectError(output.ErrValidation, err)
	}
	out, err := op(ctx, s, in)
	if err != nil {
		return nil, opError(err, out, encode)
	}
	return connect.NewResponse(encode(out)), nil
}

// opError maps a failed op to the Connect error the client reads. A
// *verb.Error keeps its code; when op also returned a result, the encoded
// result follows the ErrorDetail as a second detail, the partial-failure form.
func opError[Res, Out any](err error, out Out, encode func(Out) *Res) *connect.Error {
	var verbErr *verb.Error
	if !errors.As(err, &verbErr) {
		return output.NewConnectError(output.ErrGeneral, err)
	}
	if verbErr == nil || verbErr.Err == nil {
		return output.NewConnectError(output.ErrGeneral, errors.New("the operation failed without a cause"))
	}
	connectErr := output.NewConnectError(verbErr.Code, err)
	if reflect.ValueOf(&out).Elem().IsZero() {
		return connectErr
	}
	// A result that does not encode as a detail leaves the error carrying its
	// code alone: the refusal still reaches the client.
	if msg, ok := any(encode(out)).(proto.Message); ok {
		if detail, detailErr := connect.NewErrorDetail(msg); detailErr == nil {
			connectErr.AddDetail(detail)
		}
	}
	return connectErr
}
