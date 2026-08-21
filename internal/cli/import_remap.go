package cli

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Import-time ID remapping (v12, closing the silent-drop trap).
//
// The restore machinery inserts rows WITH their source ids and `INSERT OR
// IGNORE`s collisions — which was exactly right when a database held one
// project and an import was a restore, and exactly wrong for a shared store:
// consolidating a second repository's export would find its DKT-1 already
// taken and silently count the row under `skipped`, links and all.
//
// The fix runs BEFORE the restore, over the loaded document: every id that
// already exists in the destination is reassigned a fresh one, and every
// reference to it — parent_id, join rows, votes, revisions — is rewritten to
// match. Ids that are still free are kept, so a restore into a fresh store
// remains byte-stable. The restore machinery itself is untouched: with
// collisions gone, its inserts insert.
type importRemap struct {
	tx *sql.Tx
	// nextID hands out fresh ids per table, starting past both the
	// destination's max and the document's own max — a fresh id must collide
	// with neither an existing row nor a not-yet-inserted document row.
	remapped int
}

// remapExportForImport rewrites data in place for import into projectID.
// It returns how many rows were reassigned.
func remapExportForImport(tx *sql.Tx, data *model.ExportData, projectID int) (int, error) {
	r := &importRemap{tx: tx}

	labelMap, err := r.tableMap("labels", ids(data.Labels, func(l *model.Label) int { return l.ID }))
	if err != nil {
		return 0, err
	}
	issueMap, err := r.tableMap("issues", ids(data.Issues, func(i *model.Issue) int { return i.ID }))
	if err != nil {
		return 0, err
	}
	commentMap, err := r.tableMap("comments", ids(data.Comments, func(c *model.Comment) int { return c.ID }))
	if err != nil {
		return 0, err
	}
	relationMap, err := r.tableMap("issue_relations", ids(data.Relations, func(rel model.Relation) int { return rel.ID }))
	if err != nil {
		return 0, err
	}
	activityMap, err := r.tableMap("activity_log", ids(data.ActivityLog, func(a *model.Activity) int { return a.ID }))
	if err != nil {
		return 0, err
	}
	proposalMap, err := r.tableMap("proposals", ids(data.Proposals, func(p *model.Proposal) int { return p.ID }))
	if err != nil {
		return 0, err
	}
	voteMap, err := r.tableMap("votes", ids(data.Votes, func(v *model.Vote) int { return v.ID }))
	if err != nil {
		return 0, err
	}
	docMap, err := r.tableMap("docs", ids(data.Docs, func(d *model.Doc) int { return d.ID }))
	if err != nil {
		return 0, err
	}
	revisionMap, err := r.tableMap("doc_revisions", ids(data.DocRevisions, func(rev *model.DocRevision) int { return rev.ID }))
	if err != nil {
		return 0, err
	}
	docCommentMap, err := r.tableMap("doc_comments", ids(data.DocComments, func(c *model.DocComment) int { return c.ID }))
	if err != nil {
		return 0, err
	}

	apply := func(m map[int]int, id int) int {
		if fresh, ok := m[id]; ok {
			return fresh
		}
		return id
	}

	for _, l := range data.Labels {
		l.ID = apply(labelMap, l.ID)
		l.ProjectID = projectID
	}
	for _, i := range data.Issues {
		i.ID = apply(issueMap, i.ID)
		i.ProjectID = projectID
		if i.ParentID != nil {
			p := apply(issueMap, *i.ParentID)
			i.ParentID = &p
		}
	}
	for idx := range data.IssueLabelMappings {
		data.IssueLabelMappings[idx].IssueID = apply(issueMap, data.IssueLabelMappings[idx].IssueID)
		data.IssueLabelMappings[idx].LabelID = apply(labelMap, data.IssueLabelMappings[idx].LabelID)
	}
	for idx := range data.IssueFileMappings {
		data.IssueFileMappings[idx].IssueID = apply(issueMap, data.IssueFileMappings[idx].IssueID)
	}
	for _, c := range data.Comments {
		c.ID = apply(commentMap, c.ID)
		c.IssueID = apply(issueMap, c.IssueID)
	}
	for idx := range data.Relations {
		data.Relations[idx].ID = apply(relationMap, data.Relations[idx].ID)
		data.Relations[idx].SourceIssueID = apply(issueMap, data.Relations[idx].SourceIssueID)
		data.Relations[idx].TargetIssueID = apply(issueMap, data.Relations[idx].TargetIssueID)
	}
	for _, a := range data.ActivityLog {
		a.ID = apply(activityMap, a.ID)
		a.IssueID = apply(issueMap, a.IssueID)
	}
	for _, p := range data.Proposals {
		p.ID = apply(proposalMap, p.ID)
		p.ProjectID = projectID
	}
	for _, v := range data.Votes {
		v.ID = apply(voteMap, v.ID)
		v.ProposalID = apply(proposalMap, v.ProposalID)
	}
	for idx := range data.ProposalIssues {
		data.ProposalIssues[idx].ProposalID = apply(proposalMap, data.ProposalIssues[idx].ProposalID)
		data.ProposalIssues[idx].IssueID = apply(issueMap, data.ProposalIssues[idx].IssueID)
	}
	for _, d := range data.Docs {
		d.ID = apply(docMap, d.ID)
		d.ProjectID = projectID
	}
	for _, rev := range data.DocRevisions {
		rev.ID = apply(revisionMap, rev.ID)
		rev.DocID = apply(docMap, rev.DocID)
	}
	for _, c := range data.DocComments {
		c.ID = apply(docCommentMap, c.ID)
		c.DocID = apply(docMap, c.DocID)
	}
	for idx := range data.DocIssueLinks {
		data.DocIssueLinks[idx].DocID = apply(docMap, data.DocIssueLinks[idx].DocID)
		data.DocIssueLinks[idx].IssueID = apply(issueMap, data.DocIssueLinks[idx].IssueID)
	}
	for idx := range data.ProposalDocs {
		data.ProposalDocs[idx].ProposalID = apply(proposalMap, data.ProposalDocs[idx].ProposalID)
		data.ProposalDocs[idx].DocID = apply(docMap, data.ProposalDocs[idx].DocID)
	}

	return r.remapped, nil
}

// ids projects a collection onto its id column.
func ids[T any](rows []T, id func(T) int) []int {
	out := make([]int, 0, len(rows))
	for _, row := range rows {
		out = append(out, id(row))
	}
	return out
}

// tableMap returns fresh ids for every source id that already exists in the
// destination table. Fresh ids start past both the destination's max and the
// document's own max, so they collide with neither existing rows nor
// document rows that keep their ids.
func (r *importRemap) tableMap(table string, sourceIDs []int) (map[int]int, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}

	var dbMax sql.NullInt64
	if err := r.tx.QueryRow(
		`SELECT MAX(id) FROM ` + table).Scan(&dbMax); err != nil {
		return nil, fmt.Errorf("reading the id ceiling for %s: %w", table, err)
	}
	next := int(dbMax.Int64)
	for _, id := range sourceIDs {
		if id > next {
			next = id
		}
	}
	next++

	out := map[int]int{}
	for _, id := range sourceIDs {
		var exists bool
		if err := r.tx.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id = ?)`, id).Scan(&exists); err != nil {
			return nil, fmt.Errorf("probing %s id %d: %w", table, id, err)
		}
		if !exists {
			continue
		}
		out[id] = next
		next++
		r.remapped++
	}
	return out, nil
}
