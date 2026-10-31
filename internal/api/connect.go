// Package api holds the daemon's Connect API. `make proto` generates its Go
// messages (docketv1) and Connect client and handler stubs (docketv1connect)
// from proto/docket/v1/docket.proto; the generated files are committed.
package api

import (
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// NewHandler mounts svc for the daemon and returns the path to serve it on.
// It rejects a request carrying a state-machine value the daemon does not
// know as INCOMPATIBLE.
func NewHandler(svc docketv1connect.DocketServiceHandler, logger *slog.Logger) (string, http.Handler) {
	return docketv1connect.NewDocketServiceHandler(svc, handlerOptions(logger))
}

// requestMaxBytes caps one request message, measured after decompression.
// It is 8 MiB: room for a step completion carrying a db.ArtifactMaxBytes
// artifact even at protojson's worst-case escaping.
const requestMaxBytes = 8 << 20

// handlerOptions configure every daemon handler: the request size cap, which
// Connect enforces before decoding, and the strict protojson codec. Connect
// handlers already accept protojson, so only its validation is added.
func handlerOptions(logger *slog.Logger) connect.HandlerOption {
	return connect.WithHandlerOptions(
		connect.WithReadMaxBytes(requestMaxBytes),
		connect.WithInterceptors(strictValidation(logger)),
	)
}

// NewClient returns a client for the daemon at baseURL that speaks the strict
// protojson codec: a response carrying a state-machine value this client does
// not know fails with the INCOMPATIBLE docket code.
func NewClient(httpClient connect.HTTPClient, baseURL string, logger *slog.Logger) docketv1connect.DocketServiceClient {
	return docketv1connect.NewDocketServiceClient(httpClient, baseURL,
		connect.WithProtoJSON(),
		connect.WithInterceptors(strictValidation(logger)),
	)
}
