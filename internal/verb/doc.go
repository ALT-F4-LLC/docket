package verb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// DocListInput filters and orders a doc listing.
type DocListInput struct {
	Types    []string
	Statuses []string
	Author   string
	// Sort is "field" or "field:direction", e.g. "updated_at:desc".
	Sort  string
	Limit int
}

// DocListResult is one page of the project's docs. Total counts every doc
// the filters match, before Limit.
type DocListResult struct {
	Docs  []*store.DocSummary
	Total int
}

// DocList lists the docs of the caller's project.
func DocList(ctx context.Context, env Env, in DocListInput) (DocListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return DocListResult{}, err
	}

	opts := store.DocListOptions{
		ProjectID: projectID,
		Types:     in.Types,
		Statuses:  in.Statuses,
		Author:    in.Author,
		Limit:     in.Limit,
	}
	if in.Sort != "" {
		field, dir, _ := strings.Cut(in.Sort, ":")
		opts.Sort, opts.SortDir = field, dir
	}

	docs, total, err := env.Store.ListDocsWithCounts(opts)
	if err != nil {
		return DocListResult{}, newError(fmt.Errorf("listing docs: %w", err), output.ErrGeneral)
	}
	return DocListResult{Docs: docs, Total: total}, nil
}

// DocShowInput names a doc and, optionally, one of its revisions.
type DocShowInput struct {
	Doc string
	Rev *int
}

// DocShowResult is a doc with its history and links. When the input names a
// revision, only Revision is set; revision 0 is the latest.
type DocShowResult struct {
	Doc             *model.Doc
	Revisions       []*model.DocRevision
	Comments        []*model.DocComment
	LinkedIssues    []model.IssueRef
	LinkedProposals []int
	Revision        *model.DocRevision
}

// DocShow reads one doc, or one revision of it.
func DocShow(ctx context.Context, env Env, in DocShowInput) (DocShowResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return DocShowResult{}, err
	}
	id, err := parseDocRef(in.Doc)
	if err != nil {
		return DocShowResult{}, err
	}

	if in.Rev != nil {
		revision, err := env.Store.GetDocRevision(id, *in.Rev)
		if err != nil {
			if errors.Is(err, store.ErrValidation) {
				return DocShowResult{}, newError(err, output.ErrValidation)
			}
			return DocShowResult{}, docError(err,
				fmt.Sprintf("doc %s revision %d", in.Doc, *in.Rev), "fetching revision")
		}
		return DocShowResult{Revision: revision}, nil
	}

	doc, err := env.Store.GetDoc(id)
	if err != nil {
		return DocShowResult{}, docError(err, "doc "+in.Doc, "fetching doc")
	}
	revisions, err := env.Store.ListDocRevisions(id)
	if err != nil {
		return DocShowResult{}, newError(fmt.Errorf("fetching revisions: %w", err), output.ErrGeneral)
	}
	comments, err := env.Store.ListDocComments(id)
	if err != nil {
		return DocShowResult{}, newError(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}
	linkedIssues, err := env.Store.HydrateLinkedIssues([]int{id})
	if err != nil {
		return DocShowResult{}, newError(fmt.Errorf("fetching linked issues: %w", err), output.ErrGeneral)
	}
	linkedProposals, err := env.Store.GetDocProposals(id)
	if err != nil {
		return DocShowResult{}, newError(fmt.Errorf("fetching linked proposals: %w", err), output.ErrGeneral)
	}

	return DocShowResult{
		Doc:             doc,
		Revisions:       revisions,
		Comments:        comments,
		LinkedIssues:    linkedIssues[id],
		LinkedProposals: linkedProposals,
	}, nil
}

// DocCommentListInput names the doc whose comments to list.
type DocCommentListInput struct {
	Doc string
}

// DocCommentListResult is a doc's comments, oldest first.
type DocCommentListResult struct {
	DocID    int
	Comments []*model.DocComment
}

// DocCommentList lists the comments on one doc.
func DocCommentList(ctx context.Context, env Env, in DocCommentListInput) (DocCommentListResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return DocCommentListResult{}, err
	}
	id, err := parseDocRef(in.Doc)
	if err != nil {
		return DocCommentListResult{}, err
	}

	comments, err := env.Store.ListDocComments(id)
	if err != nil {
		return DocCommentListResult{}, docError(err, "doc "+in.Doc, "fetching comments")
	}
	return DocCommentListResult{DocID: id, Comments: comments}, nil
}

// DocCreateInput is the doc to create. Body is the content itself, never a
// path. A replayed IdempotencyKey returns the doc it first created.
type DocCreateInput struct {
	Title          string
	Body           string
	Type           string
	Status         string
	IdempotencyKey string
}

// DocCreateResult is the created doc. Registered is the caller's project as
// this call registered it, nil when it already existed; the caller records
// the project-registered event for it, since that write is outside
// store.Store.
type DocCreateResult struct {
	Doc        *model.Doc
	Registered *model.Project
}

