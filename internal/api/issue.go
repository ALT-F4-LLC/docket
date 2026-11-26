package api

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// IssueClaim takes the lease on an issue and mints its capability token.
func (s *Service) IssueClaim(ctx context.Context, req *connect.Request[docketv1.IssueClaimRequest]) (*connect.Response[docketv1.IssueClaimResponse], error) {
	return unary(ctx, s, req, decodeIssueClaim, verb.IssueClaim, encodeIssueClaim)
}

// IssueClose moves an issue to done, presenting the request's token while a
// live lease is held.
func (s *Service) IssueClose(ctx context.Context, req *connect.Request[docketv1.IssueCloseRequest]) (*connect.Response[docketv1.IssueCloseResponse], error) {
	return unary(ctx, s, req, decodeIssueClose, verb.IssueClose, encodeIssueClose)
}

// IssueCommentAdd adds a comment to an issue.
func (s *Service) IssueCommentAdd(ctx context.Context, req *connect.Request[docketv1.IssueCommentAddRequest]) (*connect.Response[docketv1.IssueCommentAddResponse], error) {
	return unary(ctx, s, req, decodeIssueCommentAdd, verb.IssueCommentAdd, encodeIssueCommentAdd)
}

// IssueCommentList lists an issue's comments.
func (s *Service) IssueCommentList(ctx context.Context, req *connect.Request[docketv1.IssueCommentListRequest]) (*connect.Response[docketv1.IssueCommentListResponse], error) {
	return unary(ctx, s, req, decodeIssueCommentList, verb.IssueCommentList, encodeIssueCommentList)
}

// IssueCreate creates an issue in the caller's project.
func (s *Service) IssueCreate(ctx context.Context, req *connect.Request[docketv1.IssueCreateRequest]) (*connect.Response[docketv1.IssueCreateResponse], error) {
	return unary(ctx, s, req, decodeIssueCreate, verb.IssueCreate, encodeIssueCreate)
}

// IssueDelete deletes an issue, cascading to or orphaning its sub-issues.
func (s *Service) IssueDelete(ctx context.Context, req *connect.Request[docketv1.IssueDeleteRequest]) (*connect.Response[docketv1.IssueDeleteResponse], error) {
	return unary(ctx, s, req, decodeIssueDelete, verb.IssueDelete, encodeIssueDelete)
}

// IssueEdit changes an issue's fields and files.
func (s *Service) IssueEdit(ctx context.Context, req *connect.Request[docketv1.IssueEditRequest]) (*connect.Response[docketv1.IssueEditResponse], error) {
	return unary(ctx, s, req, decodeIssueEdit, verb.IssueEdit, encodeIssueEdit)
}

// IssueFileAdd attaches files to an issue.
func (s *Service) IssueFileAdd(ctx context.Context, req *connect.Request[docketv1.IssueFileAddRequest]) (*connect.Response[docketv1.IssueFileAddResponse], error) {
	return unary(ctx, s, req, decodeIssueFileAdd, verb.IssueFileAdd, encodeIssueFileAdd)
}

// IssueFileList lists the files attached to an issue.
func (s *Service) IssueFileList(ctx context.Context, req *connect.Request[docketv1.IssueFileListRequest]) (*connect.Response[docketv1.IssueFileListResponse], error) {
	return unary(ctx, s, req, decodeIssueFileList, verb.IssueFileList, encodeIssueFileList)
}

// IssueFileRemove detaches files from an issue.
func (s *Service) IssueFileRemove(ctx context.Context, req *connect.Request[docketv1.IssueFileRemoveRequest]) (*connect.Response[docketv1.IssueFileRemoveResponse], error) {
	return unary(ctx, s, req, decodeIssueFileRemove, verb.IssueFileRemove, encodeIssueFileRemove)
}

// IssueGraph walks the dependency graph from one issue.
func (s *Service) IssueGraph(ctx context.Context, req *connect.Request[docketv1.IssueGraphRequest]) (*connect.Response[docketv1.IssueGraphResponse], error) {
	return unary(ctx, s, req, decodeIssueGraph, verb.IssueGraph, encodeIssueGraph)
}

