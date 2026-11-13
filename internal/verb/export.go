package verb

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ExportInput selects the export's format and the issues it carries. An
// empty filter exports every issue in the project.
type ExportInput struct {
	Format   string
	Statuses []string
	Labels   []string
}

// ExportResult is the rendered export document in the requested format. The
// transport writes it where its caller asked; the verb never opens a path.
type ExportResult struct {
	Format   string
	Document string
}

// Export renders the caller's project as one document in JSON, CSV or
// Markdown. It is a read: an identity no project holds exports nothing.
func Export(ctx context.Context, env Env, in ExportInput) (ExportResult, error) {
	switch in.Format {
	case "json", "csv", "markdown":
	default:
		return ExportResult{}, newError(
			fmt.Errorf("invalid format %q: must be one of json, csv, markdown", in.Format),
			output.ErrValidation)
	}
	for _, s := range in.Statuses {
		if err := model.ValidateStatus(model.Status(s)); err != nil {
			return ExportResult{}, newError(err, output.ErrValidation)
		}
	}

	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return ExportResult{}, err
	}

	data := model.ExportData{
		Version:    1,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, c := range exportCollections {
		if err := c.fetch(env.Store, projectID, &data); err != nil {
			return ExportResult{}, newError(fmt.Errorf("fetching %s: %w", c.name, err), output.ErrGeneral)
		}
	}
	if len(in.Statuses) > 0 || len(in.Labels) > 0 {
		filterExportData(&data, in.Statuses, in.Labels)
	}

	var document string
	switch in.Format {
	case "json":
		document, err = renderExportJSON(data)
	case "csv":
		document, err = renderExportCSV(data.Issues)
	case "markdown":
		document, err = renderExportMarkdown(data.Issues, data.Comments)
	}
	if err != nil {
		return ExportResult{}, newError(fmt.Errorf("rendering export: %w", err), output.ErrGeneral)
	}
	return ExportResult{Format: in.Format, Document: document}, nil
}

// exportCollection binds one collection of the export document to both halves
// of its round trip: fetch reads it out of the store for an export, and restore
// writes it back for an import. A collection is one value, so it cannot be
// exported and never imported back.
type exportCollection struct {
	// name is the plural noun a fetch failure reads with: "fetching doc
	// revisions". Insert failures name a single row instead, through the
	// collection's describe.
	name    string
	fetch   func(store.Store, int, *model.ExportData) error
	restore func(store.Tx, *model.ExportData) (imported, skipped int, err error)
}

// collection builds the descriptor for one collection. describe names one row
// for an insert failure ("issue DKT-4", "label \"backend\""), enough to find
// it in the file being imported. An empty collection is fetched as an empty
// slice, so the JSON document carries [] rather than null.
func collection[T any](
	name string,
	field func(*model.ExportData) *[]T,
	list func(store.Store, int) ([]T, error),
	describe func(T) string,
	insert func(store.Tx, T) (bool, error),
) exportCollection {
	return collectionWithFixup(name, field, list, describe, insert, nil)
}

// collectionWithFixup is collection for a collection its inserts alone cannot
// fully restore. afterInsert runs once the collection's rows are in, and
// receives exactly the rows the insert accepted, never the ones an existing id
// caused to be skipped, so a fixup touches only what this import created.
func collectionWithFixup[T any](
	name string,
	field func(*model.ExportData) *[]T,
	list func(store.Store, int) ([]T, error),
	describe func(T) string,
	insert func(store.Tx, T) (bool, error),
	afterInsert func(store.Tx, []T) error,
) exportCollection {
	return exportCollection{
		name: name,
		fetch: func(st store.Store, projectID int, data *model.ExportData) error {
			rows, err := list(st, projectID)
			if err != nil {
				return err
			}
			if rows == nil {
				rows = []T{}
			}
			*field(data) = rows
			return nil
		},
		restore: func(tx store.Tx, data *model.ExportData) (int, int, error) {
			var imported, skipped int
			var inserted []T
			for _, row := range *field(data) {
				ok, err := insert(tx, row)
				if err != nil {
					return 0, 0, fmt.Errorf("inserting %s: %w", describe(row), err)
				}
				if !ok {
					skipped++
					continue
				}
				imported++
				if afterInsert != nil {
					inserted = append(inserted, row)
				}
			}
			if afterInsert != nil {
				if err := afterInsert(tx, inserted); err != nil {
					return 0, 0, err
				}
			}
			return imported, skipped, nil
		},
	}
}

