package app

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
)

type ListIssuesParams struct {
	Statuses    []string
	Priorities  []string
	Labels      []string
	Types       []string
	Assignee    string
	ParentID    *int
	RootsOnly   bool
	IncludeDone bool
	Sort        string
	SortDir     string
	Limit       int
}

type IssueListData struct {
	Issues    []*model.Issue
	Total     int
	ParentMap map[int]*model.Issue
	Progress  map[int]render.SubIssueProgress
}

type BoardParams struct {
	Labels     []string
	Priorities []string
	Assignee   string
	Expand     bool
}

type BoardData struct {
	Issues   []*model.Issue
	Progress map[int]render.SubIssueProgress
	Total    int
	Expanded bool
}

type IssueDetailData struct {
	Issue           *model.Issue
	SubIssues       []*model.Issue
	Relations       []model.Relation
	LinkedProposals []model.Proposal
	Comments        []*model.Comment
	Activity        []model.Activity
}

func ListIssues(conn *sql.DB, params ListIssuesParams) (IssueListData, error) {
	issues, total, err := db.ListIssues(conn, db.ListOptions{
		Statuses:    params.Statuses,
		Priorities:  params.Priorities,
		Labels:      params.Labels,
		Types:       params.Types,
		Assignee:    params.Assignee,
		ParentID:    params.ParentID,
		RootsOnly:   params.RootsOnly,
		IncludeDone: params.IncludeDone,
		Sort:        params.Sort,
		SortDir:     params.SortDir,
		Limit:       params.Limit,
	})
	if err != nil {
		return IssueListData{}, fmt.Errorf("listing issues: %w", err)
	}
	if err := db.HydrateDocs(conn, issues); err != nil {
		return IssueListData{}, fmt.Errorf("fetching linked docs: %w", err)
	}

	parentMap, progress, err := loadParentProgress(conn, issues)
	if err != nil {
		return IssueListData{}, err
	}

	return IssueListData{
		Issues:    issues,
		Total:     total,
		ParentMap: parentMap,
		Progress:  progress,
	}, nil
}

func LoadBoard(conn *sql.DB, params BoardParams) (BoardData, error) {
	issues, _, err := db.ListIssues(conn, db.ListOptions{
		Priorities:  params.Priorities,
		Labels:      params.Labels,
		Assignee:    params.Assignee,
		IncludeDone: true,
	})
	if err != nil {
		return BoardData{}, fmt.Errorf("listing issues: %w", err)
	}

	if !params.Expand {
		var roots []*model.Issue
		for _, issue := range issues {
			if issue.ParentID == nil {
				roots = append(roots, issue)
			}
		}
		issues = roots
	}

	progress, err := loadSubIssueProgress(conn, issues)
	if err != nil {
		return BoardData{}, err
	}

	return BoardData{
		Issues:   issues,
		Progress: progress,
		Total:    len(issues),
		Expanded: params.Expand,
	}, nil
}

func GetIssueDetail(conn *sql.DB, id int) (IssueDetailData, error) {
	issue, err := db.GetIssue(conn, id)
	if err != nil {
		return IssueDetailData{}, err
	}

	issue.Labels, err = db.GetIssueLabels(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching labels: %w", err)
	}

	issue.Files, err = db.GetIssueFiles(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching files: %w", err)
	}

	if err := db.HydrateDocs(conn, []*model.Issue{issue}); err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching linked docs: %w", err)
	}

	subIssues, err := db.GetSubIssues(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching sub-issues: %w", err)
	}

	relations, err := db.GetIssueRelations(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching relations: %w", err)
	}

	linkedProposals, err := db.GetIssueProposals(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching linked proposals: %w", err)
	}

	comments, err := db.ListComments(conn, id)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching comments: %w", err)
	}

	activity, err := db.GetActivity(conn, id, 10)
	if err != nil {
		return IssueDetailData{}, fmt.Errorf("fetching activity: %w", err)
	}

	return IssueDetailData{
		Issue:           issue,
		SubIssues:       subIssues,
		Relations:       relations,
		LinkedProposals: linkedProposals,
		Comments:        comments,
		Activity:        activity,
	}, nil
}

func loadParentProgress(conn *sql.DB, issues []*model.Issue) (map[int]*model.Issue, map[int]render.SubIssueProgress, error) {
	resultIDs := make(map[int]struct{}, len(issues))
	for _, issue := range issues {
		resultIDs[issue.ID] = struct{}{}
	}

	missingParentIDs := make(map[int]struct{})
	for _, issue := range issues {
		if issue.ParentID == nil {
			continue
		}
		pid := *issue.ParentID
		if _, inResult := resultIDs[pid]; !inResult {
			missingParentIDs[pid] = struct{}{}
		}
	}

	parentMap := make(map[int]*model.Issue)
	if len(missingParentIDs) > 0 {
		ids := make([]int, 0, len(missingParentIDs))
		for id := range missingParentIDs {
			ids = append(ids, id)
		}
		var err error
		parentMap, err = db.GetIssuesByIDs(conn, ids)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching parent issues: %w", err)
		}
	}

	parentIDSet := make(map[int]struct{})
	for _, issue := range issues {
		parentIDSet[issue.ID] = struct{}{}
	}
	for id := range parentMap {
		parentIDSet[id] = struct{}{}
	}
	for _, issue := range issues {
		if issue.ParentID == nil {
			continue
		}
		pid := *issue.ParentID
		if _, inResult := resultIDs[pid]; inResult {
			parentIDSet[pid] = struct{}{}
			continue
		}
		if _, inMap := parentMap[pid]; inMap {
			parentIDSet[pid] = struct{}{}
		}
	}

	progress := make(map[int]render.SubIssueProgress)
	if len(parentIDSet) == 0 {
		return parentMap, progress, nil
	}

	parentIDs := make([]int, 0, len(parentIDSet))
	for id := range parentIDSet {
		parentIDs = append(parentIDs, id)
	}
	batchProgress, err := db.GetBatchSubIssueProgress(conn, parentIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching sub-issue progress: %w", err)
	}
	for id, counts := range batchProgress {
		if counts[1] > 0 {
			progress[id] = render.SubIssueProgress{Done: counts[0], Total: counts[1]}
		}
	}

	return parentMap, progress, nil
}

func loadSubIssueProgress(conn *sql.DB, issues []*model.Issue) (map[int]render.SubIssueProgress, error) {
	if len(issues) == 0 {
		return map[int]render.SubIssueProgress{}, nil
	}

	parentIDs := make([]int, len(issues))
	for i, issue := range issues {
		parentIDs[i] = issue.ID
	}

	batchProgress, err := db.GetBatchSubIssueProgress(conn, parentIDs)
	if err != nil {
		return nil, fmt.Errorf("fetching sub-issue progress: %w", err)
	}

	progress := make(map[int]render.SubIssueProgress, len(batchProgress))
	for id, counts := range batchProgress {
		if counts[1] > 0 {
			progress[id] = render.SubIssueProgress{Done: counts[0], Total: counts[1]}
		}
	}

	return progress, nil
}
