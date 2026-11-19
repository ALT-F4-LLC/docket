package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func (s *Service) DocCommentAdd(ctx context.Context, req *connect.Request[docketv1.DocCommentAddRequest]) (*connect.Response[docketv1.DocCommentAddResponse], error) {
	return unary(ctx, s, req, decodeDocCommentAdd, verb.DocCommentAdd, encodeDocCommentAdd)
}

func (s *Service) DocCommentList(ctx context.Context, req *connect.Request[docketv1.DocCommentListRequest]) (*connect.Response[docketv1.DocCommentListResponse], error) {
	return unary(ctx, s, req, decodeDocCommentList, verb.DocCommentList, encodeDocCommentList)
}

func (s *Service) DocCreate(ctx context.Context, req *connect.Request[docketv1.DocCreateRequest]) (*connect.Response[docketv1.DocCreateResponse], error) {
	return unary(ctx, s, req, decodeDocCreate, verb.DocCreate, encodeDocCreate)
}

func (s *Service) DocDelete(ctx context.Context, req *connect.Request[docketv1.DocDeleteRequest]) (*connect.Response[docketv1.DocDeleteResponse], error) {
	return unary(ctx, s, req, decodeDocDelete, verb.DocDelete, encodeDocDelete)
}

func (s *Service) DocEdit(ctx context.Context, req *connect.Request[docketv1.DocEditRequest]) (*connect.Response[docketv1.DocEditResponse], error) {
	return unary(ctx, s, req, decodeDocEdit, verb.DocEdit, encodeDocEdit)
}

func (s *Service) DocLinkAdd(ctx context.Context, req *connect.Request[docketv1.DocLinkAddRequest]) (*connect.Response[docketv1.DocLinkAddResponse], error) {
	return unary(ctx, s, req, decodeDocLinkAdd, verb.DocLinkAdd, encodeDocLinkAdd)
}

func (s *Service) DocLinkRemove(ctx context.Context, req *connect.Request[docketv1.DocLinkRemoveRequest]) (*connect.Response[docketv1.DocLinkRemoveResponse], error) {
	return unary(ctx, s, req, decodeDocLinkRemove, verb.DocLinkRemove, encodeDocLinkRemove)
}

func (s *Service) DocList(ctx context.Context, req *connect.Request[docketv1.DocListRequest]) (*connect.Response[docketv1.DocListResponse], error) {
	return unary(ctx, s, req, decodeDocList, verb.DocList, encodeDocList(req.Msg.GetWithBody(), int(req.Msg.GetLimit())))
}

func (s *Service) DocShow(ctx context.Context, req *connect.Request[docketv1.DocShowRequest]) (*connect.Response[docketv1.DocShowResponse], error) {
	return unary(ctx, s, req, decodeDocShow, verb.DocShow, encodeDocShow)
}

func decodeDocCommentAdd(m *docketv1.DocCommentAddRequest) (verb.DocCommentAddInput, error) {
	return verb.DocCommentAddInput{Doc: m.GetDoc(), Body: m.GetMessage()}, nil
}

func encodeDocCommentAdd(r verb.DocCommentAddResult) *docketv1.DocCommentAddResponse {
	return &docketv1.DocCommentAddResponse{Comment: docCommentMessage(r.Comment)}
}

func decodeDocCommentList(m *docketv1.DocCommentListRequest) (verb.DocCommentListInput, error) {
	return verb.DocCommentListInput{Doc: m.GetDoc()}, nil
}

// encodeDocCommentList reports every comment: the verb applies no limit, so
// the list is never truncated.
func encodeDocCommentList(r verb.DocCommentListResult) *docketv1.DocCommentListResponse {
	items := make([]*docketv1.DocComment, 0, len(r.Comments))
	for _, c := range r.Comments {
		items = append(items, docCommentMessage(c))
	}
	return &docketv1.DocCommentListResponse{Items: items, Total: int32(len(items))}
}

func decodeDocCreate(m *docketv1.DocCreateRequest) (verb.DocCreateInput, error) {
	return verb.DocCreateInput{Title: m.GetTitle(), Body: m.GetDescription(), Type: m.GetType(), Status: m.GetStatus()}, nil
}

func encodeDocCreate(r verb.DocCreateResult) *docketv1.DocCreateResponse {
	return &docketv1.DocCreateResponse{Doc: docMessage(r.Doc, true)}
}

func decodeDocDelete(m *docketv1.DocDeleteRequest) (verb.DocDeleteInput, error) {
	return verb.DocDeleteInput{Doc: m.GetDoc(), Cascade: m.GetCascade(), Force: m.GetForce()}, nil
}

// encodeDocDelete names the doc deleted or, on the refusal without force, the
// doc that would be.
func encodeDocDelete(r verb.DocDeleteResult) *docketv1.DocDeleteResponse {
	res := &docketv1.DocDeleteResponse{}
	if r.Doc != nil {
		res.Id = model.FormatDocID(r.Doc.ID)
	}
	return res
}

