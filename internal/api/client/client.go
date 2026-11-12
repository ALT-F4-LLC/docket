// Package client builds the CLI's Connect client for the daemon.
package client

import (
	"log/slog"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/codec"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// NewClient returns a client for the daemon at baseURL that speaks the strict
// protojson codec: a response carrying a state-machine value this client does
// not know fails with the INCOMPATIBLE docket code.
func NewClient(httpClient connect.HTTPClient, baseURL string, logger *slog.Logger) docketv1connect.DocketServiceClient {
	return docketv1connect.NewDocketServiceClient(httpClient, baseURL,
		connect.WithProtoJSON(),
		connect.WithInterceptors(codec.StrictValidation(logger)),
	)
}
