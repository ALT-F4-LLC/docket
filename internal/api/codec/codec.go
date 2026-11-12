// Package codec holds the strict protojson codec the daemon and its clients
// share: Connect's protojson wire codec plus StrictValidation, installed on
// both ends of the transport.
//
// Validation runs in an interceptor, not in Codec.Unmarshal: Connect replaces
// any Unmarshal error with a detail-less invalid_argument error, which would
// drop the INCOMPATIBLE detail, and an interceptor also checks messages a peer
// sends as binary protobuf.
package codec

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

// StrictValidation checks every message this end receives, a handler's request
// or a client's response, with output.Validator. A message holding a
// state-machine value this end does not know fails with the INCOMPATIBLE
// docket code before a handler or caller sees it.
func StrictValidation(logger *slog.Logger) connect.Interceptor {
	validator := output.NewValidator(logger)
	validate := func(message any) error {
		msg, ok := message.(proto.Message)
		if !ok {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("strict protojson: %T is not a proto message", message))
		}
		if err := validator.Validate(msg); err != nil {
			return output.NewConnectError(output.ErrIncompatible, err)
		}
		return nil
	}
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !req.Spec().IsClient {
				if err := validate(req.Any()); err != nil {
					return nil, err
				}
				return next(ctx, req)
			}
			res, err := next(ctx, req)
			if err != nil {
				return nil, err
			}
			if err := validate(res.Any()); err != nil {
				return nil, err
			}
			return res, nil
		}
	})
}
