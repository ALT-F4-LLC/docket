package cli

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// exportCollection binds one collection of the export document to everything
// that has to be done with it: read it out of a database, give it an
// empty-rather-than-null identity in JSON, count it, and write it back.
//
// Those four used to be stated separately — a fetch in the export command, a
// nil check beside it, an insert loop in the import command, and a third
// enumeration in the tests — with nothing tying the four statements of a
// collection to each other. The failure that arrangement invites is the worst
// kind: a collection present in the export half and absent from the import half
// produces a file that parses, an import that succeeds, and data that is
// simply gone. Nothing errors, so nothing is noticed until someone looks for a
// row that no longer exists.
//
// Bundling the halves makes that failure structural instead. A collection is
// one value or it is nothing, so it cannot be half-added, and
// TestExportCollectionsCoverEveryExportField proves the table accounts for
// every slice on the export document.
type exportCollection struct {
	// name is the plural noun a fetch failure reads with: "fetching doc
	// revisions". Insert failures name a single row instead and get their
	// wording from describe, since "inserting doc revisions" would not say
	// which one.
	name string

	fetch     func(*sql.DB, int, *model.ExportData) error
	normalize func(*model.ExportData)
	restore   func(*sql.Tx, *model.ExportData) (imported, skipped int, err error)

	// count reports how many rows the document currently holds for this
	// collection. No command needs it; the round-trip test does, to compare a
	// source against its import collection by collection without restating the
	// list of collections a third time.
	count func(*model.ExportData) int
}

// collection builds a descriptor out of the three things that actually differ
// between collections: where the rows live on the export document, how the
// database hands them over, and how it takes them back. describe names one row
// for an insert failure ("issue DKT-4", "label \"backend\"") — enough to find
// the offending row in the file being imported.
//
// The type parameter is what makes the binding airtight: fetch, normalize,
// count and restore all close over the same field accessor, so a descriptor
// cannot read one collection and write another.
func collection[T any](
	name string,
	field func(*model.ExportData) *[]T,
	list func(*sql.DB, int) ([]T, error),
	describe func(T) string,
	insert func(*sql.Tx, T) (bool, error),
) exportCollection {
	return collectionWithFixup(name, field, list, describe, insert, nil)
}

