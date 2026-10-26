package api

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// DaemonVersion is the daemon build version Handshake reports. The CLI sets
// it from its own build version during command setup.
var DaemonVersion = "dev"

// MetaHandler serves Handshake, the RPC a client calls before any verb. A
// daemon service embeds it; every other RPC stays unimplemented here.
type MetaHandler struct {
	docketv1connect.UnimplementedDocketServiceHandler
}

// Handshake reports the daemon's build version.
func (MetaHandler) Handshake(context.Context, *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	return connect.NewResponse(&docketv1.HandshakeResponse{DaemonVersion: DaemonVersion}), nil
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
