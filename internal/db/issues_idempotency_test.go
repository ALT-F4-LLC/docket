package db

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// issueRequest is one CreateIssueIdempotent call's content.
type issueRequest struct {
	issue  *model.Issue
	labels []string
	files  []string
}

func baseIssueRequest() issueRequest {
	return issueRequest{
		issue: &model.Issue{
			Title:       "fingerprinted",
			Description: "the body",
			Status:      model.StatusTodo,
			Priority:    model.PriorityMedium,
			Kind:        model.IssueKindTask,
		},
		labels: []string{"alpha", "beta"},
		files:  []string{"a.go", "b.go"},
	}
}

// A key reused with different content is a conflict naming the key, and the
// rejected call inserts nothing.
func TestCreateIssueIdempotentRejectsKeyReuseWithDifferentContent(t *testing.T) {
	cases := map[string]func(r *issueRequest){
		"title":       func(r *issueRequest) { r.issue.Title = "other title" },
		"description": func(r *issueRequest) { r.issue.Description = "other body" },
		"kind":        func(r *issueRequest) { r.issue.Kind = model.IssueKindBug },
		"priority":    func(r *issueRequest) { r.issue.Priority = model.PriorityHigh },
		"labels":      func(r *issueRequest) { r.labels = []string{"alpha"} },
		"files":       func(r *issueRequest) { r.files = []string{"a.go", "c.go"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			db := newIdemDB(t)
			first := baseIssueRequest()
			_, _, err := CreateIssueIdempotent(db, first.issue, first.labels, first.files, "reused-key")
			testsupport.Must(t, err, "first create: %v", err)

			second := baseIssueRequest()
			change(&second)
			_, inserted, err := CreateIssueIdempotent(db, second.issue, second.labels, second.files, "reused-key")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reuse with a different %s returned %v, want ErrConflict", name, err)
			}
			if !strings.Contains(err.Error(), "reused-key") {
				t.Errorf("conflict %q does not name the key", err)
			}
			if inserted {
				t.Error("the rejected create reported inserted=true")
			}
			if n := countIssues(t, db); n != 1 {
				t.Errorf("issue count = %d, want 1", n)
			}
		})
	}
}

// Identical content under a reused key replays the original issue, however the
// fields that are not request content differ between the calls.
func TestCreateIssueIdempotentReplaysIdenticalContent(t *testing.T) {
	db := newIdemDB(t)

	first := baseIssueRequest()
	first.issue.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	first.issue.UpdatedAt = first.issue.CreatedAt
	original, _, err := CreateIssueIdempotent(db, first.issue, first.labels, first.files, "same-key")
	testsupport.Must(t, err, "first create: %v", err)

	second := baseIssueRequest()
	second.issue.ID = 99
	second.issue.CreatedAt = time.Now().UTC()
	second.issue.UpdatedAt = second.issue.CreatedAt.Add(time.Hour)
	replay, inserted, err := CreateIssueIdempotent(db, second.issue, second.labels, second.files, "same-key")
	testsupport.Must(t, err, "replay: %v", err)

	if replay != original || inserted {
		t.Errorf("replay = (%d, inserted %v), want the original %d with inserted false",
			replay, inserted, original)
	}
	if n := countIssues(t, db); n != 1 {
		t.Errorf("issue count = %d, want 1", n)
	}
}

// No labels or files given as nil and as empty lists is the same request: the
// CLI passes empty flag slices where a store caller passes nil.
func TestCreateIssueIdempotentTreatsNilAndEmptyListsAlike(t *testing.T) {
	db := newIdemDB(t)

	original, _, err := CreateIssueIdempotent(db, baseIssueRequest().issue, nil, nil, "no-lists")
	testsupport.Must(t, err, "first create: %v", err)
	replay, _, err := CreateIssueIdempotent(db, baseIssueRequest().issue, []string{}, []string{}, "no-lists")
	testsupport.Must(t, err, "replay with empty lists: %v", err)

	if replay != original {
		t.Errorf("replay returned issue %d, want the original %d", replay, original)
	}
}
