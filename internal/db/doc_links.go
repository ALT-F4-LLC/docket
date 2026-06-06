package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// LinkDocIssue links a doc to an issue. Returns ErrNotFound if either side is
// missing; ErrConflict if the link already exists.
func LinkDocIssue(db *sql.DB, docID, issueID int) error {
	if err := assertDocExists(db, docID); err != nil {
		return err
	}
	if err := assertIssueExists(db, issueID); err != nil {
		return err
	}

	_, err := db.Exec(
		`INSERT INTO doc_issue_links (doc_id, issue_id, created_at) VALUES (?, ?, ?)`,
		docID, issueID, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("linking doc to issue: %w", err)
	}
	return nil
}

// UnlinkDocIssue removes a doc↔issue link. Returns ErrNotFound if no such
// link exists.
func UnlinkDocIssue(db *sql.DB, docID, issueID int) error {
	res, err := db.Exec(
		`DELETE FROM doc_issue_links WHERE doc_id = ? AND issue_id = ?`,
		docID, issueID,
	)
	if err != nil {
		return fmt.Errorf("unlinking doc from issue: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDocIssues returns issue IDs linked to a doc, ordered by issue_id ASC.
func GetDocIssues(db *sql.DB, docID int) ([]int, error) {
	return queryLinkIDs(db,
		`SELECT issue_id FROM doc_issue_links WHERE doc_id = ? ORDER BY issue_id ASC`,
		docID,
	)
}

// GetIssueDocs returns doc IDs linked to an issue, ordered by doc_id ASC.
func GetIssueDocs(db *sql.DB, issueID int) ([]int, error) {
	return queryLinkIDs(db,
		`SELECT doc_id FROM doc_issue_links WHERE issue_id = ? ORDER BY doc_id ASC`,
		issueID,
	)
}

func HydrateDocs(db *sql.DB, issues []*model.Issue) error {
	if len(issues) == 0 {
		return nil
	}

	ids := make([]any, len(issues))
	issueMap := make(map[int]*model.Issue, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
		issueMap[issue.ID] = issue
	}

	placeholders := makePlaceholders(len(ids))
	query := fmt.Sprintf(
		`SELECT l.issue_id, d.id, d.type, d.status, d.title
		 FROM doc_issue_links l
		 JOIN docs d ON d.id = l.doc_id
		 WHERE l.issue_id IN (%s)
		 ORDER BY d.id ASC`, placeholders,
	)

	rows, err := db.Query(query, ids...)
	if err != nil {
		return fmt.Errorf("querying docs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var issueID int
		var ref model.DocRef
		if err := rows.Scan(&issueID, &ref.ID, &ref.Type, &ref.Status, &ref.Title); err != nil {
			return fmt.Errorf("scanning doc: %w", err)
		}
		if issue, ok := issueMap[issueID]; ok {
			issue.Docs = append(issue.Docs, ref)
		}
	}
	return rows.Err()
}

func HydrateLinkedIssues(db *sql.DB, docIDs []int) (map[int][]model.IssueRef, error) {
	out := make(map[int][]model.IssueRef, len(docIDs))
	if len(docIDs) == 0 {
		return out, nil
	}

	ids := make([]any, len(docIDs))
	for i, id := range docIDs {
		ids[i] = id
	}

	placeholders := makePlaceholders(len(ids))
	query := fmt.Sprintf(
		`SELECT l.doc_id, i.id, i.kind, i.status, i.title
		 FROM doc_issue_links l
		 JOIN issues i ON i.id = l.issue_id
		 WHERE l.doc_id IN (%s)
		 ORDER BY i.id ASC`, placeholders,
	)

	rows, err := db.Query(query, ids...)
	if err != nil {
		return nil, fmt.Errorf("querying linked issues: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var docID int
		var ref model.IssueRef
		if err := rows.Scan(&docID, &ref.ID, &ref.Kind, &ref.Status, &ref.Title); err != nil {
			return nil, fmt.Errorf("scanning linked issue: %w", err)
		}
		out[docID] = append(out[docID], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating linked issue rows: %w", err)
	}
	return out, nil
}

// LinkProposalDoc links a proposal (vote) to a doc. Returns ErrNotFound if
// either side is missing; ErrConflict if the link already exists.
func LinkProposalDoc(db *sql.DB, proposalID, docID int) error {
	if err := assertProposalExists(db, proposalID); err != nil {
		return err
	}
	if err := assertDocExists(db, docID); err != nil {
		return err
	}

	_, err := db.Exec(
		`INSERT INTO proposal_docs (proposal_id, doc_id, created_at) VALUES (?, ?, ?)`,
		proposalID, docID, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			return ErrConflict
		}
		return fmt.Errorf("linking proposal to doc: %w", err)
	}
	return nil
}

// UnlinkProposalDoc removes a proposal↔doc link. Returns ErrNotFound if no
// such link exists.
func UnlinkProposalDoc(db *sql.DB, proposalID, docID int) error {
	res, err := db.Exec(
		`DELETE FROM proposal_docs WHERE proposal_id = ? AND doc_id = ?`,
		proposalID, docID,
	)
	if err != nil {
		return fmt.Errorf("unlinking proposal from doc: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetProposalDocs returns doc IDs linked to a proposal, ordered by doc_id ASC.
func GetProposalDocs(db *sql.DB, proposalID int) ([]int, error) {
	return queryLinkIDs(db,
		`SELECT doc_id FROM proposal_docs WHERE proposal_id = ? ORDER BY doc_id ASC`,
		proposalID,
	)
}

// GetDocProposals returns proposal IDs linked to a doc, ordered by
// proposal_id ASC.
func GetDocProposals(db *sql.DB, docID int) ([]int, error) {
	return queryLinkIDs(db,
		`SELECT proposal_id FROM proposal_docs WHERE doc_id = ? ORDER BY proposal_id ASC`,
		docID,
	)
}

// InsertDocIssueLink inserts a doc_issue_links row, skipping on PK conflict.
// Used by export/import round-trip. Must be called within a transaction.
// Returns true if inserted.
func InsertDocIssueLink(tx *sql.Tx, docID, issueID int, createdAt string) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO doc_issue_links (doc_id, issue_id, created_at) VALUES (?, ?, ?)`,
		docID, issueID, createdAt,
	)
	if err != nil {
		return false, fmt.Errorf("inserting doc_issue_link (%d,%d): %w", docID, issueID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertProposalDocLink inserts a proposal_docs row, skipping on PK conflict.
// Must be called within a transaction. Returns true if inserted.
func InsertProposalDocLink(tx *sql.Tx, proposalID, docID int, createdAt string) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO proposal_docs (proposal_id, doc_id, created_at) VALUES (?, ?, ?)`,
		proposalID, docID, createdAt,
	)
	if err != nil {
		return false, fmt.Errorf("inserting proposal_doc (%d,%d): %w", proposalID, docID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- helpers ---

func assertDocExists(db *sql.DB, id int) error {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM docs WHERE id = ?)", id).Scan(&exists); err != nil {
		return fmt.Errorf("checking doc existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func assertIssueExists(db *sql.DB, id int) error {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", id).Scan(&exists); err != nil {
		return fmt.Errorf("checking issue existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func assertProposalExists(db *sql.DB, id int) error {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM proposals WHERE id = ?)", id).Scan(&exists); err != nil {
		return fmt.Errorf("checking proposal existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func queryLinkIDs(db *sql.DB, query string, arg int) ([]int, error) {
	rows, err := db.Query(query, arg)
	if err != nil {
		return nil, fmt.Errorf("querying link ids: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning link id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating link id rows: %w", err)
	}
	return out, nil
}

func isUniqueOrPKConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "PRIMARY KEY")
}