// IssueHeartbeat extends the lease the request's token holds.
func (s *Service) IssueHeartbeat(ctx context.Context, req *connect.Request[docketv1.IssueHeartbeatRequest]) (*connect.Response[docketv1.IssueHeartbeatResponse], error) {
	return unary(ctx, s, req, decodeIssueHeartbeat, verb.IssueHeartbeat, encodeIssueHeartbeat)
}

// IssueLabelAdd attaches labels to an issue, creating any that do not exist.
func (s *Service) IssueLabelAdd(ctx context.Context, req *connect.Request[docketv1.IssueLabelAddRequest]) (*connect.Response[docketv1.IssueLabelAddResponse], error) {
	return unary(ctx, s, req, decodeIssueLabelAdd, verb.IssueLabelAdd, encodeIssueLabelAdd)
}

// IssueLabelDelete deletes a label from the caller's project.
func (s *Service) IssueLabelDelete(ctx context.Context, req *connect.Request[docketv1.IssueLabelDeleteRequest]) (*connect.Response[docketv1.IssueLabelDeleteResponse], error) {
	return unary(ctx, s, req, decodeIssueLabelDelete, verb.IssueLabelDelete, encodeIssueLabelDelete)
}

// IssueLabelList lists the caller's project's labels.
func (s *Service) IssueLabelList(ctx context.Context, req *connect.Request[docketv1.IssueLabelListRequest]) (*connect.Response[docketv1.IssueLabelListResponse], error) {
	return unary(ctx, s, req, decodeIssueLabelList, verb.IssueLabelList, encodeIssueLabelList)
}

// IssueLabelRm detaches labels from an issue.
func (s *Service) IssueLabelRm(ctx context.Context, req *connect.Request[docketv1.IssueLabelRmRequest]) (*connect.Response[docketv1.IssueLabelRmResponse], error) {
	return unary(ctx, s, req, decodeIssueLabelRm, verb.IssueLabelRm, encodeIssueLabelRm)
}

// IssueLinkAdd creates a relation between two issues.
func (s *Service) IssueLinkAdd(ctx context.Context, req *connect.Request[docketv1.IssueLinkAddRequest]) (*connect.Response[docketv1.IssueLinkAddResponse], error) {
	return unary(ctx, s, req, decodeIssueLinkAdd, verb.IssueLinkAdd, encodeIssueLinkAdd)
}

// IssueLinkList lists one issue's relations.
func (s *Service) IssueLinkList(ctx context.Context, req *connect.Request[docketv1.IssueLinkListRequest]) (*connect.Response[docketv1.IssueLinkListResponse], error) {
	return unary(ctx, s, req, decodeIssueLinkList, verb.IssueLinkList, encodeIssueLinkList)
}

// IssueLinkRemove removes a relation between two issues.
func (s *Service) IssueLinkRemove(ctx context.Context, req *connect.Request[docketv1.IssueLinkRemoveRequest]) (*connect.Response[docketv1.IssueLinkRemoveResponse], error) {
	return unary(ctx, s, req, decodeIssueLinkRemove, verb.IssueLinkRemove, encodeIssueLinkRemove)
}

// IssueList lists issues.
func (s *Service) IssueList(ctx context.Context, req *connect.Request[docketv1.IssueListRequest]) (*connect.Response[docketv1.IssueListResponse], error) {
	return unary(ctx, s, req, decodeIssueList, verb.IssueList, encodeIssueList(req.Msg.GetWithBody(), int(req.Msg.GetLimit())))
}

// IssueLog reads an issue's activity history.
func (s *Service) IssueLog(ctx context.Context, req *connect.Request[docketv1.IssueLogRequest]) (*connect.Response[docketv1.IssueLogResponse], error) {
	return unary(ctx, s, req, decodeIssueLog, verb.IssueLog, encodeIssueLog)
}

// IssueMove moves an issue to a new status or migrates it to another project.
func (s *Service) IssueMove(ctx context.Context, req *connect.Request[docketv1.IssueMoveRequest]) (*connect.Response[docketv1.IssueMoveResponse], error) {
	return unary(ctx, s, req, decodeIssueMove, verb.IssueMove, encodeIssueMove)
}

