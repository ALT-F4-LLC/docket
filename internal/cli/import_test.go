package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/spf13/cobra"
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
	activityLog, err := db.ListAllActivity(conn)
	if err != nil {
		t.Fatalf("ListAllActivity: %v", err)
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
		ActivityLog:        activityLog,
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
	if _, err := doImport(dst, export, false); err != nil {
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
	if _, err := doImport(dst, export, false); err != nil {
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

func TestDoImportRoundTripPreservesActivityLog(t *testing.T) {
	src := newTestDB(t)

	issueID := createIssue(t, src, "tracked issue", model.StatusTodo, model.PriorityMedium)
	if err := db.RecordActivity(src, issueID, "status", "todo", "in-progress", "@senior-engineer"); err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}

	wantActivity, err := db.ListAllActivity(src)
	if err != nil {
		t.Fatalf("ListAllActivity(src): %v", err)
	}
	if len(wantActivity) < 2 {
		t.Fatalf("expected at least 2 activity rows in source (created + status), got %d", len(wantActivity))
	}

	export := buildExport(t, src)

	dst := newTestDB(t)
	if err := db.ClearAllData(dst); err != nil {
		t.Fatalf("ClearAllData(dst): %v", err)
	}
	if _, err := doImport(dst, export, false); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotActivity, err := db.ListAllActivity(dst)
	if err != nil {
		t.Fatalf("ListAllActivity(dst): %v", err)
	}
	if len(gotActivity) != len(wantActivity) {
		t.Fatalf("expected %d activity rows after import, got %d", len(wantActivity), len(gotActivity))
	}
	for i := range wantActivity {
		w, g := wantActivity[i], gotActivity[i]
		if g.ID != w.ID {
			t.Errorf("activity[%d] id mismatch: want %d, got %d", i, w.ID, g.ID)
		}
		if g.IssueID != w.IssueID || g.FieldChanged != w.FieldChanged ||
			g.OldValue != w.OldValue || g.NewValue != w.NewValue || g.ChangedBy != w.ChangedBy {
			t.Errorf("activity[%d] field mismatch: want %+v, got %+v", i, w, g)
		}
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Errorf("expected no foreign key violations after import, found at least one")
	}
}

func TestDoImportReplaceRollsBackOnFailure(t *testing.T) {
	dst := newTestDB(t)
	seededIssueID := createIssue(t, dst, "must survive", model.StatusTodo, model.PriorityHigh)
	if err := db.AddLabelToIssue(dst, seededIssueID, "keep-me", "", "tester"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	src := newTestDB(t)
	createIssue(t, src, "incoming", model.StatusTodo, model.PriorityMedium)
	docID := createDoc(t, src, "incoming doc", "tdd", "draft")

	export := buildExport(t, src)
	export.DocIssueLinks = append(export.DocIssueLinks, model.DocIssueLink{
		DocID:     docID,
		IssueID:   999999,
		CreatedAt: "2026-01-01T00:00:00Z",
	})

	if _, err := doImport(dst, export, true); err == nil {
		t.Fatal("expected doImport(replace=true) to fail on dangling doc-issue link, got nil")
	}

	gotIssues, err := db.ListAllIssues(dst)
	if err != nil {
		t.Fatalf("ListAllIssues(dst): %v", err)
	}
	if len(gotIssues) != 1 || gotIssues[0].ID != seededIssueID || gotIssues[0].Title != "must survive" {
		t.Fatalf("expected seeded issue preserved after rollback, got %+v", gotIssues)
	}

	gotLabels, err := db.ListAllLabelsRaw(dst)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw(dst): %v", err)
	}
	if len(gotLabels) != 1 || gotLabels[0].Name != "keep-me" {
		t.Fatalf("expected seeded label preserved after rollback, got %+v", gotLabels)
	}
}

func TestDoImportReplaceClearsThenImports(t *testing.T) {
	dst := newTestDB(t)
	createIssue(t, dst, "old data", model.StatusTodo, model.PriorityHigh)

	src := newTestDB(t)
	createIssue(t, src, "new data", model.StatusTodo, model.PriorityMedium)

	export := buildExport(t, src)

	if _, err := doImport(dst, export, true); err != nil {
		t.Fatalf("doImport(replace=true): %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst)
	if err != nil {
		t.Fatalf("ListAllIssues(dst): %v", err)
	}
	if len(gotIssues) != 1 || gotIssues[0].Title != "new data" {
		t.Fatalf("expected only imported issue after successful replace, got %+v", gotIssues)
	}
}

func createChildIssue(t *testing.T, conn *sql.DB, title string, status model.Status, parentID int) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:    title,
		Status:   status,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindFeature,
		ParentID: &parentID,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(child %q): %v", title, err)
	}
	return id
}

func runFilteredExport(t *testing.T, conn *sql.DB, statuses []string) *model.ExportData {
	t.Helper()

	cmd := &cobra.Command{}
	cmd.Flags().StringP("format", "o", "json", "")
	cmd.Flags().StringP("file", "f", "", "")
	cmd.Flags().StringSliceP("status", "s", nil, "")
	cmd.Flags().StringSliceP("label", "l", nil, "")
	cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))

	outPath := filepath.Join(t.TempDir(), "export.json")
	if err := cmd.Flags().Set("file", outPath); err != nil {
		t.Fatalf("set file flag: %v", err)
	}
	for _, s := range statuses {
		if err := cmd.Flags().Set("status", s); err != nil {
			t.Fatalf("set status flag: %v", err)
		}
	}

	if err := exportCmd.RunE(cmd, nil); err != nil {
		t.Fatalf("exportCmd.RunE: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", outPath, err)
	}
	var export model.ExportData
	if err := json.Unmarshal(raw, &export); err != nil {
		t.Fatalf("Unmarshal export: %v", err)
	}
	return &export
}

