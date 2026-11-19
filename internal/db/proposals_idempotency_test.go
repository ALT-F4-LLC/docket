package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func baseProposalRequest() *model.Proposal {
	return &model.Proposal{
		ProjectID:      DefaultProjectID,
		Description:    "fingerprinted",
		Criticality:    model.CriticalityMedium,
		Threshold:      0.6,
		CreatedBy:      "alice",
		DomainTags:     []string{"cli", "db"},
		FilesChanged:   []string{"a.go"},
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
	}
}

func countProposalRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&n)
	testsupport.Must(t, err, "counting proposals: %v", err)
	return n
}

// A key reused with different caller-supplied content is a conflict naming the
// key, and the rejected call inserts nothing.
func TestCreateProposalIdempotentRejectsKeyReuseWithDifferentContent(t *testing.T) {
	cases := map[string]func(p *model.Proposal){
		"project":         func(p *model.Proposal) { p.ProjectID = DefaultProjectID + 1 },
		"description":     func(p *model.Proposal) { p.Description = "other" },
		"criticality":     func(p *model.Proposal) { p.Criticality = model.CriticalityHigh },
		"threshold":       func(p *model.Proposal) { p.Threshold = 0.9 },
		"created by":      func(p *model.Proposal) { p.CreatedBy = "mallory" },
		"domain tags":     func(p *model.Proposal) { p.DomainTags = []string{"cli"} },
		"files changed":   func(p *model.Proposal) { p.FilesChanged = []string{"b.go"} },
		"sealed":          func(p *model.Proposal) { p.Sealed = true },
		"hold on dissent": func(p *model.Proposal) { p.HoldOnDissent = true },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			db := newIdemDB(t)
			_, err := CreateProposalIdempotent(db, baseProposalRequest(), "reused-key")
			testsupport.Must(t, err, "first create: %v", err)

			second := baseProposalRequest()
			change(second)
			_, err = CreateProposalIdempotent(db, second, "reused-key")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reuse with a different %s returned %v, want ErrConflict", name, err)
			}
			if !strings.Contains(err.Error(), "reused-key") {
				t.Errorf("conflict %q does not name the key", err)
			}
			if n := countProposalRows(t, db); n != 1 {
				t.Errorf("proposal count = %d, want 1", n)
			}
		})
	}
}

// Identical caller-supplied content under a reused key replays the original
// proposal, however the fields outside the fingerprint differ between calls.
func TestCreateProposalIdempotentReplaysIdenticalContent(t *testing.T) {
	db := newIdemDB(t)

	first := baseProposalRequest()
	first.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	first.UpdatedAt = first.CreatedAt
	original, err := CreateProposalIdempotent(db, first, "same-key")
	testsupport.Must(t, err, "first create: %v", err)

	score := 0.5
	reason := "escalated"
	second := baseProposalRequest()
	second.ID = 99
	second.Status = model.ProposalStatusApproved
	second.WeightedScore = &score
	second.CreatedAt = time.Now().UTC()
	second.UpdatedAt = second.CreatedAt.Add(time.Hour)
	second.FinalOutcome = "approved"
	second.EscalationReason = &reason
	second.Rationale = "a different rationale"
	second.RequiredVoters = 5
	replay, err := CreateProposalIdempotent(db, second, "same-key")
	testsupport.Must(t, err, "replay: %v", err)

	if replay != original {
		t.Errorf("replay returned proposal %d, want the original %d", replay, original)
	}
	if n := countProposalRows(t, db); n != 1 {
		t.Errorf("proposal count = %d, want 1", n)
	}
}

// No domain tags or changed files given as nil and as empty lists is the same
// request.
func TestCreateProposalIdempotentTreatsNilAndEmptyListsAlike(t *testing.T) {
	db := newIdemDB(t)

	first := baseProposalRequest()
	first.DomainTags, first.FilesChanged = nil, nil
	original, err := CreateProposalIdempotent(db, first, "no-lists")
	testsupport.Must(t, err, "first create: %v", err)

	second := baseProposalRequest()
	second.DomainTags, second.FilesChanged = []string{}, []string{}
	replay, err := CreateProposalIdempotent(db, second, "no-lists")
	testsupport.Must(t, err, "replay with empty lists: %v", err)

	if replay != original {
		t.Errorf("replay returned proposal %d, want the original %d", replay, original)
	}
}
