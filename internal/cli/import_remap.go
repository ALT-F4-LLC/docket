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
// already exists in the destination AS ANOTHER PROJECT'S ROW is reassigned a
// fresh one, and every reference to it — parent_id, join rows, votes,
// revisions — is rewritten to match. Ids that are still free are kept, so a
// restore into a fresh store remains byte-stable. The restore machinery itself
// is untouched: with collisions gone, its inserts insert.
//
// WHAT COUNTS AS A COLLISION IS PROJECT-SCOPED. An export is one
// project's tracker, and re-importing it into that project with --merge must
// skip what is already there, not duplicate it: a row with the same id that
// belongs to THIS project is the same row (AUTOINCREMENT never reuses an id),
// so it keeps its id and the restore's INSERT OR IGNORE skips it. A row with
// that id in another project is the consolidation case, and is remapped.
// Labels are matched by (project, name) rather than id, because that is the
// table's real key: a label deleted and recreated under a new id is still the
// label the export's mappings mean, and a remapped duplicate would be dropped
// by UNIQUE(project_id, name) and leave every mapping to it dangling.
//
// Known limit: a foreign export merged into a project that already holds its
// own rows under the same ids is indistinguishable from the project's own
// export, and those rows are skipped rather than remapped. Import a foreign
// export into a fresh project instead, which is the consolidation path.
type importRemap struct {
	tx *sql.Tx
	// remapped counts the ids reassigned; a label matched by name is not one.
	remapped int
}

// remapExportForImport rewrites data in place for import into projectID.
// It returns how many rows were reassigned.
func remapExportForImport(tx *sql.Tx, data *model.ExportData, projectID int) (int, error) {
	r := &importRemap{tx: tx}

	labelMap, err := r.labelMap(data.Labels, projectID)
	if err != nil {
		return 0, err
	}
	issueMap, err := r.tableMap("issues", ownedIssue, projectID,
		ids(data.Issues, func(i *model.Issue) int { return i.ID }))
	if err != nil {
		return 0, err
	}
	commentMap, err := r.tableMap("comments", ownedComment, projectID,
		ids(data.Comments, func(c *model.Comment) int { return c.ID }))
	if err != nil {
		return 0, err
	}
	relationMap, err := r.tableMap("issue_relations", ownedRelation, projectID,
		ids(data.Relations, func(rel model.Relation) int { return rel.ID }))
	if err != nil {
		return 0, err
	}
	activityMap, err := r.tableMap("activity_log", ownedActivity, projectID,
		ids(data.ActivityLog, func(a *model.Activity) int { return a.ID }))
	if err != nil {
		return 0, err
	}
	proposalMap, err := r.tableMap("proposals", ownedProposal, projectID,
		ids(data.Proposals, func(p *model.Proposal) int { return p.ID }))
	if err != nil {
		return 0, err
	}
	voteMap, err := r.tableMap("votes", ownedVote, projectID,
		ids(data.Votes, func(v *model.Vote) int { return v.ID }))
	if err != nil {
		return 0, err
	}
	docMap, err := r.tableMap("docs", ownedDoc, projectID,
		ids(data.Docs, func(d *model.Doc) int { return d.ID }))
	if err != nil {
		return 0, err
	}
	revisionMap, err := r.tableMap("doc_revisions", ownedDocRevision, projectID,
		ids(data.DocRevisions, func(rev *model.DocRevision) int { return rev.ID }))
	if err != nil {
		return 0, err
	}
	docCommentMap, err := r.tableMap("doc_comments", ownedDocComment, projectID,
		ids(data.DocComments, func(c *model.DocComment) int { return c.ID }))
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

// Ownership probes: does the row with this id belong to THIS project? Root
// tables carry project_id; child tables reach it through their parent. Each
// takes (id, projectID).
const (
	ownedIssue   = `SELECT EXISTS(SELECT 1 FROM issues WHERE id = ? AND project_id = ?)`
	ownedComment = `SELECT EXISTS(SELECT 1 FROM comments c JOIN issues i ON i.id = c.issue_id
		WHERE c.id = ? AND i.project_id = ?)`
	ownedRelation = `SELECT EXISTS(SELECT 1 FROM issue_relations r JOIN issues i ON i.id = r.source_issue_id
		WHERE r.id = ? AND i.project_id = ?)`
	ownedActivity = `SELECT EXISTS(SELECT 1 FROM activity_log a JOIN issues i ON i.id = a.issue_id
		WHERE a.id = ? AND i.project_id = ?)`
	ownedProposal = `SELECT EXISTS(SELECT 1 FROM proposals WHERE id = ? AND project_id = ?)`
	ownedVote     = `SELECT EXISTS(SELECT 1 FROM votes v JOIN proposals p ON p.id = v.proposal_id
		WHERE v.id = ? AND p.project_id = ?)`
	ownedDoc         = `SELECT EXISTS(SELECT 1 FROM docs WHERE id = ? AND project_id = ?)`
	ownedDocRevision = `SELECT EXISTS(SELECT 1 FROM doc_revisions r JOIN docs d ON d.id = r.doc_id
		WHERE r.id = ? AND d.project_id = ?)`
	ownedDocComment = `SELECT EXISTS(SELECT 1 FROM doc_comments c JOIN docs d ON d.id = c.doc_id
		WHERE c.id = ? AND d.project_id = ?)`
)

// tableMap returns fresh ids for every source id that already exists in the
// destination table as ANOTHER project's row. A source id held by this
// project's own row is kept: it is the same row, and the restore skips it.
// Fresh ids start past both the destination's max and the document's own max,
// so they collide with neither existing rows nor document rows that keep
// their ids.
func (r *importRemap) tableMap(table, ownedSQL string, projectID int, sourceIDs []int) (map[int]int, error) {
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
		var owned bool
		if err := r.tx.QueryRow(ownedSQL, id, projectID).Scan(&owned); err != nil {
			return nil, fmt.Errorf("probing %s id %d for project %d: %w", table, id, projectID, err)
		}
		if owned {
			continue
		}
		out[id] = next
		next++
		r.remapped++
	}
	return out, nil
}

// labelMap is tableMap for labels, whose identity is (project, name). A source
// label whose name this project already carries maps onto that row's id, so
// the restore skips the label and every mapping to it lands on the existing
// row. A source id that is free, or taken only by another project's label of
// a different name, follows the ordinary id rule.
func (r *importRemap) labelMap(labels []*model.Label, projectID int) (map[int]int, error) {
	if len(labels) == 0 {
		return nil, nil
	}

	byName := map[int]int{}
	var unresolved []int
	for _, l := range labels {
		var existing sql.NullInt64
		err := r.tx.QueryRow(
			`SELECT id FROM labels WHERE project_id = ? AND name = ?`, projectID, l.Name).Scan(&existing)
		switch {
		case err == sql.ErrNoRows:
			unresolved = append(unresolved, l.ID)
			continue
		case err != nil:
			return nil, fmt.Errorf("probing label %q in project %d: %w", l.Name, projectID, err)
		}
		if int(existing.Int64) != l.ID {
			byName[l.ID] = int(existing.Int64)
		}
	}

	fresh, err := r.tableMap("labels",
		`SELECT EXISTS(SELECT 1 FROM labels WHERE id = ? AND project_id = ?)`, projectID, unresolved)
	if err != nil {
		return nil, err
	}
	for id, to := range fresh {
		byName[id] = to
	}
	return byName, nil
}
