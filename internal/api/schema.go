package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// SchemaDeprecate retires a schema version from service in the caller's
// project.
func (s *Service) SchemaDeprecate(ctx context.Context, req *connect.Request[docketv1.SchemaDeprecateRequest]) (*connect.Response[docketv1.SchemaDeprecateResponse], error) {
	return unary(ctx, s, req, decodeSchemaDeprecate, verb.SchemaDeprecate, encodeSchemaDeprecate)
}

// SchemaList lists the schemas registered in the caller's project.
func (s *Service) SchemaList(ctx context.Context, req *connect.Request[docketv1.SchemaListRequest]) (*connect.Response[docketv1.SchemaListResponse], error) {
	return unary(ctx, s, req, decodeSchemaList, verb.SchemaList, encodeSchemaList)
}

// SchemaRegister registers the schema document the request carries.
func (s *Service) SchemaRegister(ctx context.Context, req *connect.Request[docketv1.SchemaRegisterRequest]) (*connect.Response[docketv1.SchemaRegisterResponse], error) {
	return unary(ctx, s, req, decodeSchemaRegister, verb.SchemaRegister, encodeSchemaRegister)
}

// SchemaShow reads one schema from the caller's project.
func (s *Service) SchemaShow(ctx context.Context, req *connect.Request[docketv1.SchemaShowRequest]) (*connect.Response[docketv1.SchemaShowResponse], error) {
	return unary(ctx, s, req, decodeSchemaShow, verb.SchemaShow, encodeSchemaShow)
}

// schemaRef joins a request's name and version into the name@version ref the
// verb parses, so a malformed pair is refused there as it is for the CLI.
func schemaRef(name string, version int32) string {
	return fmt.Sprintf("%s@%d", name, version)
}

func decodeSchemaDeprecate(req *docketv1.SchemaDeprecateRequest) (verb.SchemaDeprecateInput, error) {
	return verb.SchemaDeprecateInput{Ref: schemaRef(req.GetName(), req.GetVersion())}, nil
}

func encodeSchemaDeprecate(res verb.SchemaDeprecateResult) *docketv1.SchemaDeprecateResponse {
	return &docketv1.SchemaDeprecateResponse{Schema: schemaMessage(res.Schema)}
}

func decodeSchemaList(req *docketv1.SchemaListRequest) (verb.SchemaListInput, error) {
	return verb.SchemaListInput{
		Name:              req.GetName(),
		Limit:             int(req.GetLimit()),
		IncludeDeprecated: req.GetDeprecated(),
	}, nil
}

func encodeSchemaList(res verb.SchemaListResult) *docketv1.SchemaListResponse {
	items := make([]*docketv1.Schema, 0, len(res.Schemas))
	for _, s := range res.Schemas {
		items = append(items, schemaMessage(s))
	}
	return &docketv1.SchemaListResponse{
		Items:     items,
		Total:     int32(res.Total),
		Truncated: len(items) < res.Total,
	}
}

func decodeSchemaRegister(req *docketv1.SchemaRegisterRequest) (verb.SchemaRegisterInput, error) {
	body := []byte(req.GetBody())
	return verb.SchemaRegisterInput{
		Ref:        schemaRef(req.GetName(), req.GetVersion()),
		Document:   func() ([]byte, error) { return body, nil },
		SourcePath: req.GetSourcePath(),
	}, nil
}

func encodeSchemaRegister(res verb.SchemaRegisterResult) *docketv1.SchemaRegisterResponse {
	return &docketv1.SchemaRegisterResponse{Schema: schemaMessage(res.Schema)}
}

// decodeSchemaShow sends a bare name when the request leaves version unset,
// which the verb reads as the highest version still in service. The request's
// body flag is a rendering choice: the response always carries the body, and
// whether to print it is the client's decision.
func decodeSchemaShow(req *docketv1.SchemaShowRequest) (verb.SchemaShowInput, error) {
	if req.Version == nil {
		return verb.SchemaShowInput{Ref: req.GetName()}, nil
	}
	return verb.SchemaShowInput{Ref: schemaRef(req.GetName(), req.GetVersion())}, nil
}

func encodeSchemaShow(res verb.SchemaShowResult) *docketv1.SchemaShowResponse {
	return &docketv1.SchemaShowResponse{Schema: schemaMessage(res.Schema)}
}

// schemaMessage is s on the wire, or nil when the verb returned no row.
func schemaMessage(s *model.Schema) *docketv1.Schema {
	if s == nil {
		return nil
	}
	return &docketv1.Schema{
		Name:         s.Name,
		Version:      int32(s.Version),
		SourcePath:   s.SourcePath,
		SourceSha256: s.SourceSHA256,
		Body:         s.Body,
		Builtin:      s.Builtin,
		Deprecated:   s.Deprecated(),
		CreatedAtMs:  s.CreatedAtMS,
		RowVersion:   int32(s.RowVersion),
	}
}
