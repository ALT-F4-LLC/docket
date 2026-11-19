package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Board lists the caller's project's issues as status columns.
func (s *Service) Board(ctx context.Context, req *connect.Request[docketv1.BoardRequest]) (*connect.Response[docketv1.BoardResponse], error) {
	return unary(ctx, s, req, decodeBoard, verb.Board, encodeBoard(req.Msg.GetWithBody()))
}

// decodeBoard refuses a filter field verb.Board cannot apply: the board shows
// every status and filters no issue kind, so dropping either silently would
// show the caller issues it filtered out.
func decodeBoard(req *docketv1.BoardRequest) (verb.BoardInput, error) {
	f := req.GetFilter()
	if len(f.GetStatus()) > 0 || len(f.GetType()) > 0 {
		return verb.BoardInput{}, errors.New("board filters by priority, label, size and assignee only")
	}
	return verb.BoardInput{
		Labels:     f.GetLabel(),
		Priorities: f.GetPriority(),
		Sizes:      f.GetSize(),
		Assignee:   f.GetAssignee(),
		Expand:     req.GetExpand(),
	}, nil
}

// encodeBoard groups the verb's issues into columns in the board's status
// order, one column per status even when empty. with_body is the encoder's
// alone: it decides only whether descriptions cross the wire, which the verb
// has no part in.
func encodeBoard(withBody bool) func(verb.BoardResult) *docketv1.BoardResponse {
	return func(out verb.BoardResult) *docketv1.BoardResponse {
		groups := make(map[model.Status][]*docketv1.Issue)
		for _, issue := range out.Issues {
			groups[issue.Status] = append(groups[issue.Status], boardIssueMessage(issue, withBody))
		}
		res := &docketv1.BoardResponse{}
		for _, status := range render.StatusOrder {
			res.Columns = append(res.Columns, &docketv1.BoardColumn{Status: string(status), Issues: groups[status]})
		}
		return res
	}
}

// boardIssueMessage is the wire form of one board card. The description
// crosses only when the caller asked for bodies, as the CLI's --with-body does.
func boardIssueMessage(issue *model.Issue, withBody bool) *docketv1.Issue {
	msg := &docketv1.Issue{
		Id:         model.FormatID(issue.ID),
		Title:      issue.Title,
		Status:     string(issue.Status),
		Priority:   string(issue.Priority),
		Kind:       string(issue.Kind),
		Assignee:   issue.Assignee,
		Labels:     issue.Labels,
		Files:      issue.Files,
		CreatedAt:  issue.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:  issue.UpdatedAt.UTC().Format(time.RFC3339),
		Version:    int32(issue.Version),
		Resolution: issue.Resolution,
		Size:       string(issue.Size),
	}
	if withBody {
		msg.Description = issue.Description
	}
	if issue.ParentID != nil {
		msg.ParentId = model.FormatID(*issue.ParentID)
	}
	for _, doc := range issue.Docs {
		msg.Docs = append(msg.Docs, &docketv1.DocRef{
			Id: model.FormatDocID(doc.ID), Type: doc.Type, Title: doc.Title, Status: doc.Status,
		})
	}
	if issue.Lease.Held() {
		msg.Lease = &docketv1.Lease{Owner: issue.Lease.Owner, ExpiresMs: issue.Lease.ExpiresMS, Attempt: int32(issue.Lease.Attempt)}
	}
	if issue.Scope != nil {
		msg.Scope = &docketv1.ScopeGlobs{Globs: *issue.Scope}
	}
	return msg
}
