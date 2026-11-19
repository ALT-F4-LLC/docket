package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func (s *Service) Next(ctx context.Context, req *connect.Request[docketv1.NextRequest]) (*connect.Response[docketv1.NextResponse], error) {
	return unary(ctx, s, req, decodeNext, verb.Next, encodeNext(req.Msg.GetWithBody(), int(req.Msg.GetLimit())))
}

// decodeNext refuses an assignee filter, which verb.Next cannot apply, so the
// ready set never silently carries issues its caller filtered out.
func decodeNext(m *docketv1.NextRequest) (verb.NextInput, error) {
	f := m.GetFilter()
	if f.GetAssignee() != "" {
		return verb.NextInput{}, errors.New("next filters by status, priority, size, label and type only")
	}
	return verb.NextInput{
		Statuses:   f.GetStatus(),
		Priorities: f.GetPriority(),
		Sizes:      f.GetSize(),
		Labels:     f.GetLabel(),
		Types:      f.GetType(),
		Run:        m.GetRun(),
		Limit:      int(m.GetLimit()),
	}, nil
}

// encodeNext returns the encoder for one Next request: issues carry their
// description only under with_body, as `next --with-body` does, and truncated
// follows the request's limit. Run mode's held reasons and reaped instances
// have no slot in NextResponse and do not cross the wire.
func encodeNext(withBody bool, limit int) func(verb.NextResult) *docketv1.NextResponse {
	return func(r verb.NextResult) *docketv1.NextResponse {
		res := &docketv1.NextResponse{Total: int32(r.Total)}
		count := len(r.Issues)
		for _, issue := range r.Issues {
			res.Issues = append(res.Issues, issueMessage(issue, withBody))
		}
		if r.Steps != nil {
			count = len(r.Steps.Steps)
			for _, row := range r.Steps.Steps {
				res.Steps = append(res.Steps, stepRowMessage(row))
			}
		}
		res.Truncated = output.IsTruncated(limit, r.Total, count)
		return res
	}
}

// issueMessage renders i as model.VersionedIssue's JSON marshaler does: display
// refs, UTC RFC 3339 timestamps, and the lease only while one is held.
func issueMessage(i *model.Issue, withBody bool) *docketv1.Issue {
	m := &docketv1.Issue{
		Id:         model.FormatID(i.ID),
		Title:      i.Title,
		Status:     string(i.Status),
		Priority:   string(i.Priority),
		Kind:       string(i.Kind),
		Assignee:   i.Assignee,
		Labels:     i.Labels,
		Files:      i.Files,
		CreatedAt:  wireTime(i.CreatedAt),
		UpdatedAt:  wireTime(i.UpdatedAt),
		Version:    int32(i.Version),
		Resolution: i.Resolution,
		Size:       string(i.Size),
	}
	if withBody {
		m.Description = i.Description
	}
	if i.ParentID != nil {
		m.ParentId = model.FormatID(*i.ParentID)
	}
	for _, d := range i.Docs {
		m.Docs = append(m.Docs, &docketv1.DocRef{Id: model.FormatDocID(d.ID), Type: d.Type, Title: d.Title, Status: d.Status})
	}
	if i.Lease.Held() {
		m.Lease = &docketv1.Lease{Owner: i.Lease.Owner, ExpiresMs: i.Lease.ExpiresMS, Attempt: int32(i.Lease.Attempt)}
	}
	if i.Scope != nil {
		m.Scope = &docketv1.ScopeGlobs{Globs: *i.Scope}
	}
	return m
}
