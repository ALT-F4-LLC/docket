package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// WorkflowDeprecate retires one registered workflow version from binding in
// the caller's project.
func (s *Service) WorkflowDeprecate(ctx context.Context, req *connect.Request[docketv1.WorkflowDeprecateRequest]) (*connect.Response[docketv1.WorkflowDeprecateResponse], error) {
	return unary(ctx, s, req, decodeWorkflowDeprecate, verb.WorkflowDeprecate, encodeWorkflowDeprecate)
}

// WorkflowLint runs a register's validation over the request's definition and
// writes nothing.
func (s *Service) WorkflowLint(ctx context.Context, req *connect.Request[docketv1.WorkflowLintRequest]) (*connect.Response[docketv1.WorkflowLintResponse], error) {
	return unary(ctx, s, req, decodeWorkflowLint, verb.WorkflowLint, encodeWorkflowLint)
}

// WorkflowList lists the workflows registered in the caller's project.
func (s *Service) WorkflowList(ctx context.Context, req *connect.Request[docketv1.WorkflowListRequest]) (*connect.Response[docketv1.WorkflowListResponse], error) {
	return unary(ctx, s, req, decodeWorkflowList, verb.WorkflowList, encodeWorkflowList)
}

// WorkflowRegister registers the definition the request carries.
func (s *Service) WorkflowRegister(ctx context.Context, req *connect.Request[docketv1.WorkflowRegisterRequest]) (*connect.Response[docketv1.WorkflowRegisterResponse], error) {
	decode := decodeWorkflowRegister
	if TransportOf(ctx) == TransportHTTP {
		decode = decodeWorkflowRegisterOverHTTP
	}
	return unary(ctx, s, req, decode, verb.WorkflowRegister, encodeWorkflowRegister)
}

// WorkflowShow reads one workflow from the caller's project.
func (s *Service) WorkflowShow(ctx context.Context, req *connect.Request[docketv1.WorkflowShowRequest]) (*connect.Response[docketv1.WorkflowShowResponse], error) {
	return unary(ctx, s, req, decodeWorkflowShow, verb.WorkflowShow, encodeWorkflowShow)
}

// workflowRef joins a request's name and version into the name@version ref
// the verb parses, so a malformed pair is refused there as it is for the CLI.
func workflowRef(name string, version int32) string {
	return fmt.Sprintf("%s@%d", name, version)
}

func decodeWorkflowDeprecate(req *docketv1.WorkflowDeprecateRequest) (verb.WorkflowDeprecateInput, error) {
	return verb.WorkflowDeprecateInput{Ref: workflowRef(req.GetName(), req.GetVersion())}, nil
}

func encodeWorkflowDeprecate(res verb.WorkflowDeprecateResult) *docketv1.WorkflowDeprecateResponse {
	return &docketv1.WorkflowDeprecateResponse{Workflow: workflowMessage(res.Workflow)}
}

func decodeWorkflowLint(req *docketv1.WorkflowLintRequest) (verb.WorkflowLintInput, error) {
	return verb.WorkflowLintInput{
		Body:        []byte(req.GetBody()),
		SourcePath:  req.GetSourcePath(),
		AllProjects: req.GetAllProjects(),
	}, nil
}

// encodeWorkflowLint reports each project that would refuse the definition
// as an error finding located at that project's identity. A single-project
// refusal never reaches here: the verb returns it as the call's error.
func encodeWorkflowLint(res verb.WorkflowLintResult) *docketv1.WorkflowLintResponse {
	var findings []*docketv1.LintFinding
	for _, v := range res.Projects {
		if v.Err == nil {
			continue
		}
		findings = append(findings, &docketv1.LintFinding{
			Severity: "error",
			Message:  v.Err.Error(),
			Location: v.Project.Identity,
		})
	}
	return &docketv1.WorkflowLintResponse{Ok: len(findings) == 0, Findings: findings}
}

func decodeWorkflowList(req *docketv1.WorkflowListRequest) (verb.WorkflowListInput, error) {
	return verb.WorkflowListInput{
		Name:              req.GetName(),
		Limit:             int(req.GetLimit()),
		IncludeDeprecated: req.GetDeprecated(),
		Orphans:           req.GetOrphans(),
	}, nil
}

func encodeWorkflowList(res verb.WorkflowListResult) *docketv1.WorkflowListResponse {
	items := make([]*docketv1.Workflow, 0, len(res.Workflows))
	for _, w := range res.Workflows {
		items = append(items, workflowMessage(w))
	}
	return &docketv1.WorkflowListResponse{
		Items:     items,
		Total:     int32(res.Total),
		Truncated: len(items) < res.Total,
	}
}

func decodeWorkflowRegister(req *docketv1.WorkflowRegisterRequest) (verb.WorkflowRegisterInput, error) {
	body := []byte(req.GetBody())
	return verb.WorkflowRegisterInput{
		Source:     func() ([]byte, error) { return body, nil },
		SourcePath: req.GetSourcePath(),
	}, nil
}

// decodeWorkflowRegisterOverHTTP drops the request's source_path. The daemon
// later reads a stored source_path from its own filesystem, and an HTTP
// client's paths name files on its own machine, so the row stores none.
func decodeWorkflowRegisterOverHTTP(req *docketv1.WorkflowRegisterRequest) (verb.WorkflowRegisterInput, error) {
	in, err := decodeWorkflowRegister(req)
	in.SourcePath = ""
	return in, err
}

func encodeWorkflowRegister(res verb.WorkflowRegisterResult) *docketv1.WorkflowRegisterResponse {
	return &docketv1.WorkflowRegisterResponse{Workflow: workflowMessage(res.Workflow)}
}

// decodeWorkflowShow sends a bare name when the request leaves version unset,
// which the verb reads as the highest version still in service.
func decodeWorkflowShow(req *docketv1.WorkflowShowRequest) (verb.WorkflowShowInput, error) {
	ref := req.GetName()
	if req.Version != nil {
		ref = workflowRef(req.GetName(), req.GetVersion())
	}
	return verb.WorkflowShowInput{Ref: ref, Source: req.GetSource()}, nil
}

func encodeWorkflowShow(res verb.WorkflowShowResult) *docketv1.WorkflowShowResponse {
	return &docketv1.WorkflowShowResponse{Workflow: workflowMessage(res.Workflow)}
}

// workflowMessage is w on the wire, or nil when the verb returned no row.
func workflowMessage(w *model.Workflow) *docketv1.Workflow {
	if w == nil {
		return nil
	}
	return &docketv1.Workflow{
		Name:         w.Name,
		Version:      int32(w.Version),
		Description:  w.Description,
		SourcePath:   w.SourcePath,
		SourceSha256: w.SourceSHA256,
		Body:         w.Body,
		Deprecated:   w.Deprecated(),
		CreatedAtMs:  w.CreatedAtMS,
		RowVersion:   int32(w.RowVersion),
	}
}
