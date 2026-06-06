package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

func buildExport(t *testing.T, conn *sql.DB) *model.ExportData {
	t.Helper()

	issues, err := db.ListAllIssues(conn)
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	comments, err := db.ListAllComments(conn)
	if err != nil {
		t.Fatalf("ListAllComments: %v", err)
	}
	relations, err := db.GetAllRelations(conn)
	if err != nil {
		t.Fatalf("GetAllRelations: %v", err)
	}
	labels, err := db.ListAllLabelsRaw(conn)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
	labelMappings, err := db.ListAllIssueLabelMappings(conn)
	if err != nil {
		t.Fatalf("ListAllIssueLabelMappings: %v", err)
	}
	fileMappings, err := db.ListAllIssueFileMappings(conn)
	if err != nil {
		t.Fatalf("ListAllIssueFileMappings: %v", err)
	}
	docs, err := db.ListAllDocs(conn)
	if err != nil {
		t.Fatalf("ListAllDocs: %v", err)
	}
	docRevisions, err := db.ListAllDocRevisions(conn)
	if err != nil {
		t.Fatalf("ListAllDocRevisions: %v", err)
	}
	docComments, err := db.ListAllDocComments(conn)
	if err != nil {
		t.Fatalf("ListAllDocComments: %v", err)
	}
	docIssueLinks, err := db.ListAllDocIssueLinks(conn)
	if err != nil {
		t.Fatalf("ListAllDocIssueLinks: %v", err)
	}
	proposalDocs, err := db.ListAllProposalDocs(conn)
	if err != nil {
		t.Fatalf("ListAllProposalDocs: %v", err)
	}
	proposals, err := db.ListAllProposals(conn)
	if err != nil {
		t.Fatalf("ListAllProposals: %v", err)
	}
	votes, err := db.ListAllVotes(conn)
	if err != nil {
		t.Fatalf("ListAllVotes: %v", err)
	}
	proposalIssues, err := db.ListAllProposalIssues(conn)
	if err != nil {
		t.Fatalf("ListAllProposalIssues: %v", err)
	}

	return &model.ExportData{
		Version:            1,
		ExportedAt:         "2026-01-01T00:00:00Z",
		Issues:             issues,
		Comments:           comments,
		Relations:          relations,
		Labels:             labels,
		IssueLabelMappings: labelMappings,
		IssueFileMappings:  fileMappings,
		Docs:               docs,
		DocRevisions:       docRevisions,
		DocComments:        docComments,
		DocIssueLinks:      docIssueLinks,
		Proposals:          proposals,
		Votes:              votes,
		ProposalIssues:     proposalIssues,
		ProposalDocs:       proposalDocs,
	}
}

func TestDoImportRoundTripPreservesDocs(t *testing.T) {
	src := newTestDB(t)

	issueID := createIssue(t, src, "linked issue", model.StatusTodo, model.PriorityMedium)

	docID := createDoc(t, src, "design doc", "tdd", "draft")
	revisedBody := "second revision body"
	if _, err := db.UpdateDoc(src, docID, db.DocUpdate{Body: &revisedBody, Author: "editor"}); err != nil {
		t.Fatalf("UpdateDoc: %v", err)
	}

	otherDocID := createDoc(t, src, "decision record", "adr", "accepted")

	for _, c := range []*model.DocComment{
		{DocID: docID, Body: "first comment", Author: "alice"},
		{DocID: docID, Body: "second comment", Author: "bob"},
		{DocID: otherDocID, Body: "third comment", Author: "carol"},
	} {
		if _, err := db.CreateDocComment(src, c); err != nil {
			t.Fatalf("CreateDocComment: %v", err)
		}
	}

	linkDocIssue(t, src, docID, issueID)

	export := buildExport(t, src)

	dst := newTestDB(t)
	if _, err := doImport(dst, export); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotDocs, err := db.ListAllDocs(dst)
	if err != nil {
		t.Fatalf("ListAllDocs(dst): %v", err)
	}
	if len(gotDocs) != 2 {
		t.Fatalf("expected 2 docs after import, got %d", len(gotDocs))
	}

	gotRevisions, err := db.ListAllDocRevisions(dst)
	if err != nil {
		t.Fatalf("ListAllDocRevisions(dst): %v", err)
	}
	// docID: create + body edit = 2 revisions; otherDocID: create = 1 revision.
	if len(gotRevisions) != 3 {
		t.Fatalf("expected 3 doc revisions after import, got %d", len(gotRevisions))
	}

	gotComments, err := db.ListAllDocComments(dst)
	if err != nil {
		t.Fatalf("ListAllDocComments(dst): %v", err)
	}
	if len(gotComments) != 3 {
		t.Fatalf("expected 3 doc comments after import, got %d", len(gotComments))
	}

	gotLinks, err := db.ListAllDocIssueLinks(dst)
	if err != nil {
		t.Fatalf("ListAllDocIssueLinks(dst): %v", err)
	}
	if len(gotLinks) != 1 {
		t.Fatalf("expected 1 doc-issue link after import, got %d", len(gotLinks))
	}
	if gotLinks[0].DocID != docID || gotLinks[0].IssueID != issueID {
		t.Errorf("expected link (doc=%d, issue=%d), got (doc=%d, issue=%d)",
			docID, issueID, gotLinks[0].DocID, gotLinks[0].IssueID)
	}

	gotDoc, err := db.GetDoc(dst, docID)
	if err != nil {
		t.Fatalf("GetDoc(dst, %d): %v", docID, err)
	}
	if gotDoc.Body != revisedBody {
		t.Errorf("expected doc body %q after import, got %q", revisedBody, gotDoc.Body)
	}
	if gotDoc.Title != "design doc" {
		t.Errorf("expected doc title %q, got %q", "design doc", gotDoc.Title)
	}
}

