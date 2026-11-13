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

// The request headers unary reads into Env. The daemon records author and cwd
// verbatim and unverified; the client sets both on every request, and the
// daemon never supplies either itself. Project is the caller's project
// identity, copied verbatim.
const (
	authorHeader  = "Docket-Author"
	cwdHeader     = "Docket-Cwd"
	projectHeader = "Docket-Project"
)

// unary serves one RPC: it builds the Env op runs with, refuses a request
// missing an attribution header, decodes the request, runs op, and encodes
// op's result. It is the one place a Service method builds Env and maps
// errors: a missing header or a decode failure is VALIDATION_ERROR and op does
// not run; a *verb.Error carries its own code, and any other op failure is
// GENERAL_ERROR.
func unary[Req, Res any, PRes interface {
	*Res
	proto.Message
}, In, Out any](
	ctx context.Context,
	s *Service,
	req *connect.Request[Req],
	decode func(*Req) (In, error),
	op func(context.Context, verb.Env, In) (Out, error),
	encode func(Out) PRes,
) (*connect.Response[Res], error) {
	env := verb.Env{
		Store:  s.store,
		Author: req.Header().Get(authorHeader),
		Cwd:    req.Header().Get(cwdHeader),
		// The wire carries no anchoring signal, so the daemon never treats a
		// caller's identity as anchored and never registers a project from it.
		Project: verb.ProjectInput{Identity: req.Header().Get(projectHeader)},
		Build:   verb.BuildInfo{Version: s.build},
	}
	// Handshake is exempt because a client calls it before anything else; the
	// exemption keys on the routed procedure, which the caller cannot choose
	// for another RPC.
	if req.Spec().Procedure != docketv1connect.DocketServiceHandshakeProcedure {
		for _, h := range []struct{ name, value string }{{authorHeader, env.Author}, {cwdHeader, env.Cwd}} {
			if h.value == "" {
				return nil, output.NewConnectError(output.ErrValidation, fmt.Errorf(
					"the request has no %s header; the daemon records it on every request but Handshake", h.name))
			}
		}
	}
	in, err := decode(req.Msg)
	if err != nil {
		return nil, output.NewConnectError(output.ErrValidation, err)
	}
	out, err := op(ctx, env, in)
	if err != nil {
		return nil, opError(err, out, encode)
	}
	return connect.NewResponse((*Res)(encode(out))), nil
}

// opError maps a failed op to the Connect error the client reads. A
// *verb.Error keeps its code and any other error is GENERAL_ERROR. Whenever op
// also returned a non-zero result, whatever the error, the encoded result
// follows the ErrorDetail as a second detail, the partial-failure form; a verb
// that refuses must therefore return a zero result unless the client should
// see it.
func opError[Res any, PRes interface {
	*Res
	proto.Message
}, Out any](err error, out Out, encode func(Out) PRes) *connect.Error {
	code, cause := output.ErrGeneral, err
	var verbErr *verb.Error
	if errors.As(err, &verbErr) {
		if verbErr == nil || verbErr.Err == nil {
			cause = errors.New("the operation failed without a cause")
		} else {
			code = verbErr.Code
		}
	}
	connectErr := output.NewConnectError(code, cause)
	if reflect.ValueOf(&out).Elem().IsZero() {
		return connectErr
	}
	// A result that does not marshal leaves the error carrying its code
	// alone: the refusal still reaches the client.
	if detail, detailErr := connect.NewErrorDetail(encode(out)); detailErr == nil {
		connectErr.AddDetail(detail)
	}
	return connectErr
}
