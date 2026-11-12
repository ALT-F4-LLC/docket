package api

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// Handshake reports the build version the Service was built with.
func (s *Service) Handshake(ctx context.Context, req *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	return unary(ctx, s, req, decodeHandshake, handshake, encodeHandshake)
}

// decodeHandshake accepts every request: Handshake reads no request field.
func decodeHandshake(*docketv1.HandshakeRequest) (struct{}, error) {
	return struct{}{}, nil
}

func handshake(_ context.Context, s *Service, _ struct{}) (string, error) {
	return s.build, nil
}

func encodeHandshake(build string) *docketv1.HandshakeResponse {
	return &docketv1.HandshakeResponse{DaemonVersion: build}
}

// Connect forwards to client.Connect while callers move to that package.
func Connect(ctx context.Context, httpClient connect.HTTPClient, baseURL string, logger *slog.Logger) (docketv1connect.DocketServiceClient, *docketv1.HandshakeResponse, error) {
	return client.Connect(ctx, httpClient, baseURL, logger)
}