func decodeDocEdit(m *docketv1.DocEditRequest) (verb.DocEditInput, error) {
	return verb.DocEditInput{Doc: m.GetDoc(), Title: m.Title, Body: m.Description, Type: m.Type, Status: m.Status}, nil
}

func encodeDocEdit(r verb.DocEditResult) *docketv1.DocEditResponse {
	return &docketv1.DocEditResponse{Doc: docMessage(r.Doc, true)}
}

func decodeDocLinkAdd(m *docketv1.DocLinkAddRequest) (verb.DocLinkAddInput, error) {
	return verb.DocLinkAddInput{Doc: m.GetDoc(), Issue: m.GetIssue()}, nil
}

func encodeDocLinkAdd(r verb.DocLinkAddResult) *docketv1.DocLinkAddResponse {
	return &docketv1.DocLinkAddResponse{Doc: model.FormatDocID(r.DocID), Issue: model.FormatID(r.IssueID)}
}

func decodeDocLinkRemove(m *docketv1.DocLinkRemoveRequest) (verb.DocLinkRemoveInput, error) {
	return verb.DocLinkRemoveInput{Doc: m.GetDoc(), Issue: m.GetIssue()}, nil
}

func encodeDocLinkRemove(r verb.DocLinkRemoveResult) *docketv1.DocLinkRemoveResponse {
	return &docketv1.DocLinkRemoveResponse{Doc: model.FormatDocID(r.DocID), Issue: model.FormatID(r.IssueID)}
}

func decodeDocList(m *docketv1.DocListRequest) (verb.DocListInput, error) {
	return verb.DocListInput{
		Types: m.GetType(), Statuses: m.GetStatus(), Author: m.GetAuthor(), Sort: m.GetSort(), Limit: int(m.GetLimit()),
	}, nil
}

// encodeDocList returns the encoder for one DocList request: rows carry their
// body only under with_body, as `doc list --with-body` does, and truncated
// follows the request's limit.
func encodeDocList(withBody bool, limit int) func(verb.DocListResult) *docketv1.DocListResponse {
	return func(r verb.DocListResult) *docketv1.DocListResponse {
		items := make([]*docketv1.Doc, 0, len(r.Docs))
		for _, s := range r.Docs {
			items = append(items, docMessage(s.Doc, withBody))
		}
		return &docketv1.DocListResponse{
			Items:     items,
			Total:     int32(r.Total),
			Truncated: output.IsTruncated(limit, r.Total, len(items)),
		}
	}
}

func decodeDocShow(m *docketv1.DocShowRequest) (verb.DocShowInput, error) {
	in := verb.DocShowInput{Doc: m.GetDoc()}
	if m.Rev != nil {
		rev := int(m.GetRev())
		in.Rev = &rev
	}
	return in, nil
}

// encodeDocShow returns the doc with its revisions and linked issues, or, when
// the request named a revision, a doc carrying only its id and that revision.
func encodeDocShow(r verb.DocShowResult) *docketv1.DocShowResponse {
	if r.Revision != nil {
		return &docketv1.DocShowResponse{Doc: &docketv1.Doc{
			Id:        model.FormatDocID(r.Revision.DocID),
			Revisions: []*docketv1.DocRevision{docRevisionMessage(r.Revision)},
		}}
	}
	doc := docMessage(r.Doc, true)
	if doc == nil {
		return &docketv1.DocShowResponse{}
	}
	for _, rev := range r.Revisions {
		doc.Revisions = append(doc.Revisions, docRevisionMessage(rev))
	}
	for _, issue := range r.LinkedIssues {
		doc.LinkedIssues = append(doc.LinkedIssues, model.FormatID(issue.ID))
	}
	return &docketv1.DocShowResponse{Doc: doc}
}

// docMessage renders d as model.Doc's JSON marshaler does: display ref and UTC
// RFC 3339 timestamps. A nil doc, as a partial result may carry, is nil.
func docMessage(d *model.Doc, withBody bool) *docketv1.Doc {
	if d == nil {
		return nil
	}
	m := &docketv1.Doc{
		Id:        model.FormatDocID(d.ID),
		Type:      d.Type,
		Status:    d.Status,
		Title:     d.Title,
		Author:    d.Author,
		CreatedAt: wireTime(d.CreatedAt),
		UpdatedAt: wireTime(d.UpdatedAt),
	}
	if withBody {
		m.Body = d.Body
	}
	return m
}

func docRevisionMessage(r *model.DocRevision) *docketv1.DocRevision {
	return &docketv1.DocRevision{
		Id:             int32(r.ID),
		DocId:          model.FormatDocID(r.DocID),
		RevisionNumber: int32(r.RevisionNumber),
		Body:           r.Body,
		ChangeKind:     r.ChangeKind,
		Author:         r.Author,
		CreatedAt:      wireTime(r.CreatedAt),
	}
}

func docCommentMessage(c *model.DocComment) *docketv1.DocComment {
	if c == nil {
		return nil
	}
	return &docketv1.DocComment{
		Id:        int32(c.ID),
		DocId:     model.FormatDocID(c.DocID),
		Body:      c.Body,
		Author:    c.Author,
		CreatedAt: wireTime(c.CreatedAt),
	}
}

func wireTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
