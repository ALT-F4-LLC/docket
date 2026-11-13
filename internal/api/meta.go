package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Handshake reports the build version the Service was built with.
func (s *Service) Handshake(ctx context.Context, req *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	return unary(ctx, s, req, decodeHandshake, handshake, encodeHandshake)
}

// decodeHandshake accepts every request: Handshake reads no request field.
func decodeHandshake(*docketv1.HandshakeRequest) (struct{}, error) {
	return struct{}{}, nil
}

func handshake(_ context.Context, env verb.Env, _ struct{}) (string, error) {
	return env.Build.Version, nil
}

func encodeHandshake(build string) *docketv1.HandshakeResponse {
	return &docketv1.HandshakeResponse{DaemonVersion: build}
}
