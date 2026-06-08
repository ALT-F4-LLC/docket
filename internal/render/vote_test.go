package render

import (
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func makeTestProposal(id int, description string) *model.Proposal {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &model.Proposal{
		ID:             id,
		Description:    description,
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func TestRenderProposalDetail_RendersLinkedDocsStyled(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	out := RenderProposalDetail(makeTestProposal(1, "Ratify TDD"), nil, nil, []int{1, 2})

	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("styled output missing Linked Docs header:\n%s", out)
	}
	for _, want := range []string{"DOC-1", "DOC-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("styled output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "DOC-1") > strings.Index(out, "DOC-2") {
		t.Errorf("docs not ordered by id ascending:\n%s", out)
	}
}

func TestRenderProposalDetail_RendersLinkedDocsPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	out := RenderProposalDetail(makeTestProposal(1, "Ratify TDD"), nil, nil, []int{3})

	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("plain output missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "  DOC-3") {
		t.Errorf("plain output missing expected indented doc line:\n%s", out)
	}
}

func TestRenderProposalDetail_OmitsLinkedDocsWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noColor bool
	}{
		{"styled", false},
		{"plain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noColor {
				t.Setenv("NO_COLOR", "1")
			} else {
				t.Setenv("TERM", "xterm-256color")
			}

			out := RenderProposalDetail(makeTestProposal(1, "No docs"), nil, nil, nil)

			if strings.Contains(out, "Linked Docs") {
				t.Errorf("empty docs should omit Linked Docs section:\n%s", out)
			}
		})
	}
}