// exportCollections is the export document, in the order an import has to
// write it: every row's foreign keys point only at collections listed above
// it, so restoring it front to back needs no ordering logic.
var exportCollections = []exportCollection{
	collection("labels",
		func(d *model.ExportData) *[]*model.Label { return &d.Labels },
		store.Store.ListAllLabelsRaw,
		func(label *model.Label) string { return fmt.Sprintf("label %q", label.Name) },
		store.Tx.InsertLabelWithID),

	// Issues carry a self-referential parent_id, so a child whose parent sorts
	// after it would fail its own foreign key on insert. Every issue therefore
	// lands with a NULL parent and the fixup reattaches afterwards, by which
	// time every possible parent exists.
	collectionWithFixup("issues",
		func(d *model.ExportData) *[]*model.Issue { return &d.Issues },
		store.Store.ListAllIssues,
		func(issue *model.Issue) string { return fmt.Sprintf("issue %s", model.FormatID(issue.ID)) },
		insertIssueWithoutParent,
		reattachIssueParents),

	// A join row whose issue or label is in neither the document nor the
	// project is skipped, never a raw foreign-key failure: the rows it joins
	// were dropped by a filtered export or deleted since, and the import runs
	// in one transaction, so failing here would write nothing at all.
	collection("label mappings",
		func(d *model.ExportData) *[]model.IssueLabelMapping { return &d.IssueLabelMappings },
		store.Store.ListAllIssueLabelMappings,
		func(m model.IssueLabelMapping) string {
			return fmt.Sprintf("issue-label mapping (issue=%d, label=%d)", m.IssueID, m.LabelID)
		},
		func(tx store.Tx, m model.IssueLabelMapping) (bool, error) {
			ok, err := rowsExist(tx, rowRef{store.ImportIssues, m.IssueID}, rowRef{store.ImportLabels, m.LabelID})
			if err != nil || !ok {
				return false, err
			}
			return tx.InsertIssueLabelMapping(m.IssueID, m.LabelID)
		}),
	collection("file mappings",
		func(d *model.ExportData) *[]model.IssueFileMapping { return &d.IssueFileMappings },
		store.Store.ListAllIssueFileMappings,
		func(m model.IssueFileMapping) string {
			return fmt.Sprintf("issue-file mapping (issue=%d, file=%q)", m.IssueID, m.FilePath)
		},
		func(tx store.Tx, m model.IssueFileMapping) (bool, error) {
			ok, err := rowsExist(tx, rowRef{store.ImportIssues, m.IssueID})
			if err != nil || !ok {
				return false, err
			}
			return tx.InsertIssueFileMapping(m.IssueID, m.FilePath)
		}),
	collection("comments",
		func(d *model.ExportData) *[]*model.Comment { return &d.Comments },
		store.Store.ListAllComments,
		func(c *model.Comment) string { return fmt.Sprintf("comment %d", c.ID) },
		store.Tx.InsertCommentWithID),
	collection("relations",
		func(d *model.ExportData) *[]model.Relation { return &d.Relations },
		store.Store.GetAllRelations,
		func(rel model.Relation) string { return fmt.Sprintf("relation %d", rel.ID) },
		func(tx store.Tx, rel model.Relation) (bool, error) { return tx.InsertRelationWithID(&rel) }),
	collection("activity log",
		func(d *model.ExportData) *[]*model.Activity { return &d.ActivityLog },
		store.Store.ListAllActivity,
		func(a *model.Activity) string { return fmt.Sprintf("activity %d", a.ID) },
		store.Tx.InsertActivityWithID),
	collection("proposals",
		func(d *model.ExportData) *[]*model.Proposal { return &d.Proposals },
		store.Store.ListAllProposals,
		func(p *model.Proposal) string { return fmt.Sprintf("proposal %s", model.FormatProposalID(p.ID)) },
		store.Tx.InsertProposalWithID),
	collection("votes",
		func(d *model.ExportData) *[]*model.Vote { return &d.Votes },
		store.Store.ListAllVotes,
		func(v *model.Vote) string { return fmt.Sprintf("vote %d", v.ID) },
		store.Tx.InsertVoteWithID),
	collection("proposal-issue links",
		func(d *model.ExportData) *[]model.ProposalIssueLink { return &d.ProposalIssues },
		store.Store.ListAllProposalIssues,
		func(l model.ProposalIssueLink) string {
			return fmt.Sprintf("proposal-issue link (proposal=%d, issue=%d)", l.ProposalID, l.IssueID)
		},
		func(tx store.Tx, l model.ProposalIssueLink) (bool, error) {
			return tx.InsertProposalIssueLink(l.ProposalID, l.IssueID)
		}),
	collection("docs",
		func(d *model.ExportData) *[]*model.Doc { return &d.Docs },
		store.Store.ListAllDocs,
		func(doc *model.Doc) string { return fmt.Sprintf("doc %s", model.FormatDocID(doc.ID)) },
		store.Tx.InsertDocWithID),
	collection("doc revisions",
		func(d *model.ExportData) *[]*model.DocRevision { return &d.DocRevisions },
		store.Store.ListAllDocRevisions,
		func(rev *model.DocRevision) string { return fmt.Sprintf("doc revision %d", rev.ID) },
		store.Tx.InsertDocRevisionWithID),
	collection("doc comments",
		func(d *model.ExportData) *[]*model.DocComment { return &d.DocComments },
		store.Store.ListAllDocComments,
		func(c *model.DocComment) string { return fmt.Sprintf("doc comment %d", c.ID) },
		store.Tx.InsertDocCommentWithID),
	collection("doc-issue links",
		func(d *model.ExportData) *[]model.DocIssueLink { return &d.DocIssueLinks },
		store.Store.ListAllDocIssueLinks,
		func(l model.DocIssueLink) string {
			return fmt.Sprintf("doc-issue link (doc=%d, issue=%d)", l.DocID, l.IssueID)
		},
		func(tx store.Tx, l model.DocIssueLink) (bool, error) {
			return tx.InsertDocIssueLink(l.DocID, l.IssueID, l.CreatedAt)
		}),
	collection("proposal-doc links",
		func(d *model.ExportData) *[]model.ProposalDocLink { return &d.ProposalDocs },
		store.Store.ListAllProposalDocs,
		func(l model.ProposalDocLink) string {
			return fmt.Sprintf("proposal-doc link (proposal=%d, doc=%d)", l.ProposalID, l.DocID)
		},
		func(tx store.Tx, l model.ProposalDocLink) (bool, error) {
			return tx.InsertProposalDocLink(l.ProposalID, l.DocID, l.CreatedAt)
		}),
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

// rowRef names one row of an import table.
type rowRef struct {
	table store.ImportTable
	id    int
}

// rowsExist reports whether every row exists. It is the guard a join row's
// insert runs before trusting its foreign keys, so a dangling mapping is
// counted as skipped instead of aborting the import.
func rowsExist(tx store.Tx, rows ...rowRef) (bool, error) {
	for _, row := range rows {
		exists, err := tx.ImportRowExistsTx(row.table, row.id)
		if err != nil || !exists {
			return false, err
		}
	}
	return true, nil
}

// insertIssueWithoutParent inserts an issue with parent_id held back, and puts
// the caller's value back the way it found it: a merge that fails partway must
// not leave the document rewritten, and reattachIssueParents reads the parent
// back off these same rows.
func insertIssueWithoutParent(tx store.Tx, issue *model.Issue) (bool, error) {
	parentID := issue.ParentID
	issue.ParentID = nil
	inserted, err := tx.InsertIssueWithID(issue)
	issue.ParentID = parentID
	return inserted, err
}

// reattachIssueParents restores parent_id on the issues this import inserted.
//
// Issues skipped as duplicates under merge never reach it: the destination's
// copy already has the parent the destination gave it, and overwriting that
// would re-home issues the import did not create. A parent absent from the
// destination leaves the child at the top level rather than failing the
// import, which is what makes a filtered export importable at all.
func reattachIssueParents(tx store.Tx, issues []*model.Issue) error {
	for _, issue := range issues {
		if issue.ParentID == nil {
			continue
		}
		parentExists, err := tx.ImportRowExistsTx(store.ImportIssues, *issue.ParentID)
		if err != nil {
			return fmt.Errorf("checking parent for issue %s: %w", model.FormatID(issue.ID), err)
		}
		if !parentExists {
			continue
		}
		if err := tx.RestoreIssueParentTx(issue.ID, *issue.ParentID); err != nil {
			return err
		}
	}
	return nil
}

// filterExportData narrows an already-fetched export document to the issues
// matching the given status and label filters, and to whatever else is still
// reachable once those issues are gone.
//
// Unlike fetching, normalizing, and restoring, this stays written out
// collection by collection, because no two collections reach the surviving set
// the same way: comments and activity follow their issue, relations need BOTH
// endpoints to survive, docs survive by having a surviving link rather than by
// any field of their own, revisions and doc comments follow the docs that
// survived that way, votes follow proposals which themselves survived by link,
// and labels survive only if a surviving mapping still points at them. The
// order matters too — each of the last few steps reads a set the step above it
// just narrowed. A table of predicates would have to encode that dependency
// anyway, and would hide it while doing so.
func filterExportData(data *model.ExportData, statuses, labels []string) {
	data.Issues = filterIssues(data.Issues, statuses, labels)

	// Build set of filtered issue IDs.
	issueIDs := make(map[int]bool, len(data.Issues))
	for _, issue := range data.Issues {
		issueIDs[issue.ID] = true
	}

	// Filter comments to only those belonging to filtered issues.
	filtered := make([]*model.Comment, 0, len(data.Comments))
	for _, c := range data.Comments {
		if issueIDs[c.IssueID] {
			filtered = append(filtered, c)
		}
	}
	data.Comments = filtered

	// Filter relations to only those where both sides are in the filtered set.
	filteredRels := make([]model.Relation, 0, len(data.Relations))
	for _, r := range data.Relations {
		if issueIDs[r.SourceIssueID] && issueIDs[r.TargetIssueID] {
			filteredRels = append(filteredRels, r)
		}
	}
	data.Relations = filteredRels

	// Filter label mappings to only those for filtered issues.
	filteredMappings := make([]model.IssueLabelMapping, 0, len(data.IssueLabelMappings))
	for _, m := range data.IssueLabelMappings {
		if issueIDs[m.IssueID] {
			filteredMappings = append(filteredMappings, m)
		}
	}
	data.IssueLabelMappings = filteredMappings

	// Filter file mappings to only those for filtered issues.
	filteredFileMappings := make([]model.IssueFileMapping, 0, len(data.IssueFileMappings))
	for _, m := range data.IssueFileMappings {
		if issueIDs[m.IssueID] {
			filteredFileMappings = append(filteredFileMappings, m)
		}
	}
	data.IssueFileMappings = filteredFileMappings

	// Filter activity log to only entries for filtered issues.
	filteredActivity := make([]*model.Activity, 0, len(data.ActivityLog))
	for _, a := range data.ActivityLog {
		if issueIDs[a.IssueID] {
			filteredActivity = append(filteredActivity, a)
		}
	}
	data.ActivityLog = filteredActivity

	// Filter doc-issue links to only those whose issue survives the filter.
	filteredDocIssueLinks := make([]model.DocIssueLink, 0, len(data.DocIssueLinks))
	for _, l := range data.DocIssueLinks {
		if issueIDs[l.IssueID] {
			filteredDocIssueLinks = append(filteredDocIssueLinks, l)
		}
	}
	data.DocIssueLinks = filteredDocIssueLinks

	// Filter proposal-issue links to only those whose issue survives the filter.
	filteredProposalIssues := make([]model.ProposalIssueLink, 0, len(data.ProposalIssues))
	for _, l := range data.ProposalIssues {
		if issueIDs[l.IssueID] {
			filteredProposalIssues = append(filteredProposalIssues, l)
		}
	}
	data.ProposalIssues = filteredProposalIssues

	survivingDocIDs := make(map[int]bool, len(data.DocIssueLinks))
	for _, l := range data.DocIssueLinks {
		survivingDocIDs[l.DocID] = true
	}
	filteredDocs := make([]*model.Doc, 0, len(data.Docs))
	for _, d := range data.Docs {
		if survivingDocIDs[d.ID] {
			filteredDocs = append(filteredDocs, d)
		}
	}
	data.Docs = filteredDocs

	filteredDocRevisions := make([]*model.DocRevision, 0, len(data.DocRevisions))
	for _, r := range data.DocRevisions {
		if survivingDocIDs[r.DocID] {
			filteredDocRevisions = append(filteredDocRevisions, r)
		}
	}
	data.DocRevisions = filteredDocRevisions

	filteredDocComments := make([]*model.DocComment, 0, len(data.DocComments))
	for _, c := range data.DocComments {
		if survivingDocIDs[c.DocID] {
			filteredDocComments = append(filteredDocComments, c)
		}
	}
	data.DocComments = filteredDocComments

	survivingProposalIDs := make(map[int]bool, len(data.ProposalIssues))
	for _, l := range data.ProposalIssues {
		survivingProposalIDs[l.ProposalID] = true
	}
	filteredProposals := make([]*model.Proposal, 0, len(data.Proposals))
	for _, p := range data.Proposals {
		if survivingProposalIDs[p.ID] {
			filteredProposals = append(filteredProposals, p)
		}
	}
	data.Proposals = filteredProposals

	filteredVotes := make([]*model.Vote, 0, len(data.Votes))
	for _, v := range data.Votes {
		if survivingProposalIDs[v.ProposalID] {
			filteredVotes = append(filteredVotes, v)
		}
	}
	data.Votes = filteredVotes

	filteredProposalDocs := make([]model.ProposalDocLink, 0, len(data.ProposalDocs))
	for _, l := range data.ProposalDocs {
		if survivingProposalIDs[l.ProposalID] && survivingDocIDs[l.DocID] {
			filteredProposalDocs = append(filteredProposalDocs, l)
		}
	}
	data.ProposalDocs = filteredProposalDocs

	// Filter labels to only those referenced by remaining mappings.
	usedLabelIDs := make(map[int]bool)
	for _, m := range data.IssueLabelMappings {
		usedLabelIDs[m.LabelID] = true
	}
	filteredLabels := make([]*model.Label, 0, len(data.Labels))
	for _, l := range data.Labels {
		if usedLabelIDs[l.ID] {
			filteredLabels = append(filteredLabels, l)
		}
	}
	data.Labels = filteredLabels
}

// filterIssues returns issues matching the given status and label filters.
func filterIssues(issues []*model.Issue, statuses, labels []string) []*model.Issue {
	statusSet := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		statusSet[s] = true
	}
	labelSet := make(map[string]bool, len(labels))
	for _, l := range labels {
		labelSet[l] = true
	}

	filtered := make([]*model.Issue, 0, len(issues))
	for _, issue := range issues {
		if len(statusSet) > 0 && !statusSet[string(issue.Status)] {
			continue
		}
		if len(labelSet) > 0 {
			hasAny := false
			for _, il := range issue.Labels {
				if labelSet[il] {
					hasAny = true
					break
				}
			}
			if !hasAny {
				continue
			}
		}
		filtered = append(filtered, issue)
	}

	survivingIDs := make(map[int]bool, len(filtered))
	for _, issue := range filtered {
		survivingIDs[issue.ID] = true
	}
	for _, issue := range filtered {
		if issue.ParentID != nil && !survivingIDs[*issue.ParentID] {
			issue.ParentID = nil
		}
	}

	return filtered
}