func TestFilteredExportRoundTripDropsUnlinkedAndNullsParent(t *testing.T) {
	src := newTestDB(t)

	parentID := createIssue(t, src, "in-progress parent", model.StatusInProgress, model.PriorityMedium)
	childID := createChildIssue(t, src, "done child", model.StatusDone, parentID)

	linkedDocID := createDoc(t, src, "linked design doc", "tdd", "draft")
	linkDocIssue(t, src, linkedDocID, childID)
	if _, err := db.CreateDocComment(src, &model.DocComment{DocID: linkedDocID, Body: "on linked doc", Author: "alice"}); err != nil {
		t.Fatalf("CreateDocComment(linked): %v", err)
	}

	standaloneDocID := createDoc(t, src, "standalone adr", "adr", "accepted")
	if _, err := db.CreateDocComment(src, &model.DocComment{DocID: standaloneDocID, Body: "on standalone doc", Author: "bob"}); err != nil {
		t.Fatalf("CreateDocComment(standalone): %v", err)
	}

	linkedProposalID, err := db.CreateProposal(src, &model.Proposal{
		Description: "linked proposal", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedBy: "@team-lead",
	})
	if err != nil {
		t.Fatalf("CreateProposal(linked): %v", err)
	}
	if _, err := db.CastVote(src, &model.Vote{
		ProposalID: linkedProposalID, VoterName: "@senior-engineer", VoterRole: "senior-engineer",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8, Summary: "ok",
	}); err != nil {
		t.Fatalf("CastVote(linked): %v", err)
	}
	if err := db.LinkProposalIssue(src, linkedProposalID, childID); err != nil {
		t.Fatalf("LinkProposalIssue(linked): %v", err)
	}
	if err := db.LinkProposalDoc(src, linkedProposalID, linkedDocID); err != nil {
		t.Fatalf("LinkProposalDoc(linked): %v", err)
	}

	standaloneProposalID, err := db.CreateProposal(src, &model.Proposal{
		Description: "standalone proposal", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedBy: "@team-lead",
	})
	if err != nil {
		t.Fatalf("CreateProposal(standalone): %v", err)
	}
	if _, err := db.CastVote(src, &model.Vote{
		ProposalID: standaloneProposalID, VoterName: "@sdet", VoterRole: "sdet",
		Verdict: model.VerdictApprove, Confidence: 0.5, DomainRelevance: 0.5, Summary: "ok",
	}); err != nil {
		t.Fatalf("CastVote(standalone): %v", err)
	}

	export := runFilteredExport(t, src, []string{string(model.StatusDone)})

	if len(export.Issues) != 1 || export.Issues[0].ID != childID {
		t.Fatalf("expected only the done child in filtered export, got %+v", export.Issues)
	}
	if export.Issues[0].ParentID != nil {
		t.Errorf("expected filtered-out parent to be nulled, got parent_id=%v", *export.Issues[0].ParentID)
	}
	if len(export.Docs) != 1 || export.Docs[0].ID != linkedDocID {
		t.Errorf("expected only the linked doc in filtered export, got %+v", export.Docs)
	}
	for _, c := range export.DocComments {
		if c.DocID == standaloneDocID {
			t.Errorf("standalone doc comment leaked into filtered export: %+v", c)
		}
	}
	if len(export.Proposals) != 1 || export.Proposals[0].ID != linkedProposalID {
		t.Errorf("expected only the linked proposal in filtered export, got %+v", export.Proposals)
	}
	for _, v := range export.Votes {
		if v.ProposalID == standaloneProposalID {
			t.Errorf("standalone proposal's vote leaked into filtered export: %+v", v)
		}
	}
	if len(export.ProposalDocs) != 1 || export.ProposalDocs[0].ProposalID != linkedProposalID || export.ProposalDocs[0].DocID != linkedDocID {
		t.Errorf("expected single surviving proposal-doc link, got %+v", export.ProposalDocs)
	}

	dst := newTestDB(t)
	if err := db.ClearAllData(dst); err != nil {
		t.Fatalf("ClearAllData(dst): %v", err)
	}
	if _, err := doImport(dst, export, false); err != nil {
		t.Fatalf("doImport of filtered export: %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst)
	if err != nil {
		t.Fatalf("ListAllIssues(dst): %v", err)
	}
	if len(gotIssues) != 1 || gotIssues[0].ID != childID {
		t.Fatalf("expected single child issue imported, got %+v", gotIssues)
	}
	if gotIssues[0].ParentID != nil {
		t.Errorf("expected imported child to have NULL parent_id, got %v", *gotIssues[0].ParentID)
	}

	gotDocs, err := db.ListAllDocs(dst)
	if err != nil {
		t.Fatalf("ListAllDocs(dst): %v", err)
	}
	if len(gotDocs) != 1 || gotDocs[0].ID != linkedDocID {
		t.Errorf("expected only linked doc imported, got %+v", gotDocs)
	}

	gotProposals, err := db.ListAllProposals(dst)
	if err != nil {
		t.Fatalf("ListAllProposals(dst): %v", err)
	}
	if len(gotProposals) != 1 || gotProposals[0].ID != linkedProposalID {
		t.Errorf("expected only linked proposal imported, got %+v", gotProposals)
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Errorf("expected no foreign key violations after import, found at least one")
	}
}

func TestFilteredExportReplaceImportRoundTripsAndDropsStandalone(t *testing.T) {
	src := newTestDB(t)

	parentID := createIssue(t, src, "in-progress parent", model.StatusInProgress, model.PriorityMedium)
	childID := createChildIssue(t, src, "done child", model.StatusDone, parentID)

	linkedDocID := createDoc(t, src, "linked design doc", "tdd", "draft")
	linkDocIssue(t, src, linkedDocID, childID)
	standaloneDocID := createDoc(t, src, "standalone adr", "adr", "accepted")

	export := runFilteredExport(t, src, []string{string(model.StatusDone)})

	dst := newTestDB(t)
	staleID := createIssue(t, dst, "stale data to be replaced", model.StatusTodo, model.PriorityHigh)

	if _, err := doImport(dst, export, true); err != nil {
		t.Fatalf("doImport(filtered, replace=true): %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst)
	if err != nil {
		t.Fatalf("ListAllIssues(dst): %v", err)
	}
	if len(gotIssues) != 1 || gotIssues[0].ID != childID {
		t.Fatalf("expected only the filtered child after replace, got %+v", gotIssues)
	}
	if gotIssues[0].ID == staleID {
		t.Fatalf("stale issue survived --replace import")
	}
	if gotIssues[0].ParentID != nil {
		t.Errorf("expected dangling parent nulled after filtered replace import, got parent_id=%v", *gotIssues[0].ParentID)
	}

	gotDocs, err := db.ListAllDocs(dst)
	if err != nil {
		t.Fatalf("ListAllDocs(dst): %v", err)
	}
	if len(gotDocs) != 1 || gotDocs[0].ID != linkedDocID {
		t.Errorf("expected only linked doc after filtered replace import, got %+v", gotDocs)
	}
	for _, d := range gotDocs {
		if d.ID == standaloneDocID {
			t.Errorf("standalone doc leaked into filtered replace import: %+v", d)
		}
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Errorf("expected no foreign key violations after filtered replace import, found at least one")
	}
}

func TestFilteredExportReplaceImportRollsBackOnFailure(t *testing.T) {
	src := newTestDB(t)
	parentID := createIssue(t, src, "in-progress parent", model.StatusInProgress, model.PriorityMedium)
	childID := createChildIssue(t, src, "done child", model.StatusDone, parentID)
	docID := createDoc(t, src, "linked doc", "tdd", "draft")
	linkDocIssue(t, src, docID, childID)

	export := runFilteredExport(t, src, []string{string(model.StatusDone)})
	export.DocIssueLinks = append(export.DocIssueLinks, model.DocIssueLink{
		DocID:     docID,
		IssueID:   999999,
		CreatedAt: "2026-01-01T00:00:00Z",
	})

	dst := newTestDB(t)
	survivorID := createIssue(t, dst, "must survive failed replace", model.StatusTodo, model.PriorityHigh)
	if err := db.AddLabelToIssue(dst, survivorID, "keep-me", "", "tester"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	if _, err := doImport(dst, export, true); err == nil {
		t.Fatal("expected doImport(filtered, replace=true) to fail on dangling doc-issue link, got nil")
	}

	gotIssues, err := db.ListAllIssues(dst)
	if err != nil {
		t.Fatalf("ListAllIssues(dst): %v", err)
	}
	if len(gotIssues) != 1 || gotIssues[0].ID != survivorID || gotIssues[0].Title != "must survive failed replace" {
		t.Fatalf("expected pre-existing data preserved after failed filtered replace, got %+v", gotIssues)
	}

	gotLabels, err := db.ListAllLabelsRaw(dst)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw(dst): %v", err)
	}
	if len(gotLabels) != 1 || gotLabels[0].Name != "keep-me" {
		t.Fatalf("expected pre-existing label preserved after failed filtered replace, got %+v", gotLabels)
	}
}

func TestUnfilteredExportIncludesStandaloneDocsAndProposals(t *testing.T) {
	src := newTestDB(t)

	createIssue(t, src, "some issue", model.StatusTodo, model.PriorityMedium)
	createDoc(t, src, "standalone doc", "adr", "accepted")
	if _, err := db.CreateProposal(src, &model.Proposal{
		Description: "standalone proposal", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedBy: "@team-lead",
	}); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	export := runFilteredExport(t, src, nil)

	if len(export.Docs) != 1 {
		t.Errorf("unfiltered export should include standalone doc, got %d docs", len(export.Docs))
	}
	if len(export.Proposals) != 1 {
		t.Errorf("unfiltered export should include standalone proposal, got %d proposals", len(export.Proposals))
	}
}
