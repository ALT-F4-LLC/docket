package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Export renders the caller's project as one document; the client writes it.
func (s *Service) Export(ctx context.Context, req *connect.Request[docketv1.ExportRequest]) (*connect.Response[docketv1.ExportResponse], error) {
	return unary(ctx, s, req, decodeExport, verb.Export, encodeExport)
}

// decodeExport refuses a filter field verb.Export cannot apply, so an export
// never silently carries issues its caller filtered out.
func decodeExport(req *docketv1.ExportRequest) (verb.ExportInput, error) {
	f := req.GetFilter()
	if len(f.GetPriority()) > 0 || len(f.GetType()) > 0 || len(f.GetSize()) > 0 || f.GetAssignee() != "" {
		return verb.ExportInput{}, errors.New("export filters by status and label only")
	}
	return verb.ExportInput{
		Format:   req.GetFormat(),
		Statuses: f.GetStatus(),
		Labels:   f.GetLabel(),
	}, nil
}

func encodeExport(out verb.ExportResult) *docketv1.ExportResponse {
	return &docketv1.ExportResponse{Format: out.Format, Document: out.Document}
}