// renderExportJSON produces a pretty-printed JSON string of the export data.
func renderExportJSON(data model.ExportData) (string, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// renderExportCSV produces a CSV string with a header row and one row per issue.
func renderExportCSV(issues []*model.Issue) (string, error) {
	var buf strings.Builder
	cw := csv.NewWriter(&buf)

	header := []string{"id", "parent_id", "title", "description", "status", "priority", "type", "assignee", "labels", "files", "created_at", "updated_at"}
	if err := cw.Write(header); err != nil {
		return "", err
	}

	for _, issue := range issues {
		parentID := ""
		if issue.ParentID != nil {
			parentID = model.FormatID(*issue.ParentID)
		}

		labelsStr := strings.Join(issue.Labels, ",")
		// Use ";" to separate file paths since paths may contain commas.
		filesStr := strings.Join(issue.Files, ";")

		row := []string{
			model.FormatID(issue.ID),
			parentID,
			csvSafe(issue.Title),
			csvSafe(issue.Description),
			string(issue.Status),
			string(issue.Priority),
			string(issue.Kind),
			csvSafe(issue.Assignee),
			csvSafe(labelsStr),
			csvSafe(filesStr),
			issue.CreatedAt.UTC().Format(time.RFC3339),
			issue.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if err := cw.Write(row); err != nil {
			return "", err
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// escapeMarkdown replaces characters that have special meaning in Markdown so
// that arbitrary user text can be safely embedded in headings and inline spans.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`#`, `\#`,
		`*`, `\*`,
		`_`, `\_`,
		`[`, `\[`,
		`]`, `\]`,
		`<`, `\<`,
		`>`, `\>`,
		"`", "\\`",
		`|`, `\|`,
	)
	return r.Replace(s)
}

// renderExportMarkdown produces a Markdown string grouping issues by status.
func renderExportMarkdown(issues []*model.Issue, comments []*model.Comment) (string, error) {
	// Group issues by status.
	statusOrder := []model.Status{
		model.StatusBacklog,
		model.StatusTodo,
		model.StatusInProgress,
		model.StatusReview,
		model.StatusDone,
	}

	grouped := make(map[model.Status][]*model.Issue)
	for _, issue := range issues {
		grouped[issue.Status] = append(grouped[issue.Status], issue)
	}

	// Build comment lookup by issue ID.
	commentsByIssue := make(map[int][]*model.Comment)
	for _, c := range comments {
		commentsByIssue[c.IssueID] = append(commentsByIssue[c.IssueID], c)
	}

	var buf strings.Builder
	buf.WriteString("# Docket Export\n\n")

	for _, status := range statusOrder {
		group := grouped[status]
		if len(group) == 0 {
			continue
		}

		buf.WriteString(fmt.Sprintf("## %s\n\n", string(status)))

		for _, issue := range group {
			buf.WriteString(fmt.Sprintf("### %s: %s\n\n", model.FormatID(issue.ID), escapeMarkdown(issue.Title)))

			// Metadata.
			buf.WriteString(fmt.Sprintf("- **Priority:** %s\n", escapeMarkdown(string(issue.Priority))))
			buf.WriteString(fmt.Sprintf("- **Type:** %s\n", escapeMarkdown(string(issue.Kind))))
			if issue.Assignee != "" {
				buf.WriteString(fmt.Sprintf("- **Assignee:** %s\n", escapeMarkdown(issue.Assignee)))
			}
			if len(issue.Labels) > 0 {
				escaped := make([]string, len(issue.Labels))
				for i, l := range issue.Labels {
					escaped[i] = escapeMarkdown(l)
				}
				buf.WriteString(fmt.Sprintf("- **Labels:** %s\n", strings.Join(escaped, ", ")))
			}
			if len(issue.Files) > 0 {
				escapedFiles := make([]string, len(issue.Files))
				for i, f := range issue.Files {
					escapedFiles[i] = escapeMarkdown(f)
				}
				buf.WriteString(fmt.Sprintf("- **Files:** %s\n", strings.Join(escapedFiles, ", ")))
			}
			buf.WriteString("\n")

			// Description.
			if issue.Description != "" {
				buf.WriteString(escapeMarkdown(issue.Description) + "\n\n")
			}

			// Comments.
			issueComments := commentsByIssue[issue.ID]
			if len(issueComments) > 0 {
				buf.WriteString("**Comments:**\n\n")
				for _, c := range issueComments {
					buf.WriteString(fmt.Sprintf("> **%s** (%s):\n> %s\n\n",
						escapeMarkdown(c.AuthorOrAnonymous()),
						c.CreatedAt.UTC().Format(time.RFC3339),
						escapeMarkdown(c.Body),
					))
				}
			}
		}
	}

	return buf.String(), nil
}