// IssueRelease ends the lease the request's token holds.
func (s *Service) IssueRelease(ctx context.Context, req *connect.Request[docketv1.IssueReleaseRequest]) (*connect.Response[docketv1.IssueReleaseResponse], error) {
	return unary(ctx, s, req, decodeIssueRelease, verb.IssueRelease, encodeIssueRelease)
}

// IssueReopen moves a closed issue back to backlog.
func (s *Service) IssueReopen(ctx context.Context, req *connect.Request[docketv1.IssueReopenRequest]) (*connect.Response[docketv1.IssueReopenResponse], error) {
	return unary(ctx, s, req, decodeIssueReopen, verb.IssueReopen, encodeIssueReopen)
}

// IssueShow reads issues at full detail.
func (s *Service) IssueShow(ctx context.Context, req *connect.Request[docketv1.IssueShowRequest]) (*connect.Response[docketv1.IssueShowResponse], error) {
	return unary(ctx, s, req, decodeIssueShow, verb.IssueShow, encodeIssueShow)
}

// optionalTTL is a request's ttl_ms as the verb's TTL: nil when unset, so the
// verb takes the configured TTL.
func optionalTTL(ms *int64) *time.Duration {
	if ms == nil {
		return nil
	}
	ttl := time.Duration(*ms) * time.Millisecond
	return &ttl
}

func decodeIssueClaim(req *docketv1.IssueClaimRequest) (verb.IssueClaimInput, error) {
	return verb.IssueClaimInput{
		Issue: req.GetIssue(), Owner: req.GetOwner(), TTL: optionalTTL(req.TtlMs), Class: req.GetClass(),
	}, nil
}

func encodeIssueClaim(res verb.IssueClaimResult) *docketv1.IssueClaimResponse {
	if res.Token == "" {
		return &docketv1.IssueClaimResponse{}
	}
	return &docketv1.IssueClaimResponse{
		Issue: model.FormatID(res.Issue), Token: res.Token, LeaseExpiresMs: res.Lease.ExpiresMS,
		Attempt: int32(res.Lease.Attempt), Version: int32(res.Version),
	}
}

// decodeIssueClose hands the verb the request's token; the verb reads it only
// while the issue holds a live lease.
func decodeIssueClose(req *docketv1.IssueCloseRequest) (verb.IssueCloseInput, error) {
	token := req.GetToken()
	return verb.IssueCloseInput{Issue: req.GetIssue(), Token: func() string { return token }}, nil
}

func encodeIssueClose(res verb.IssueCloseResult) *docketv1.IssueCloseResponse {
	return &docketv1.IssueCloseResponse{Issue: fullIssueMessage(res.Issue)}
}

func decodeIssueCommentAdd(req *docketv1.IssueCommentAddRequest) (verb.IssueCommentAddInput, error) {
	return verb.IssueCommentAddInput{Issue: req.GetIssue(), Body: req.GetMessage()}, nil
}

func encodeIssueCommentAdd(res verb.IssueCommentAddResult) *docketv1.IssueCommentAddResponse {
	return &docketv1.IssueCommentAddResponse{Comment: commentMessage(res.Comment)}
}

func decodeIssueCommentList(req *docketv1.IssueCommentListRequest) (verb.IssueCommentListInput, error) {
	return verb.IssueCommentListInput{Issue: req.GetIssue()}, nil
}

func encodeIssueCommentList(res verb.IssueCommentListResult) *docketv1.IssueCommentListResponse {
	items := make([]*docketv1.Comment, 0, len(res.Comments))
	for _, c := range res.Comments {
		items = append(items, commentMessage(c))
	}
	return &docketv1.IssueCommentListResponse{Items: items, Total: int32(len(items))}
}

func decodeIssueCreate(req *docketv1.IssueCreateRequest) (verb.IssueCreateInput, error) {
	return verb.IssueCreateInput{
		Title: req.GetTitle(), Description: req.GetDescription(), Status: req.GetStatus(),
		Priority: req.GetPriority(), Size: req.GetSize(), Kind: req.GetType(),
		Assignee: req.GetAssignee(), Parent: req.GetParent(), Labels: req.GetLabel(), Files: req.GetFile(),
	}, nil
}

