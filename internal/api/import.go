package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Import writes the request's export document into the caller's project.
func (s *Service) Import(ctx context.Context, req *connect.Request[docketv1.ImportRequest]) (*connect.Response[docketv1.ImportResponse], error) {
	return unary(ctx, s, req, decodeImport, verb.Import, encodeImport)
}

// decodeImport hands the verb the document the request carries; the daemon
// never opens a client path.
func decodeImport(req *docketv1.ImportRequest) (verb.ImportInput, error) {
	document := []byte(req.GetDocument())
	return verb.ImportInput{
		Document: func() ([]byte, error) { return document, nil },
		Merge:    req.GetMerge(),
		Replace:  req.GetReplace(),
		Yes:      req.GetYes(),
	}, nil
}

// encodeImport answers with an empty ImportResponse: the response counts rows
// per collection, and verb.ImportResult carries only totals across all
// collections, so no field has a value to carry. Registered is never set over
// HTTP, because the daemon never treats a caller's project as anchored.
func encodeImport(verb.ImportResult) *docketv1.ImportResponse {
	return &docketv1.ImportResponse{}
}
