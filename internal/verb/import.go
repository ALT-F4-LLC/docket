package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ImportInput is an import request. Document is the export file's content:
// the transport reads the file, and the verb never opens a path.
type ImportInput struct {
	Document string
	// Merge imports into a non-empty project; Replace clears the project's
	// own data first, and only when Yes confirms it.
	Merge   bool
	Replace bool
	Yes     bool
}

// ImportResult counts what an import wrote.
type ImportResult struct {
	Imported int
	Skipped  int
	// Remapped counts rows whose source id another project already held and
	// which were assigned a fresh id, with every reference rewritten.
	Remapped int
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when Import also returns an error, because the project stays
	// registered: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// Import writes an export document into the caller's project. It writes there,
// so it may register the project.
func Import(ctx context.Context, env Env, in ImportInput) (ImportResult, error) {
	if in.Merge && in.Replace {
		return ImportResult{}, newError(
			errors.New("--merge and --replace are mutually exclusive"), output.ErrValidation)
	}

	var data model.ExportData
	if err := json.Unmarshal([]byte(in.Document), &data); err != nil {
		return ImportResult{}, newError(fmt.Errorf("parsing JSON: %w", err), output.ErrValidation)
	}
	if errs := validateExportData(&data); len(errs) > 0 {
		return ImportResult{}, newError(fmt.Errorf("validation failed with %d error(s):\n  - %s",
			len(errs), strings.Join(errs, "\n  - ")), output.ErrValidation)
	}

	// --replace is destructive in every output mode, so Yes is required
	// unconditionally: an output format, or whether a terminal is attached,
	// must never double as consent.
	if in.Replace && !in.Yes {
		return ImportResult{}, newError(
			errors.New("--replace deletes THIS PROJECT's existing data; pass --yes to confirm"),
			output.ErrValidation)
	}

	projectID, registered, err := resolveRegisteringProject(env, "import")
	res := ImportResult{Registered: registered}
	if err != nil {
		return res, err
	}

	if !in.Merge && !in.Replace {
		// The default mode needs an empty PROJECT: under the shared store,
		// another project's issues are not existing data for this one.
		count, err := env.Store.CountIssues(projectID)
		if err != nil {
			return res, newError(fmt.Errorf("checking database: %w", err), output.ErrGeneral)
		}
		if count > 0 {
			return res, newError(errors.New(
				"database is not empty: use --merge to merge with existing data or --replace to replace it"),
				output.ErrConflict)
		}
	}

	written, err := RestoreExport(env.Store, &data, in.Replace, projectID)
	if err != nil {
		return res, newError(fmt.Errorf("importing data: %w", err), output.ErrGeneral)
	}
	written.Registered = registered
	return written, nil
}

// RestoreExport writes an export document into projectID in one transaction,
// clearing the project's own data first when replace is set. Source ids
// another project already holds are reassigned first, rewriting data in
// place. It is Import's write half: Import decides the project and refuses an
// unconfirmed or conflicting import before calling it.
func RestoreExport(st store.Store, data *model.ExportData, replace bool, projectID int) (ImportResult, error) {
	tx, err := st.Begin()
	if err != nil {
		return ImportResult{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	if replace {
		// Replace clears THIS PROJECT's data, never the store's: under the
		// shared store, "replace everything" scoped any wider would destroy
		// projects the operator was not looking at.
		if err := tx.ClearProjectDataTx(projectID); err != nil {
			return ImportResult{}, fmt.Errorf("clearing project data: %w", err)
		}
	}

	remapped, err := remapExportForImport(tx, data, projectID)
	if err != nil {
		return ImportResult{}, err
	}

	res := ImportResult{Remapped: remapped}
	for _, c := range exportCollections {
		imported, skipped, err := c.restore(tx, data)
		if err != nil {
			return ImportResult{}, err
		}
		res.Imported += imported
		res.Skipped += skipped
	}

	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("committing transaction: %w", err)
	}
	return res, nil
}

// validateExportData collects every structural error in the document rather
// than failing on the first.
func validateExportData(export *model.ExportData) []string {
	var errs []string

	if export.Version != 1 {
		errs = append(errs, fmt.Sprintf("unsupported version %d: expected 1", export.Version))
	}

	// Issues are validated by UnmarshalJSON (status, priority, kind), but they
	// are validated again here to collect all errors.
	for _, issue := range export.Issues {
		if err := model.ValidateStatus(issue.Status); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
		if err := model.ValidatePriority(issue.Priority); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
		if err := model.ValidateIssueKind(issue.Kind); err != nil {
			errs = append(errs, fmt.Sprintf("issue %s: %s", model.FormatID(issue.ID), err))
		}
	}

	for _, rel := range export.Relations {
		if err := model.ValidateRelationType(rel.RelationType); err != nil {
			errs = append(errs, fmt.Sprintf("relation %d: %s", rel.ID, err))
		}
	}

	for _, p := range export.Proposals {
		if err := model.ValidateCriticality(p.Criticality); err != nil {
			errs = append(errs, fmt.Sprintf("proposal %s: %s", model.FormatProposalID(p.ID), err))
		}
		if err := model.ValidateProposalStatus(p.Status); err != nil {
			errs = append(errs, fmt.Sprintf("proposal %s: %s", model.FormatProposalID(p.ID), err))
		}
	}

	for _, v := range export.Votes {
		if err := model.ValidateVerdict(v.Verdict); err != nil {
			errs = append(errs, fmt.Sprintf("vote %d: %s", v.ID, err))
		}
	}

	return errs
}

// Import-time id remapping.
//
// The restore inserts rows WITH their source ids and skips collisions, which
// is right for a restore into the project that exported them and wrong for a
// shared store: consolidating a second repository's export would find its
// DKT-1 already taken and silently count the row as skipped, links and all.
//
// The remap runs BEFORE the restore, over the loaded document: every id that
// already exists in the destination AS ANOTHER PROJECT'S ROW is reassigned a
// fresh one, and every reference to it (parent_id, join rows, votes,
// revisions) is rewritten to match. Ids that are still free are kept, so a
// restore into a fresh store stays byte-stable.
//
// WHAT COUNTS AS A COLLISION IS PROJECT-SCOPED. Re-importing a project's own
// export with merge must skip what is already there, not duplicate it: a row
// with the same id that belongs to THIS project is the same row (AUTOINCREMENT
// never reuses an id), so it keeps its id and the restore skips it. Labels are
// matched by (project, name) rather than id, because that is the table's real
// key: a label deleted and recreated under a new id is still the label the
// export's mappings mean, and a remapped duplicate would be dropped by
// UNIQUE(project_id, name) and leave every mapping to it dangling.
//
// Known limit: a foreign export merged into a project that already holds its
// own rows under the same ids is indistinguishable from the project's own
// export, and those rows are skipped rather than remapped. Import a foreign
// export into a fresh project instead, which is the consolidation path.
type importRemap struct {
	tx store.ReadTx
	// remapped counts the ids reassigned; a label matched by name is not one.
	remapped int
}

// remapExportForImport rewrites data in place for import into projectID.
// It returns how many rows were reassigned.
func remapExportForImport(tx store.ReadTx, data *model.ExportData, projectID int) (int, error) {
	r := &importRemap{tx: tx}

	labelMap, err := r.labelMap(data.Labels, projectID)
	if err != nil {
		return 0, err
	}
	issueMap, err := r.tableMap(store.ImportIssues, projectID,
		ids(data.Issues, func(i *model.Issue) int { return i.ID }))
	if err != nil {
		return 0, err
	}
	commentMap, err := r.tableMap(store.ImportComments, projectID,
		ids(data.Comments, func(c *model.Comment) int { return c.ID }))
	if err != nil {
		return 0, err
	}
	relationMap, err := r.tableMap(store.ImportIssueRelations, projectID,
		ids(data.Relations, func(rel model.Relation) int { return rel.ID }))
	if err != nil {
		return 0, err
	}
	activityMap, err := r.tableMap(store.ImportActivityLog, projectID,
		ids(data.ActivityLog, func(a *model.Activity) int { return a.ID }))
	if err != nil {
		return 0, err
	}
	proposalMap, err := r.tableMap(store.ImportProposals, projectID,
		ids(data.Proposals, func(p *model.Proposal) int { return p.ID }))
	if err != nil {
		return 0, err
	}
	voteMap, err := r.tableMap(store.ImportVotes, projectID,
		ids(data.Votes, func(v *model.Vote) int { return v.ID }))
	if err != nil {
		return 0, err
	}
	docMap, err := r.tableMap(store.ImportDocs, projectID,
		ids(data.Docs, func(d *model.Doc) int { return d.ID }))
	if err != nil {
		return 0, err
	}
	revisionMap, err := r.tableMap(store.ImportDocRevisions, projectID,
		ids(data.DocRevisions, func(rev *model.DocRevision) int { return rev.ID }))
	if err != nil {
		return 0, err
	}
	docCommentMap, err := r.tableMap(store.ImportDocComments, projectID,
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

// tableMap returns fresh ids for every source id that already exists in the
// destination table as ANOTHER project's row. A source id held by this
// project's own row is kept: it is the same row, and the restore skips it.
// Fresh ids start past both the destination's max and the document's own max,
// so they collide with neither existing rows nor document rows that keep
// their ids.
func (r *importRemap) tableMap(table store.ImportTable, projectID int, sourceIDs []int) (map[int]int, error) {
	if len(sourceIDs) == 0 {
		return nil, nil
	}

	next, err := r.tx.ImportMaxIDTx(table)
	if err != nil {
		return nil, err
	}
	for _, id := range sourceIDs {
		if id > next {
			next = id
		}
	}
	next++

	out := map[int]int{}
	for _, id := range sourceIDs {
		exists, err := r.tx.ImportRowExistsTx(table, id)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		owned, err := r.tx.ImportRowOwnedTx(table, id, projectID)
		if err != nil {
			return nil, err
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
		existing, found, err := r.tx.LabelIDByNameTx(projectID, l.Name)
		if err != nil {
			return nil, err
		}
		if !found {
			unresolved = append(unresolved, l.ID)
			continue
		}
		if existing != l.ID {
			byName[l.ID] = existing
		}
	}

	fresh, err := r.tableMap(store.ImportLabels, projectID, unresolved)
	if err != nil {
		return nil, err
	}
	for id, to := range fresh {
		byName[id] = to
	}
	return byName, nil
}