func encodeIssueCreate(res verb.IssueCreateResult) *docketv1.IssueCreateResponse {
	return &docketv1.IssueCreateResponse{Issue: fullIssueMessage(res.Issue)}
}

// decodeIssueDelete reads force as cascade, as the CLI's --force does.
func decodeIssueDelete(req *docketv1.IssueDeleteRequest) (verb.IssueDeleteInput, error) {
	return verb.IssueDeleteInput{
		Issue: req.GetIssue(), Cascade: req.GetCascade() || req.GetForce(), Orphan: req.GetOrphan(),
	}, nil
}

// encodeIssueDelete names the deleted issue only: the verb counts the
// sub-issues it cascaded to or orphaned but does not name them.
func encodeIssueDelete(res verb.IssueDeleteResult) *docketv1.IssueDeleteResponse {
	if res.Issue == "" {
		return &docketv1.IssueDeleteResponse{}
	}
	return &docketv1.IssueDeleteResponse{Deleted: []string{res.Issue}}
}

// decodeIssueEdit replaces the issue's files only when file is non-empty: a
// repeated field cannot say "set to none" apart from "not given".
func decodeIssueEdit(req *docketv1.IssueEditRequest) (verb.IssueEditInput, error) {
	in := verb.IssueEditInput{
		Issue: req.GetIssue(), Title: req.Title, Description: req.Description, Status: req.Status,
		Priority: req.Priority, Size: req.Size, Kind: req.Type, Assignee: req.Assignee, Parent: req.Parent,
	}
	if files := req.GetFile(); len(files) > 0 {
		in.Files = &files
	}
	return in, nil
}

func encodeIssueEdit(res verb.IssueEditResult) *docketv1.IssueEditResponse {
	return &docketv1.IssueEditResponse{Issue: fullIssueMessage(res.Issue)}
}

func decodeIssueFileAdd(req *docketv1.IssueFileAddRequest) (verb.IssueFileAddInput, error) {
	return verb.IssueFileAddInput{Issue: req.GetIssue(), Paths: req.GetPaths()}, nil
}

func encodeIssueFileAdd(res verb.IssueFileAddResult) *docketv1.IssueFileAddResponse {
	if res.Issue == nil {
		return &docketv1.IssueFileAddResponse{}
	}
	return &docketv1.IssueFileAddResponse{Issue: model.FormatID(res.Issue.ID), Files: res.Files}
}

func decodeIssueFileList(req *docketv1.IssueFileListRequest) (verb.IssueFileListInput, error) {
	return verb.IssueFileListInput{Issue: req.GetIssue()}, nil
}

func encodeIssueFileList(res verb.IssueFileListResult) *docketv1.IssueFileListResponse {
	return &docketv1.IssueFileListResponse{Issue: model.FormatID(res.IssueID), Files: res.Files}
}

func decodeIssueFileRemove(req *docketv1.IssueFileRemoveRequest) (verb.IssueFileRemoveInput, error) {
	return verb.IssueFileRemoveInput{Issue: req.GetIssue(), Paths: req.GetPaths()}, nil
}

func encodeIssueFileRemove(res verb.IssueFileRemoveResult) *docketv1.IssueFileRemoveResponse {
	if res.Issue == nil {
		return &docketv1.IssueFileRemoveResponse{}
	}
	return &docketv1.IssueFileRemoveResponse{Issue: model.FormatID(res.Issue.ID), Files: res.Files}
}

// decodeIssueGraph refuses mermaid: the daemon sends no rendering, and the
// client draws the flowchart from the nodes and edges.
func decodeIssueGraph(req *docketv1.IssueGraphRequest) (verb.IssueGraphInput, error) {
	if req.GetMermaid() {
		return verb.IssueGraphInput{}, errors.New("mermaid is not served: the client renders it from nodes and edges")
	}
	return verb.IssueGraphInput{Issue: req.GetIssue(), Direction: req.GetDirection(), Depth: int(req.GetDepth())}, nil
}