// DocCreate creates a doc in the caller's project, authored by env.Author. It
// writes into that project, so it may register an anchored one.
func DocCreate(ctx context.Context, env Env, in DocCreateInput) (DocCreateResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return DocCreateResult{}, err
	}
	var result DocCreateResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	id, err := env.Store.CreateDocIdempotent(&model.Doc{
		ProjectID: projectID,
		Type:      in.Type,
		Status:    in.Status,
		Title:     in.Title,
		Body:      in.Body,
		Author:    env.Author,
	}, in.IdempotencyKey)
	if errors.Is(err, store.ErrConflict) {
		return result, newError(err, output.ErrConflict)
	}
	if err != nil {
		return result, newError(fmt.Errorf("creating doc: %w", err), output.ErrGeneral)
	}

	if result.Doc, err = env.Store.GetDoc(id); err != nil {
		return result, newError(fmt.Errorf("fetching created doc: %w", err), output.ErrGeneral)
	}
	return result, nil
}

// DocEditInput names a doc and the fields to change; a nil field is left as
// it is. Body is the content itself, never a path.
type DocEditInput struct {
	Doc    string
	Title  *string
	Body   *string
	Type   *string
	Status *string
}

// DocEditResult is the doc after the edit. Changed is false when the input
// changed nothing, either because it set no field or because every field it
// set already held that value. Registered is as in DocCreateResult.
type DocEditResult struct {
	Doc        *model.Doc
	Changed    bool
	Registered *model.Project
}

// DocEdit changes a doc's fields, recording a revision authored by
// env.Author. It is a write, so it may register the caller's anchored project.
func DocEdit(ctx context.Context, env Env, in DocEditInput) (DocEditResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return DocEditResult{}, err
	}
	var result DocEditResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	id, err := parseDocRef(in.Doc)
	if err != nil {
		return result, err
	}
	label := "doc " + model.FormatDocID(id)

	doc, err := env.Store.GetDoc(id)
	if err != nil {
		return result, docError(err, label, "fetching doc")
	}
	if in.Title == nil && in.Body == nil && in.Type == nil && in.Status == nil {
		result.Doc = doc
		return result, nil
	}

	rev, err := env.Store.UpdateDoc(id, store.DocUpdate{
		Title:  in.Title,
		Body:   in.Body,
		Status: in.Status,
		Type:   in.Type,
		Author: env.Author,
	})
	if err != nil {
		return result, docError(err, label, "updating doc")
	}
	if result.Doc, err = env.Store.GetDoc(id); err != nil {
		return result, newError(fmt.Errorf("fetching updated doc: %w", err), output.ErrGeneral)
	}
	result.Changed = rev != 0
	return result, nil
}

// DocDeleteInput names the doc to delete. Cascade also removes its issue and
// proposal links, which otherwise refuse the delete. Force is the caller's
// consent to the deletion.
type DocDeleteInput struct {
	Doc     string
	Cascade bool
	Force   bool
}

// DocDeleteResult is the doc as it stood before the delete. Registered is as
// in DocCreateResult.
type DocDeleteResult struct {
	Doc        *model.Doc
	Registered *model.Project
}

// DocDelete deletes a doc. Without Force it deletes nothing and returns the
// doc it would delete together with a VALIDATION_ERROR refusal, so a caller
// that can ask for consent has what to ask about. It is a write, so it may
// register the caller's anchored project.
func DocDelete(ctx context.Context, env Env, in DocDeleteInput) (DocDeleteResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return DocDeleteResult{}, err
	}
	var result DocDeleteResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	id, err := parseDocRef(in.Doc)
	if err != nil {
		return result, err
	}
	ref := model.FormatDocID(id)

	doc, err := env.Store.GetDoc(id)
	if err != nil {
		return result, docError(err, "doc "+ref, "fetching doc")
	}
	result.Doc = doc
	if !in.Force {
		return result, newError(fmt.Errorf(
			"deleting %s is destructive; pass --force to confirm", ref), output.ErrValidation)
	}

	if err := env.Store.DeleteDoc(id, in.Cascade); err != nil {
		if e := notFound(err, "doc "+ref); e != nil {
			return result, e
		}
		if errors.Is(err, store.ErrConflict) {
			return result, newError(err, output.ErrConflict)
		}
		return result, newError(fmt.Errorf("deleting doc: %w", err), output.ErrGeneral)
	}
	return result, nil
}

func parseDocRef(ref string) (int, error) {
	id, err := model.ParseDocID(ref)
	if err != nil {
		return 0, newError(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// docError reports a missing row as NOT_FOUND naming label, and any other
// failure as GENERAL_ERROR wrapped with doing.
func docError(err error, label, doing string) error {
	if e := notFound(err, label); e != nil {
		return e
	}
	return newError(fmt.Errorf("%s: %w", doing, err), output.ErrGeneral)
}
