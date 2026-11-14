package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// ProjectDelete deletes the project the request names.
func (s *Service) ProjectDelete(ctx context.Context, req *connect.Request[docketv1.ProjectDeleteRequest]) (*connect.Response[docketv1.ProjectDeleteResponse], error) {
	return unary(ctx, s, req, decodeProjectDelete, verb.ProjectDelete, encodeProjectDelete)
}

// ProjectList lists the store's projects.
func (s *Service) ProjectList(ctx context.Context, req *connect.Request[docketv1.ProjectListRequest]) (*connect.Response[docketv1.ProjectListResponse], error) {
	return unary(ctx, s, req, decodeProjectList, verb.ProjectList, encodeProjectList)
}

// ProjectSetPrefix sets the caller's project's display prefix.
func (s *Service) ProjectSetPrefix(ctx context.Context, req *connect.Request[docketv1.ProjectSetPrefixRequest]) (*connect.Response[docketv1.ProjectSetPrefixResponse], error) {
	return unary(ctx, s, req, decodeProjectSetPrefix, verb.ProjectSetPrefix, encodeProjectSetPrefix)
}

func decodeProjectDelete(msg *docketv1.ProjectDeleteRequest) (verb.ProjectDeleteInput, error) {
	return verb.ProjectDeleteInput{Project: msg.GetProject()}, nil
}

// encodeProjectDelete drops Registered: the daemon's Env is never anchored, so
// the verb never registers a project for it.
func encodeProjectDelete(res verb.ProjectDeleteResult) *docketv1.ProjectDeleteResponse {
	return &docketv1.ProjectDeleteResponse{Project: encodeProject(res.Project)}
}

func decodeProjectList(*docketv1.ProjectListRequest) (verb.ProjectListInput, error) {
	return verb.ProjectListInput{}, nil
}

// encodeProjectList returns every project; the listing is never truncated.
func encodeProjectList(res verb.ProjectListResult) *docketv1.ProjectListResponse {
	items := make([]*docketv1.Project, 0, len(res.Projects))
	for _, p := range res.Projects {
		items = append(items, encodeProject(p))
	}
	return &docketv1.ProjectListResponse{Items: items, Total: int32(len(items))}
}

func decodeProjectSetPrefix(msg *docketv1.ProjectSetPrefixRequest) (verb.ProjectSetPrefixInput, error) {
	return verb.ProjectSetPrefixInput{Prefix: msg.GetPrefix()}, nil
}

// encodeProjectSetPrefix drops Registered, as encodeProjectDelete does.
func encodeProjectSetPrefix(res verb.ProjectSetPrefixResult) *docketv1.ProjectSetPrefixResponse {
	return &docketv1.ProjectSetPrefixResponse{Project: encodeProject(res.Project)}
}

// encodeProject returns nil for a nil row: a refused verb returns a result
// with no project.
func encodeProject(p *model.Project) *docketv1.Project {
	if p == nil {
		return nil
	}
	return &docketv1.Project{
		Id: int32(p.ID), Identity: p.Identity, Name: p.Name,
		Prefix: p.Prefix, CreatedAtMs: p.CreatedAtMS,
	}
}