// encodeIssueGraph lists nodes by issue id. The walk's adjacency maps have no
// slot and do not cross the wire.
func encodeIssueGraph(res verb.IssueGraphResult) *docketv1.IssueGraphResponse {
	if res.Issue == nil {
		return &docketv1.IssueGraphResponse{}
	}
	msg := &docketv1.IssueGraphResponse{Root: model.FormatID(res.Issue.ID)}
	nodes := slices.Collect(maps.Values(res.Issues))
	slices.SortFunc(nodes, func(a, b *model.Issue) int { return cmp.Compare(a.ID, b.ID) })
	for _, issue := range nodes {
		msg.Nodes = append(msg.Nodes, issueMessage(issue, false))
	}
	for _, e := range res.Edges {
		msg.Edges = append(msg.Edges, &docketv1.GraphEdge{
			Source: model.FormatID(e.From), Target: model.FormatID(e.To), RelationType: e.Type,
		})
	}
	return msg
}

func decodeIssueHeartbeat(req *docketv1.IssueHeartbeatRequest) (verb.IssueHeartbeatInput, error) {
	return verb.IssueHeartbeatInput{
		Issue: req.GetIssue(), Token: req.GetToken(), TTL: optionalTTL(req.TtlMs), Class: req.GetClass(),
	}, nil
}

func encodeIssueHeartbeat(res verb.IssueHeartbeatResult) *docketv1.IssueHeartbeatResponse {
	if res.Issue == 0 {
		return &docketv1.IssueHeartbeatResponse{}
	}
	return &docketv1.IssueHeartbeatResponse{Issue: model.FormatID(res.Issue), LeaseExpiresMs: res.Lease.ExpiresMS}
}

func decodeIssueLabelAdd(req *docketv1.IssueLabelAddRequest) (verb.IssueLabelAddInput, error) {
	return verb.IssueLabelAddInput{Issue: req.GetIssue(), Labels: req.GetLabels(), Color: req.GetColor()}, nil
}

func encodeIssueLabelAdd(res verb.IssueLabelAddResult) *docketv1.IssueLabelAddResponse {
	return &docketv1.IssueLabelAddResponse{Issue: labeledIssueMessage(res.Issue, res.Labels)}
}

func decodeIssueLabelDelete(req *docketv1.IssueLabelDeleteRequest) (verb.IssueLabelDeleteInput, error) {
	return verb.IssueLabelDeleteInput{Label: req.GetLabel(), Force: req.GetForce()}, nil
}

func encodeIssueLabelDelete(res verb.IssueLabelDeleteResult) *docketv1.IssueLabelDeleteResponse {
	if res.Label == nil {
		return &docketv1.IssueLabelDeleteResponse{}
	}
	return &docketv1.IssueLabelDeleteResponse{Label: res.Label.Name, Detached: int32(res.Detached)}
}

func decodeIssueLabelList(*docketv1.IssueLabelListRequest) (verb.IssueLabelListInput, error) {
	return verb.IssueLabelListInput{}, nil
}

func encodeIssueLabelList(res verb.IssueLabelListResult) *docketv1.IssueLabelListResponse {
	items := make([]*docketv1.Label, 0, len(res.Labels))
	for _, l := range res.Labels {
		items = append(items, &docketv1.Label{
			Id: int32(l.ID), Name: l.Name, Color: l.Color, IssueCount: int32(l.IssueCount),
		})
	}
	return &docketv1.IssueLabelListResponse{Items: items, Total: int32(len(items))}
}

func decodeIssueLabelRm(req *docketv1.IssueLabelRmRequest) (verb.IssueLabelRmInput, error) {
	return verb.IssueLabelRmInput{Issue: req.GetIssue(), Labels: req.GetLabels()}, nil
}

func encodeIssueLabelRm(res verb.IssueLabelRmResult) *docketv1.IssueLabelRmResponse {
	return &docketv1.IssueLabelRmResponse{Issue: labeledIssueMessage(res.Issue, res.Labels)}
}

func decodeIssueLinkAdd(req *docketv1.IssueLinkAddRequest) (verb.IssueLinkAddInput, error) {
	return verb.IssueLinkAddInput{Issue: req.GetIssue(), Relation: req.GetRelation(), Target: req.GetTarget()}, nil
}

func encodeIssueLinkAdd(res verb.IssueLinkAddResult) *docketv1.IssueLinkAddResponse {
	return &docketv1.IssueLinkAddResponse{Relation: relationMessage(res.Relation)}
}

