package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Plan groups the caller's open issues into phases that can each be worked
// in parallel.
func (s *Service) Plan(ctx context.Context, req *connect.Request[docketv1.PlanRequest]) (*connect.Response[docketv1.PlanResponse], error) {
	return unary(ctx, s, req, decodePlan, verb.Plan, encodePlan)
}

// decodePlan ignores with_body: it is a rendering choice, so the response
// always carries each description and the client decides whether to show it.
func decodePlan(req *docketv1.PlanRequest) (verb.PlanInput, error) {
	f := req.GetFilter()
	return verb.PlanInput{
		Statuses:   f.GetStatus(),
		Labels:     f.GetLabel(),
		Priorities: f.GetPriority(),
		Sizes:      f.GetSize(),
		Types:      f.GetType(),
		Assignee:   f.GetAssignee(),
		Root:       req.GetRoot(),
	}, nil
}

// encodePlan lists, in blocked, the planned issues that at least one other
// issue blocks, in plan order.
func encodePlan(res verb.PlanResult) *docketv1.PlanResponse {
	out := &docketv1.PlanResponse{}
	if res.Plan == nil {
		return out
	}
	for _, phase := range res.Plan.Phases {
		issues := make([]*docketv1.Issue, 0, len(phase.Issues))
		for _, issue := range phase.Issues {
			issues = append(issues, planIssueMessage(issue))
			if len(res.BlockedBy[issue.ID]) > 0 {
				out.Blocked = append(out.Blocked, model.FormatID(issue.ID))
			}
		}
		out.Phases = append(out.Phases, &docketv1.PlanPhase{Phase: int32(phase.Number), Issues: issues})
	}
	return out
}

// planIssueMessage is i on the wire, with timestamps in the CLI's RFC 3339
// UTC form; lease and scope are present only when held and declared.
func planIssueMessage(i *model.Issue) *docketv1.Issue {
	msg := &docketv1.Issue{
		Id:          model.FormatID(i.ID),
		Title:       i.Title,
		Description: i.Description,
		Status:      string(i.Status),
		Priority:    string(i.Priority),
		Kind:        string(i.Kind),
		Assignee:    i.Assignee,
		Labels:      i.Labels,
		Files:       i.Files,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   i.UpdatedAt.UTC().Format(time.RFC3339),
		Version:     int32(i.Version),
		Resolution:  i.Resolution,
		Size:        string(i.Size),
	}
	if i.ParentID != nil {
		msg.ParentId = model.FormatID(*i.ParentID)
	}
	for _, d := range i.Docs {
		msg.Docs = append(msg.Docs, &docketv1.DocRef{Id: model.FormatDocID(d.ID), Type: d.Type, Title: d.Title, Status: d.Status})
	}
	if i.Lease.Held() {
		msg.Lease = &docketv1.Lease{Owner: i.Lease.Owner, ExpiresMs: i.Lease.ExpiresMS, Attempt: int32(i.Lease.Attempt)}
	}
	if i.Scope != nil {
		msg.Scope = &docketv1.ScopeGlobs{Globs: *i.Scope}
	}
	return msg
}
