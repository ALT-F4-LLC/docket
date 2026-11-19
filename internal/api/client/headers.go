package client

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

const (
	authorHeader = "Docket-Author"
	cwdHeader    = "Docket-Cwd"
)

// attribution sets Docket-Author and Docket-Cwd on every outgoing unary
// request, replacing any value already present. The daemon records both
// verbatim and unverified. A working directory that cannot be read stops the
// request before it is sent: the cwd is never sent empty or made up.
func attribution() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !req.Spec().IsClient {
				return next(ctx, req)
			}
			cwd, err := os.Getwd()
			if err != nil {
				return nil, output.NewConnectError(output.ErrGeneral, fmt.Errorf(
					"the working directory could not be resolved (%w); a daemon "+
						"request must record where it was made from, so run it "+
						"again from a readable directory", err))
			}
			req.Header().Set(authorHeader, config.DefaultAuthor())
			req.Header().Set(cwdHeader, cwd)
			return next(ctx, req)
		}
	})
}