func TestDoImportRoundTripPreservesProposalsSubsystem(t *testing.T) {
	src := newTestDB(t)

	issueID := createIssue(t, src, "linked issue", model.StatusTodo, model.PriorityMedium)
	docID := createDoc(t, src, "linked doc", "tdd", "draft")

	score := 0.84
	proposalID, err := db.CreateProposal(src, &model.Proposal{
		Description:    "should we ship",
		Criticality:    model.CriticalityHigh,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
		WeightedScore:  &score,
		CreatedBy:      "@team-lead",
		Rationale:      "because",
		DomainTags:     []string{"backend", "data"},
		FilesChanged:   []string{"a.go", "b.go"},
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	if _, err := db.CastVote(src, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "@senior-engineer",
		VoterRole:       "senior-engineer",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         "looks correct",
		FindingsJSON:    &model.Findings{Blockers: nil, Concerns: []string{"one nit"}, Suggestions: []string{"rename x"}},
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	if err := db.LinkProposalIssue(src, proposalID, issueID); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}
	if err := db.LinkProposalDoc(src, proposalID, docID); err != nil {
		t.Fatalf("LinkProposalDoc: %v", err)
	}

	export := buildExport(t, src)

	dst := newTestDB(t)
	if _, err := doImport(dst, export); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotProposals, err := db.ListAllProposals(dst)
	if err != nil {
		t.Fatalf("ListAllProposals(dst): %v", err)
	}
	if len(gotProposals) != 1 {
		t.Fatalf("expected 1 proposal after import, got %d", len(gotProposals))
	}
	p := gotProposals[0]
	if p.ID != proposalID || p.Description != "should we ship" {
		t.Errorf("proposal mismatch: got id=%d desc=%q", p.ID, p.Description)
	}
	if p.WeightedScore == nil || *p.WeightedScore != score {
		t.Errorf("expected weighted_score %v, got %v", score, p.WeightedScore)
	}
	if len(p.DomainTags) != 2 || len(p.FilesChanged) != 2 {
		t.Errorf("expected domain_tags/files_changed to round-trip, got %v / %v", p.DomainTags, p.FilesChanged)
	}

	gotVotes, err := db.ListAllVotes(dst)
	if err != nil {
		t.Fatalf("ListAllVotes(dst): %v", err)
	}
	if len(gotVotes) != 1 {
		t.Fatalf("expected 1 vote after import, got %d", len(gotVotes))
	}
	if gotVotes[0].FindingsJSON == nil || len(gotVotes[0].FindingsJSON.Concerns) != 1 {
		t.Errorf("expected findings_json to round-trip with 1 concern, got %+v", gotVotes[0].FindingsJSON)
	}

	gotProposalIssues, err := db.ListAllProposalIssues(dst)
	if err != nil {
		t.Fatalf("ListAllProposalIssues(dst): %v", err)
	}
	if len(gotProposalIssues) != 1 || gotProposalIssues[0].ProposalID != proposalID || gotProposalIssues[0].IssueID != issueID {
		t.Errorf("expected proposal-issue link (%d,%d), got %+v", proposalID, issueID, gotProposalIssues)
	}

	gotProposalDocs, err := db.ListAllProposalDocs(dst)
	if err != nil {
		t.Fatalf("ListAllProposalDocs(dst): %v", err)
	}
	if len(gotProposalDocs) != 1 || gotProposalDocs[0].ProposalID != proposalID || gotProposalDocs[0].DocID != docID {
		t.Errorf("expected proposal-doc link (%d,%d), got %+v", proposalID, docID, gotProposalDocs)
	}
}
