package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// buildExport reads a whole database into an export document, the way the
// export command does — through the same table, so a collection the command
// would carry can never be one this helper leaves behind.
func buildExport(t *testing.T, conn *sql.DB) *model.ExportData {
	t.Helper()

	export := &model.ExportData{
		Version:    1,
		ExportedAt: "2026-01-01T00:00:00Z",
	}
	for _, c := range exportCollections {
		err := c.fetch(conn, 0, export)
		testsupport.Must(t, err, "fetching %s: %v", c.name, err)
	}
	return export
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
	if _, err := doImport(dst, export, false, 1); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotDocs, err := db.ListAllDocs(dst, 0)
	testsupport.Must(t, err, "ListAllDocs(dst, 0): %v", err)
	if len(gotDocs) != 2 {
		t.Fatalf("expected 2 docs after import, got %d", len(gotDocs))
	}

	gotRevisions, err := db.ListAllDocRevisions(dst, 0)
	testsupport.Must(t, err, "ListAllDocRevisions(dst, 0): %v", err)
	// docID: create + body edit = 2 revisions; otherDocID: create = 1 revision.
	if len(gotRevisions) != 3 {
		t.Fatalf("expected 3 doc revisions after import, got %d", len(gotRevisions))
	}

	gotComments, err := db.ListAllDocComments(dst, 0)
	testsupport.Must(t, err, "ListAllDocComments(dst, 0): %v", err)
	if len(gotComments) != 3 {
		t.Fatalf("expected 3 doc comments after import, got %d", len(gotComments))
	}

	gotLinks, err := db.ListAllDocIssueLinks(dst, 0)
	testsupport.Must(t, err, "ListAllDocIssueLinks(dst, 0): %v", err)
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
	testsupport.Must(t, err, "CreateProposal: %v", err)

	if _, err := db.CastVote(src, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "@senior-engineer",
		VoterRole:       "senior-engineer",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         "looks correct",
		FindingsJSON:    &model.Findings{Blockers: nil, Concerns: []string{"one nit"}, Suggestions: []string{"rename x"}},
		Metadata:        map[string]any{"resolved": map[string]any{"engine": "sonnet-5"}},
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	// The control for the metadata assertion below: a seat that claimed
	// nothing must arrive claiming nothing, not carrying its neighbour's bag.
	if _, err := db.CastVote(src, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "@staff-engineer",
		VoterRole:       "staff-engineer",
		Verdict:         model.VerdictApprove,
		Confidence:      0.8,
		DomainRelevance: 0.7,
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	err = db.LinkProposalIssue(src, proposalID, issueID)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)
	err = db.LinkProposalDoc(src, proposalID, docID)
	testsupport.Must(t, err, "LinkProposalDoc: %v", err)

	export := buildExport(t, src)

	dst := newTestDB(t)
	if _, err := doImport(dst, export, false, 1); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotProposals, err := db.ListAllProposals(dst, 0)
	testsupport.Must(t, err, "ListAllProposals(dst, 0): %v", err)
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

	gotVotes, err := db.ListAllVotes(dst, 0)
	testsupport.Must(t, err, "ListAllVotes(dst, 0): %v", err)
	if len(gotVotes) != 2 {
		t.Fatalf("expected 2 votes after import, got %d", len(gotVotes))
	}
	// Keyed by voter, never by position: the round trip now carries two seats
	// precisely so one can be the control, and a third seat or a changed
	// ordering would silently re-point both assertions at the wrong vote.
	importedByVoter := map[string]*model.Vote{}
	for _, v := range gotVotes {
		importedByVoter[v.VoterName] = v
	}
	claimed, ok := importedByVoter["@senior-engineer"]
	if !ok {
		t.Fatalf("no vote imported for @senior-engineer")
	}
	control, ok := importedByVoter["@staff-engineer"]
	if !ok {
		t.Fatalf("no vote imported for @staff-engineer")
	}
	if claimed.FindingsJSON == nil || len(claimed.FindingsJSON.Concerns) != 1 {
		t.Errorf("expected findings_json to round-trip with 1 concern, got %+v", claimed.FindingsJSON)
	}
	wantMetadata := map[string]any{"resolved": map[string]any{"engine": "sonnet-5"}}
	if !reflect.DeepEqual(claimed.Metadata, wantMetadata) {
		t.Errorf("expected vote metadata to round-trip as %#v, got %#v", wantMetadata, claimed.Metadata)
	}
	if control.Metadata != nil {
		t.Errorf("expected the unclaimed vote's metadata to stay absent, got %#v", control.Metadata)
	}

	gotProposalIssues, err := db.ListAllProposalIssues(dst, 0)
	testsupport.Must(t, err, "ListAllProposalIssues(dst, 0): %v", err)
	if len(gotProposalIssues) != 1 || gotProposalIssues[0].ProposalID != proposalID || gotProposalIssues[0].IssueID != issueID {
		t.Errorf("expected proposal-issue link (%d,%d), got %+v", proposalID, issueID, gotProposalIssues)
	}

	gotProposalDocs, err := db.ListAllProposalDocs(dst, 0)
	testsupport.Must(t, err, "ListAllProposalDocs(dst, 0): %v", err)
	if len(gotProposalDocs) != 1 || gotProposalDocs[0].ProposalID != proposalID || gotProposalDocs[0].DocID != docID {
		t.Errorf("expected proposal-doc link (%d,%d), got %+v", proposalID, docID, gotProposalDocs)
	}
}

func TestDoImportRoundTripPreservesActivityLog(t *testing.T) {
	src := newTestDB(t)

	issueID := createIssue(t, src, "tracked issue", model.StatusTodo, model.PriorityMedium)
	err := db.RecordActivity(src, issueID, "status", "todo", "in-progress", "@senior-engineer")
	testsupport.Must(t, err, "RecordActivity: %v", err)

	wantActivity, err := db.ListAllActivity(src, 0)
	testsupport.Must(t, err, "ListAllActivity(src, 0): %v", err)
	if len(wantActivity) < 2 {
		t.Fatalf("expected at least 2 activity rows in source (created + status), got %d", len(wantActivity))
	}

	export := buildExport(t, src)

	dst := newTestDB(t)
	err = db.ClearAllData(dst)
	testsupport.Must(t, err, "ClearAllData(dst): %v", err)
	if _, err := doImport(dst, export, false, 1); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	gotActivity, err := db.ListAllActivity(dst, 0)
	testsupport.Must(t, err, "ListAllActivity(dst, 0): %v", err)
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
	testsupport.Must(t, err, "PRAGMA foreign_key_check: %v", err)
	defer rows.Close()
	if rows.Next() {
		t.Errorf("expected no foreign key violations after import, found at least one")
	}
}

func TestDoImportReplaceRollsBackOnFailure(t *testing.T) {
	dst := newTestDB(t)
	seededIssueID := createIssue(t, dst, "must survive", model.StatusTodo, model.PriorityHigh)
	err := db.AddLabelToIssue(dst, seededIssueID, "keep-me", "", "tester")
	testsupport.Must(t, err, "AddLabelToIssue: %v", err)

	src := newTestDB(t)
	createIssue(t, src, "incoming", model.StatusTodo, model.PriorityMedium)
	docID := createDoc(t, src, "incoming doc", "tdd", "draft")

	export := buildExport(t, src)
	export.DocIssueLinks = append(export.DocIssueLinks, model.DocIssueLink{
		DocID:     docID,
		IssueID:   999999,
		CreatedAt: "2026-01-01T00:00:00Z",
	})

	if _, err := doImport(dst, export, true, 1); err == nil {
		t.Fatal("expected doImport(replace=true) to fail on dangling doc-issue link, got nil")
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != seededIssueID || gotIssues[0].Title != "must survive" {
		t.Fatalf("expected seeded issue preserved after rollback, got %+v", gotIssues)
	}

	gotLabels, err := db.ListAllLabelsRaw(dst, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw(dst, 0): %v", err)
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

	if _, err := doImport(dst, export, true, 1); err != nil {
		t.Fatalf("doImport(replace=true): %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
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
	err := cmd.Flags().Set("file", outPath)
	testsupport.Must(t, err, "set file flag: %v", err)
	for _, s := range statuses {
		err := cmd.Flags().Set("status", s)
		testsupport.Must(t, err, "set status flag: %v", err)
	}

	err = exportCmd.RunE(cmd, nil)
	testsupport.Must(t, err, "exportCmd.RunE: %v", err)

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", outPath, err)
	}
	var export model.ExportData
	err = json.Unmarshal(raw, &export)
	testsupport.Must(t, err, "Unmarshal export: %v", err)
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
	testsupport.Must(t, err, "CreateProposal(linked): %v", err)
	if _, err := db.CastVote(src, &model.Vote{
		ProposalID: linkedProposalID, VoterName: "@senior-engineer", VoterRole: "senior-engineer",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8, Summary: "ok",
	}); err != nil {
		t.Fatalf("CastVote(linked): %v", err)
	}
	err = db.LinkProposalIssue(src, linkedProposalID, childID)
	testsupport.Must(t, err, "LinkProposalIssue(linked): %v", err)
	err = db.LinkProposalDoc(src, linkedProposalID, linkedDocID)
	testsupport.Must(t, err, "LinkProposalDoc(linked): %v", err)

	standaloneProposalID, err := db.CreateProposal(src, &model.Proposal{
		Description: "standalone proposal", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedBy: "@team-lead",
	})
	testsupport.Must(t, err, "CreateProposal(standalone): %v", err)
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
	err = db.ClearAllData(dst)
	testsupport.Must(t, err, "ClearAllData(dst): %v", err)
	if _, err := doImport(dst, export, false, 1); err != nil {
		t.Fatalf("doImport of filtered export: %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != childID {
		t.Fatalf("expected single child issue imported, got %+v", gotIssues)
	}
	if gotIssues[0].ParentID != nil {
		t.Errorf("expected imported child to have NULL parent_id, got %v", *gotIssues[0].ParentID)
	}

	gotDocs, err := db.ListAllDocs(dst, 0)
	testsupport.Must(t, err, "ListAllDocs(dst, 0): %v", err)
	if len(gotDocs) != 1 || gotDocs[0].ID != linkedDocID {
		t.Errorf("expected only linked doc imported, got %+v", gotDocs)
	}

	gotProposals, err := db.ListAllProposals(dst, 0)
	testsupport.Must(t, err, "ListAllProposals(dst, 0): %v", err)
	if len(gotProposals) != 1 || gotProposals[0].ID != linkedProposalID {
		t.Errorf("expected only linked proposal imported, got %+v", gotProposals)
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	testsupport.Must(t, err, "PRAGMA foreign_key_check: %v", err)
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

	if _, err := doImport(dst, export, true, 1); err != nil {
		t.Fatalf("doImport(filtered, replace=true): %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != childID {
		t.Fatalf("expected only the filtered child after replace, got %+v", gotIssues)
	}
	if gotIssues[0].ID == staleID {
		t.Fatalf("stale issue survived --replace import")
	}
	if gotIssues[0].ParentID != nil {
		t.Errorf("expected dangling parent nulled after filtered replace import, got parent_id=%v", *gotIssues[0].ParentID)
	}

	gotDocs, err := db.ListAllDocs(dst, 0)
	testsupport.Must(t, err, "ListAllDocs(dst, 0): %v", err)
	if len(gotDocs) != 1 || gotDocs[0].ID != linkedDocID {
		t.Errorf("expected only linked doc after filtered replace import, got %+v", gotDocs)
	}
	for _, d := range gotDocs {
		if d.ID == standaloneDocID {
			t.Errorf("standalone doc leaked into filtered replace import: %+v", d)
		}
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	testsupport.Must(t, err, "PRAGMA foreign_key_check: %v", err)
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
	err := db.AddLabelToIssue(dst, survivorID, "keep-me", "", "tester")
	testsupport.Must(t, err, "AddLabelToIssue: %v", err)

	if _, err := doImport(dst, export, true, 1); err == nil {
		t.Fatal("expected doImport(filtered, replace=true) to fail on dangling doc-issue link, got nil")
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != survivorID || gotIssues[0].Title != "must survive failed replace" {
		t.Fatalf("expected pre-existing data preserved after failed filtered replace, got %+v", gotIssues)
	}

	gotLabels, err := db.ListAllLabelsRaw(dst, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw(dst, 0): %v", err)
	if len(gotLabels) != 1 || gotLabels[0].Name != "keep-me" {
		t.Fatalf("expected pre-existing label preserved after failed filtered replace, got %+v", gotLabels)
	}
}

// seedEveryCollection writes at least one row into every collection the export
// table knows about. A round-trip assertion is only worth as much as its
// fixture: a collection that is empty on both sides round-trips perfectly by
// doing nothing at all, which is exactly the outcome a dropped collection
// produces.
func seedEveryCollection(t *testing.T, conn *sql.DB) {
	t.Helper()

	parentID := createIssue(t, conn, "parent issue", model.StatusInProgress, model.PriorityMedium)
	childID := createChildIssue(t, conn, "child issue", model.StatusTodo, parentID)
	blockedID := createIssueWithFile(t, conn, "issue with a file", "internal/cli/export.go")

	err := db.AddLabelToIssue(conn, childID, "keep-me", "", "tester")
	testsupport.Must(t, err, "AddLabelToIssue: %v", err)

	_, err = db.CreateComment(conn, &model.Comment{IssueID: childID, Body: "a comment", Author: "tester"})
	testsupport.Must(t, err, "CreateComment: %v", err)

	_, err = db.CreateRelation(conn, &model.Relation{
		SourceIssueID: childID,
		TargetIssueID: blockedID,
		RelationType:  model.RelationBlocks,
	})
	testsupport.Must(t, err, "CreateRelation: %v", err)

	err = db.RecordActivity(conn, childID, "status", "todo", "in-progress", "tester")
	testsupport.Must(t, err, "RecordActivity: %v", err)

	docID := createDoc(t, conn, "design doc", "tdd", "draft")
	revisedBody := "revised body"
	_, err = db.UpdateDoc(conn, docID, db.DocUpdate{Body: &revisedBody, Author: "tester"})
	testsupport.Must(t, err, "UpdateDoc: %v", err)
	_, err = db.CreateDocComment(conn, &model.DocComment{DocID: docID, Body: "a doc comment", Author: "tester"})
	testsupport.Must(t, err, "CreateDocComment: %v", err)
	linkDocIssue(t, conn, docID, childID)

	proposalID, err := db.CreateProposal(conn, &model.Proposal{
		Description: "should we ship", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedBy: "@team-lead",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	if _, err := db.CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: "@senior-engineer", VoterRole: "senior-engineer",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8, Summary: "ok",
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	err = db.LinkProposalIssue(conn, proposalID, childID)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)
	err = db.LinkProposalDoc(conn, proposalID, docID)
	testsupport.Must(t, err, "LinkProposalDoc: %v", err)
}

// TestRoundTripPreservesEveryCollection is the check that catches export/import
// skew. It counts every collection out of a seeded database and back in again,
// so a collection that export writes and import ignores fails here with the
// name of the collection rather than being discovered later by whoever went
// looking for the rows.
//
// It walks the same table export and import walk, so a sixteenth collection is
// covered the moment it is added — the fixture is the only thing its author has
// to extend, and the guard above says so when they forget.
func TestRoundTripPreservesEveryCollection(t *testing.T) {
	src := newTestDB(t)
	seedEveryCollection(t, src)

	export := buildExport(t, src)
	for _, c := range exportCollections {
		if c.count(export) == 0 {
			t.Fatalf("fixture seeds no %s, so this test would prove nothing about that collection", c.name)
		}
	}

	dst := newTestDB(t)
	err := db.ClearAllData(dst)
	testsupport.Must(t, err, "ClearAllData(dst): %v", err)
	if _, err := doImport(dst, export, false, 1); err != nil {
		t.Fatalf("doImport: %v", err)
	}

	imported := buildExport(t, dst)
	for _, c := range exportCollections {
		if got, want := c.count(imported), c.count(export); got != want {
			t.Errorf("%s: exported %d rows, imported %d", c.name, want, got)
		}
	}

	rows, err := dst.Query("PRAGMA foreign_key_check")
	testsupport.Must(t, err, "PRAGMA foreign_key_check: %v", err)
	defer rows.Close()
	if rows.Next() {
		t.Errorf("expected no foreign key violations after import, found at least one")
	}
}

// runImportCmd drives a FRESH import command (newImportCmd) through cobra's
// own arg parsing, the way a shell invocation would: SetArgs + ExecuteContext
// on a pristine command, per runTrustVerb's pattern in trust_event_test.go.
// Building a new instance per call (rather than reusing the package's
// importCmd singleton or hand-registering a stand-in flag set) means the
// command under test carries the SAME --merge/--replace/--yes registration a
// real invocation parses; a case that stopped registering --yes would fail
// here with "unknown flag: --yes" instead of silently testing nothing.
func runImportCmd(t *testing.T, conn *sql.DB, filePath string, jsonMode bool, replace, yes bool) error {
	t.Helper()

	cmd := newImportCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	// --json and --quiet are persistent flags rootCmd normally supplies;
	// newImportCmd has no parent in this test, so register the ones RunE
	// reads directly on the fresh command.
	cmd.Flags().String("json", "", "")
	cmd.Flags().Bool("quiet", false, "")

	args := []string{filePath}
	if jsonMode {
		// The real --json is a string flag with NoOptDefVal "v1" (root.go),
		// so a bare "--json" is legal there; this ad-hoc flag has no such
		// default, and a bare "--json" would swallow the next argv token as
		// its value instead.
		args = append(args, "--json=v1")
	}
	if replace {
		args = append(args, "--replace")
	}
	if yes {
		args = append(args, "--yes")
	}
	cmd.SetArgs(args)

	ctx := context.WithValue(context.Background(), dbKey, conn)
	return cmd.ExecuteContext(ctx)
}

// seedAndWriteExportFile creates one issue ("seed issue") in conn and writes
// its export to a temp file, returning the file's path. The name says both
// halves because TestImportReplaceProceedsWithYesInJSONMode asserts on the
// seeded issue's title, not only on the file it produces.
func seedAndWriteExportFile(t *testing.T, conn *sql.DB) string {
	t.Helper()

	createIssue(t, conn, "seed issue", model.StatusTodo, model.PriorityMedium)
	export := buildExport(t, conn)

	data, err := json.Marshal(export)
	testsupport.Must(t, err, "marshal export: %v", err)

	path := filepath.Join(t.TempDir(), "export.json")
	err = os.WriteFile(path, data, 0o600)
	testsupport.Must(t, err, "WriteFile: %v", err)
	return path
}

// TestImportReplaceRequiresYesInJSONMode pins DKT-15: --json must not double
// as consent for --replace's destructive project-data wipe.
func TestImportReplaceRequiresYesInJSONMode(t *testing.T) {
	src := newTestDB(t)
	filePath := seedAndWriteExportFile(t, src)

	dst := newTestDB(t)
	survivorID := createIssue(t, dst, "must survive refused replace", model.StatusTodo, model.PriorityHigh)

	err := runImportCmd(t, dst, filePath, true, true, false)
	if err == nil {
		t.Fatal("expected import --replace --json without --yes to be refused, got nil")
	}
	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) || cmdErr.Code != output.ErrValidation {
		t.Fatalf("expected VALIDATION_ERROR, got %v", err)
	}
	if !strings.Contains(cmdErr.Error(), "--yes") {
		t.Fatalf("expected the refusal to name --yes, got %q", cmdErr.Error())
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != survivorID {
		t.Fatalf("expected refused replace to leave existing data untouched, got %+v", gotIssues)
	}
}

// TestImportReplaceRequiresYesInHumanMode is TestImportReplaceRequiresYesInJSONMode's
// human-mode twin: the consent gate (import.go's `if !yes`) no longer branches
// on JSON mode or terminal attachment at all, so a human-mode invocation must
// refuse identically. Before that fix this half was unpinned: a mutant that
// narrowed the gate to JSON-mode-only kept the whole suite green (finding
// DKT-18-C6).
func TestImportReplaceRequiresYesInHumanMode(t *testing.T) {
	src := newTestDB(t)
	filePath := seedAndWriteExportFile(t, src)

	dst := newTestDB(t)
	survivorID := createIssue(t, dst, "must survive refused replace", model.StatusTodo, model.PriorityHigh)

	err := runImportCmd(t, dst, filePath, false, true, false)
	if err == nil {
		t.Fatal("expected import --replace without --yes to be refused in human mode too, got nil")
	}
	var cmdErr *CmdError
	if !errors.As(err, &cmdErr) || cmdErr.Code != output.ErrValidation {
		t.Fatalf("expected VALIDATION_ERROR, got %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].ID != survivorID {
		t.Fatalf("expected refused replace to leave existing data untouched, got %+v", gotIssues)
	}
}

// TestImportReplaceProceedsWithYesInJSONMode is the affirmative half: once
// --yes is present, --json mode must still perform the replace. This test
// alone is a regression control, not consent evidence — it also passes
// against the pre-fix code, since pre-fix JSON mode always proceeded
// unconfirmed. TestImportReplaceRequiresYesInJSONMode carries the consent
// proof (DKT-18-C13).
func TestImportReplaceProceedsWithYesInJSONMode(t *testing.T) {
	src := newTestDB(t)
	filePath := seedAndWriteExportFile(t, src)

	dst := newTestDB(t)
	createIssue(t, dst, "old data", model.StatusTodo, model.PriorityHigh)

	restore := captureStdout(t)
	err := runImportCmd(t, dst, filePath, true, true, true)
	stdout := restore()
	if err != nil {
		t.Fatalf("import --replace --json --yes: %v", err)
	}

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Imported int `json:"imported"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &envelope); jsonErr != nil {
		t.Fatalf("decoding the writer's JSON envelope: %v (stdout: %q)", jsonErr, stdout)
	}
	if !envelope.OK {
		t.Fatalf("expected the success envelope's ok true, got %+v (stdout: %q)", envelope, stdout)
	}
	if envelope.Data.Imported == 0 {
		t.Fatalf("expected the envelope to report at least 1 imported entity, got %+v", envelope)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].Title != "seed issue" {
		t.Fatalf("expected only imported issue after confirmed replace, got %+v", gotIssues)
	}
}

// TestImportReplaceProceedsWithYesInHumanMode is the human-mode twin of
// TestImportReplaceProceedsWithYesInJSONMode.
func TestImportReplaceProceedsWithYesInHumanMode(t *testing.T) {
	src := newTestDB(t)
	filePath := seedAndWriteExportFile(t, src)

	dst := newTestDB(t)
	createIssue(t, dst, "old data", model.StatusTodo, model.PriorityHigh)

	if err := runImportCmd(t, dst, filePath, false, true, true); err != nil {
		t.Fatalf("import --replace --yes: %v", err)
	}

	gotIssues, err := db.ListAllIssues(dst, 0)
	testsupport.Must(t, err, "ListAllIssues(dst, 0): %v", err)
	if len(gotIssues) != 1 || gotIssues[0].Title != "seed issue" {
		t.Fatalf("expected only imported issue after confirmed replace, got %+v", gotIssues)
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