// collectionWithFixup is collection for a collection that its inserts alone
// cannot fully restore. afterInsert runs once the collection's rows are in, and
// receives exactly the rows the insert accepted — never the ones an existing ID
// caused to be skipped — so a fixup can touch only what this import created.
func collectionWithFixup[T any](
	name string,
	field func(*model.ExportData) *[]T,
	list func(*sql.DB, int) ([]T, error),
	describe func(T) string,
	insert func(*sql.Tx, T) (bool, error),
	afterInsert func(*sql.Tx, []T) error,
) exportCollection {
	return exportCollection{
		name: name,
		fetch: func(conn *sql.DB, projectID int, data *model.ExportData) error {
			rows, err := list(conn, projectID)
			if err != nil {
				return err
			}
			*field(data) = rows
			return nil
		},
		normalize: func(data *model.ExportData) {
			if *field(data) == nil {
				*field(data) = []T{}
			}
		},
		count: func(data *model.ExportData) int {
			return len(*field(data))
		},
		restore: func(tx *sql.Tx, data *model.ExportData) (int, int, error) {
			var imported, skipped int
			var inserted []T
			for _, row := range *field(data) {
				ok, err := insert(tx, row)
				if err != nil {
					return 0, 0, fmt.Errorf("inserting %s: %w", describe(row), err)
				}
				if ok {
					imported++
					if afterInsert != nil {
						inserted = append(inserted, row)
					}
					continue
				}
				skipped++
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

// exportCollections is the export document, stated once, in the order an import
// has to write it: every row's foreign keys point only at collections listed
// above it. Export reads in the same order because reads are independent of
// each other, and one order is one fewer thing to keep straight.
//
// A collection added here is exported, imported, and covered by the round-trip
// test at once. There is no second list to remember.
var exportCollections = []exportCollection{
	collection(
		"labels",
		func(data *model.ExportData) *[]*model.Label { return &data.Labels },
		db.ListAllLabelsRaw,
		func(label *model.Label) string { return fmt.Sprintf("label %q", label.Name) },
		db.InsertLabelWithID,
	),

	// Issues carry a self-referential parent_id, so a child whose parent sorts
	// after it would fail its own foreign key on insert. Every issue therefore
	// lands with a NULL parent and the fixup reattaches afterwards, by which
	// time every possible parent exists.
	collectionWithFixup(
		"issues",
		func(data *model.ExportData) *[]*model.Issue { return &data.Issues },
		db.ListAllIssues,
		func(issue *model.Issue) string { return fmt.Sprintf("issue %s", model.FormatID(issue.ID)) },
		insertIssueWithoutParent,
		reattachIssueParents,
	),

	collection(
		"label mappings",
		func(data *model.ExportData) *[]model.IssueLabelMapping { return &data.IssueLabelMappings },
		db.ListAllIssueLabelMappings,
		func(m model.IssueLabelMapping) string {
			return fmt.Sprintf("issue-label mapping (issue=%d, label=%d)", m.IssueID, m.LabelID)
		},
		func(tx *sql.Tx, m model.IssueLabelMapping) (bool, error) {
			return db.InsertIssueLabelMapping(tx, m.IssueID, m.LabelID)
		},
	),

	collection(
		"file mappings",
		func(data *model.ExportData) *[]model.IssueFileMapping { return &data.IssueFileMappings },
		db.ListAllIssueFileMappings,
		func(m model.IssueFileMapping) string {
			return fmt.Sprintf("issue-file mapping (issue=%d, file=%q)", m.IssueID, m.FilePath)
		},
		func(tx *sql.Tx, m model.IssueFileMapping) (bool, error) {
			return db.InsertIssueFileMapping(tx, m.IssueID, m.FilePath)
		},
	),

	collection(
		"comments",
		func(data *model.ExportData) *[]*model.Comment { return &data.Comments },
		db.ListAllComments,
		func(c *model.Comment) string { return fmt.Sprintf("comment %d", c.ID) },
		db.InsertCommentWithID,
	),

	collection(
		"relations",
		func(data *model.ExportData) *[]model.Relation { return &data.Relations },
		db.GetAllRelations,
		func(rel model.Relation) string { return fmt.Sprintf("relation %d", rel.ID) },
		func(tx *sql.Tx, rel model.Relation) (bool, error) {
			return db.InsertRelationWithID(tx, &rel)
		},
	),

	collection(
		"activity log",
		func(data *model.ExportData) *[]*model.Activity { return &data.ActivityLog },
		db.ListAllActivity,
		func(a *model.Activity) string { return fmt.Sprintf("activity %d", a.ID) },
		db.InsertActivityWithID,
	),

	collection(
		"proposals",
		func(data *model.ExportData) *[]*model.Proposal { return &data.Proposals },
		db.ListAllProposals,
		func(p *model.Proposal) string { return fmt.Sprintf("proposal %s", model.FormatProposalID(p.ID)) },
		db.InsertProposalWithID,
	),

	collection(
		"votes",
		func(data *model.ExportData) *[]*model.Vote { return &data.Votes },
		db.ListAllVotes,
		func(v *model.Vote) string { return fmt.Sprintf("vote %d", v.ID) },
		db.InsertVoteWithID,
	),

	collection(
		"proposal-issue links",
		func(data *model.ExportData) *[]model.ProposalIssueLink { return &data.ProposalIssues },
		db.ListAllProposalIssues,
		func(l model.ProposalIssueLink) string {
			return fmt.Sprintf("proposal-issue link (proposal=%d, issue=%d)", l.ProposalID, l.IssueID)
		},
		func(tx *sql.Tx, l model.ProposalIssueLink) (bool, error) {
			return db.InsertProposalIssueLink(tx, l.ProposalID, l.IssueID)
		},
	),

	collection(
		"docs",
		func(data *model.ExportData) *[]*model.Doc { return &data.Docs },
		db.ListAllDocs,
		func(doc *model.Doc) string { return fmt.Sprintf("doc %s", model.FormatDocID(doc.ID)) },
		db.InsertDocWithID,
	),

	collection(
		"doc revisions",
		func(data *model.ExportData) *[]*model.DocRevision { return &data.DocRevisions },
		db.ListAllDocRevisions,
		func(rev *model.DocRevision) string { return fmt.Sprintf("doc revision %d", rev.ID) },
		db.InsertDocRevisionWithID,
	),

	collection(
		"doc comments",
		func(data *model.ExportData) *[]*model.DocComment { return &data.DocComments },
		db.ListAllDocComments,
		func(c *model.DocComment) string { return fmt.Sprintf("doc comment %d", c.ID) },
		db.InsertDocCommentWithID,
	),

	collection(
		"doc-issue links",
		func(data *model.ExportData) *[]model.DocIssueLink { return &data.DocIssueLinks },
		db.ListAllDocIssueLinks,
		func(l model.DocIssueLink) string {
			return fmt.Sprintf("doc-issue link (doc=%d, issue=%d)", l.DocID, l.IssueID)
		},
		func(tx *sql.Tx, l model.DocIssueLink) (bool, error) {
			return db.InsertDocIssueLink(tx, l.DocID, l.IssueID, l.CreatedAt)
		},
	),

	collection(
		"proposal-doc links",
		func(data *model.ExportData) *[]model.ProposalDocLink { return &data.ProposalDocs },
		db.ListAllProposalDocs,
		func(l model.ProposalDocLink) string {
			return fmt.Sprintf("proposal-doc link (proposal=%d, doc=%d)", l.ProposalID, l.DocID)
		},
		func(tx *sql.Tx, l model.ProposalDocLink) (bool, error) {
			return db.InsertProposalDocLink(tx, l.ProposalID, l.DocID, l.CreatedAt)
		},
	),
}

// insertIssueWithoutParent inserts an issue with parent_id held back, and puts
// the caller's value back the way it found it. The caller owns the export
// document — a merge that fails partway must not leave the document rewritten,
// and reattachIssueParents reads the parent back off these same rows.
func insertIssueWithoutParent(tx *sql.Tx, issue *model.Issue) (bool, error) {
	parentID := issue.ParentID
	issue.ParentID = nil
	inserted, err := db.InsertIssueWithID(tx, issue)
	issue.ParentID = parentID
	return inserted, err
}

// reattachIssueParents restores parent_id on the issues this import inserted.
//
// Two omissions here are deliberate. Issues skipped as duplicates under --merge
// never reach this function, because the destination's copy already has
// whatever parent the destination gave it and overwriting that would let an
// import silently re-home issues it did not create. And a parent that is absent
// from the destination leaves the child at the top level rather than failing
// the import, which is what makes a filtered export importable at all: a filter
// that keeps a child and drops its parent is a supported way to export.
func reattachIssueParents(tx *sql.Tx, issues []*model.Issue) error {
	for _, issue := range issues {
		if issue.ParentID == nil {
			continue
		}
		var parentExists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", *issue.ParentID).Scan(&parentExists); err != nil {
			return fmt.Errorf("checking parent for issue %s: %w", model.FormatID(issue.ID), err)
		}
		if !parentExists {
			continue
		}
		if _, err := tx.Exec(`UPDATE issues SET parent_id = ? WHERE id = ?`, *issue.ParentID, issue.ID); err != nil {
			return fmt.Errorf("setting parent_id for issue %s: %w", model.FormatID(issue.ID), err)
		}
	}
	return nil
}
