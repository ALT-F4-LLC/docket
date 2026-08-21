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

func makeTestVote(summary string) *model.Vote {
	return &model.Vote{
		ProposalID:      1,
		VoterName:       "seat-a",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         summary,
		CreatedAt:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// A multi-line summary must not be padded out to the width of its widest line
// (DKT-518): handing the whole block to one lipgloss Render did exactly that.
func TestRenderProposalDetail_MultiLineSummaryHasNoTrailingWhitespace(t *testing.T) {
	summary := "first line\nsecond line\nthird line\nfourth line\nfifth line"

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

			out := RenderProposalDetail(
				makeTestProposal(1, "Multi-line summary"),
				[]*model.Vote{makeTestVote(summary)},
				nil, nil,
			)

			for i, line := range strings.Split(out, "\n") {
				if line != strings.TrimRight(line, " \t") {
					t.Errorf("line %d has trailing whitespace: %q", i, line)
				}
			}
			for _, want := range []string{"    first line", "    fifth line"} {
				if !strings.Contains(out, want) {
					t.Errorf("summary line not indented as %q:\n%s", want, out)
				}
			}
		})
	}
}

// One long line in a summary must not inflate every other line to its width.
func TestRenderProposalDetail_LongSummaryLineDoesNotPadNeighbours(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	long := strings.Repeat("x", 1600)
	summary := "short one\n" + long + "\nshort two"

	out := RenderProposalDetail(
		makeTestProposal(1, "Long line"),
		[]*model.Vote{makeTestVote(summary)},
		nil, nil,
	)

	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(line, long) {
			continue
		}
		if len(line) > 200 {
			t.Errorf("line %d is %d chars, expected short: %q", i, len(line), line)
		}
	}
	if max := len(summary) + 2000; len(out) > max {
		t.Errorf("rendered %d bytes for a %d byte summary (max %d), expected no bulk padding",
			len(out), len(summary), max)
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
