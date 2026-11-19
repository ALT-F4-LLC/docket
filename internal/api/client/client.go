// Package client builds the CLI's Connect client for the daemon.
package client

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/codec"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// NewClient returns a client for the daemon at baseURL that speaks the strict
// protojson codec: a response carrying a state-machine value this client does
// not know fails with the INCOMPATIBLE docket code. Every request carries the
// caller's author and working directory in the Docket-Author and Docket-Cwd
// headers.
func NewClient(httpClient connect.HTTPClient, baseURL string, logger *slog.Logger) docketv1connect.DocketServiceClient {
	return docketv1connect.NewDocketServiceClient(httpClient, baseURL,
		connect.WithProtoJSON(),
		connect.WithInterceptors(codec.StrictValidation(logger), attribution()),
	)
}

// Connect returns a client for the daemon at baseURL after one Handshake,
// with the daemon's handshake response for the caller to check its build.
func Connect(ctx context.Context, httpClient connect.HTTPClient, baseURL string, logger *slog.Logger) (docketv1connect.DocketServiceClient, *docketv1.HandshakeResponse, error) {
	client := NewClient(httpClient, baseURL, logger)
	res, err := client.Handshake(ctx, connect.NewRequest(&docketv1.HandshakeRequest{}))
	if err != nil {
		return nil, nil, err
	}
	return client, res.Msg, nil
}