func decodeIssueLinkList(req *docketv1.IssueLinkListRequest) (verb.IssueLinkListInput, error) {
	return verb.IssueLinkListInput{Issue: req.GetIssue()}, nil
}

func encodeIssueLinkList(res verb.IssueLinkListResult) *docketv1.IssueLinkListResponse {
	items := make([]*docketv1.Relation, 0, len(res.Relations))
	for i := range res.Relations {
		items = append(items, relationMessage(&res.Relations[i]))
	}
	return &docketv1.IssueLinkListResponse{Items: items, Total: int32(len(items))}
}

func decodeIssueLinkRemove(req *docketv1.IssueLinkRemoveRequest) (verb.IssueLinkRemoveInput, error) {
	return verb.IssueLinkRemoveInput{Issue: req.GetIssue(), Relation: req.GetRelation(), Target: req.GetTarget()}, nil
}

func encodeIssueLinkRemove(res verb.IssueLinkRemoveResult) *docketv1.IssueLinkRemoveResponse {
	return &docketv1.IssueLinkRemoveResponse{Relation: relationMessage(res.Relation)}
}

// decodeIssueList ignores tree, which only shapes the client's table; the
// listing is the same either way.
func decodeIssueList(req *docketv1.IssueListRequest) (verb.IssueListInput, error) {
	f := req.GetFilter()
	return verb.IssueListInput{
		Statuses: f.GetStatus(), Priorities: f.GetPriority(), Sizes: f.GetSize(),
		Labels: f.GetLabel(), Types: f.GetType(), Assignee: f.GetAssignee(),
		Parent: req.GetParent(), RootsOnly: req.GetRoots(), All: req.GetAll(),
		Sort: req.GetSort(), Limit: int(req.GetLimit()),
	}, nil
}

// encodeIssueList returns the encoder for one IssueList request: rows carry
// their description only under with_body, and truncated follows the request's
// limit.
func encodeIssueList(withBody bool, limit int) func(verb.IssueListResult) *docketv1.IssueListResponse {
	return func(res verb.IssueListResult) *docketv1.IssueListResponse {
		items := make([]*docketv1.Issue, 0, len(res.Issues))
		for _, issue := range res.Issues {
			items = append(items, issueMessage(issue, withBody))
		}
		return &docketv1.IssueListResponse{
			Items: items, Total: int32(res.Total), Truncated: output.IsTruncated(limit, res.Total, len(items)),
		}
	}
}

func decodeIssueLog(req *docketv1.IssueLogRequest) (verb.IssueLogInput, error) {
	return verb.IssueLogInput{Issue: req.GetIssue(), Limit: int(req.GetLimit())}, nil
}

func encodeIssueLog(res verb.IssueLogResult) *docketv1.IssueLogResponse {
	items := make([]*docketv1.Activity, 0, len(res.Entries))
	for _, a := range res.Entries {
		items = append(items, activityMessage(a))
	}
	return &docketv1.IssueLogResponse{
		Items: items, Total: int32(res.Total), Truncated: output.IsTruncated(res.Limit, res.Total, len(items)),
	}
}

// decodeIssueMove reads an empty status as none given, since proto3 cannot
// tell the two apart.
func decodeIssueMove(req *docketv1.IssueMoveRequest) (verb.IssueMoveInput, error) {
	in := verb.IssueMoveInput{Issue: req.GetIssue(), Project: req.GetProject(), Note: req.GetNote()}
	if status := req.GetStatus(); status != "" {
		in.Status = &status
	}
	return in, nil
}

// encodeIssueMove carries the moved issue. A project migration sets no issue:
// IssueMoveResponse has no slot for the migrated subtree.
func encodeIssueMove(res verb.IssueMoveResult) *docketv1.IssueMoveResponse {
	return &docketv1.IssueMoveResponse{Issue: fullIssueMessage(res.Issue)}
}

func decodeIssueRelease(req *docketv1.IssueReleaseRequest) (verb.IssueReleaseInput, error) {
	return verb.IssueReleaseInput{Issue: req.GetIssue(), Token: req.GetToken()}, nil
}

func encodeIssueRelease(res verb.IssueReleaseResult) *docketv1.IssueReleaseResponse {
	if res.Issue == 0 {
		return &docketv1.IssueReleaseResponse{}
	}
	return &docketv1.IssueReleaseResponse{Issue: model.FormatID(res.Issue)}
}

func decodeIssueReopen(req *docketv1.IssueReopenRequest) (verb.IssueReopenInput, error) {
	return verb.IssueReopenInput{Issue: req.GetIssue()}, nil
}

func encodeIssueReopen(res verb.IssueReopenResult) *docketv1.IssueReopenResponse {
	return &docketv1.IssueReopenResponse{Issue: fullIssueMessage(res.Issue)}
}

func decodeIssueShow(req *docketv1.IssueShowRequest) (verb.IssueShowInput, error) {
	return verb.IssueShowInput{Issues: req.GetIssues()}, nil
}

// encodeIssueShow carries each detail's issue with its declared scope. Linked
// proposals and activity have no slot in IssueShowDetail and do not cross the
// wire.
func encodeIssueShow(res verb.IssueShowResult) *docketv1.IssueShowResponse {
	msg := &docketv1.IssueShowResponse{}
	for _, d := range res.Issues {
		detail := &docketv1.IssueShowDetail{Issue: fullIssueMessage(d.Issue)}
		if d.ScopeDeclared && detail.Issue != nil {
			detail.Issue.Scope = &docketv1.ScopeGlobs{Globs: d.Scope}
		}
		for _, sub := range d.SubIssues {
			detail.Children = append(detail.Children, issueMessage(sub, false))
		}
		for i := range d.Relations {
			detail.Relations = append(detail.Relations, relationMessage(&d.Relations[i]))
		}
		for _, c := range d.Comments {
			detail.Comments = append(detail.Comments, commentMessage(c))
		}
		if r := d.RunDisposition; r != nil {
			detail.Disposition = &docketv1.IssueRunDisposition{
				RunId: model.FormatRunID(r.RunID), Disposition: r.Disposition, By: r.By, Reason: r.Reason, AtMs: r.AtMS,
			}
		}
		msg.Issues = append(msg.Issues, detail)
	}
	return msg
}

// fullIssueMessage is a single-issue result on the wire, description
// included; nil when the verb returned no issue.
func fullIssueMessage(issue *model.Issue) *docketv1.Issue {
	if issue == nil {
		return nil
	}
	return issueMessage(issue, true)
}

// labeledIssueMessage is a label verb's issue, as read before the write, with
// the label set after it. Its files and docs are not read and stay empty.
func labeledIssueMessage(issue *model.Issue, labels []*model.Label) *docketv1.Issue {
	msg := fullIssueMessage(issue)
	if msg == nil {
		return nil
	}
	msg.Labels = nil
	for _, l := range labels {
		msg.Labels = append(msg.Labels, l.Name)
	}
	return msg
}

func commentMessage(c *model.Comment) *docketv1.Comment {
	if c == nil {
		return nil
	}
	return &docketv1.Comment{
		Id: int32(c.ID), IssueId: model.FormatID(c.IssueID), Body: c.Body, Author: c.Author, CreatedAt: wireTime(c.CreatedAt),
	}
}

func activityMessage(a model.Activity) *docketv1.Activity {
	return &docketv1.Activity{
		Id: int32(a.ID), IssueId: model.FormatID(a.IssueID), FieldChanged: a.FieldChanged,
		OldValue: a.OldValue, NewValue: a.NewValue, ChangedBy: a.ChangedBy, CreatedAt: wireTime(a.CreatedAt),
	}
}

// relationMessage is r on the wire. A removed relation has no row id or
// creation time, so both stay unset.
func relationMessage(r *model.Relation) *docketv1.Relation {
	if r == nil {
		return nil
	}
	msg := &docketv1.Relation{
		Id: int32(r.ID), SourceIssueId: model.FormatID(r.SourceIssueID),
		TargetIssueId: model.FormatID(r.TargetIssueID), RelationType: string(r.RelationType),
	}
	if !r.CreatedAt.IsZero() {
		msg.CreatedAt = wireTime(r.CreatedAt)
	}
	return msg
}
