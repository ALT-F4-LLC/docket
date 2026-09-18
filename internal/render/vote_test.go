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

// sealedTestProposal is a two-seat proposal opened sealed (DKT-2447).
func sealedTestProposal(status model.ProposalStatus) *model.Proposal {
	p := makeTestProposal(1, "Sealed ballot")
	p.RequiredVoters = 2
	p.Sealed = true
	p.Status = status
	return p
}

// sealedRenderCases run one renderer over the same sealed proposal in both
// themes, open and then closed, and check what each state may show.
func sealedRenderCases(t *testing.T, render func(*model.Proposal, []*model.Vote) string) {
	t.Helper()
	vote := makeTestVote("the summary of seat-a")
	vote.FindingsJSON = &model.Findings{Concerns: []model.Finding{{Text: "a concern from seat-a"}}}
	votes := []*model.Vote{vote}

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

			open := render(sealedTestProposal(model.ProposalStatusOpen), votes)
			for _, want := range []string{"seat-a", "1/2", "sealed"} {
				if !strings.Contains(open, want) {
					t.Errorf("sealed-open rendering lacks %q:\n%s", want, open)
				}
			}
			for _, leaked := range []string{string(model.VerdictApprove), "0.90", "0.80", "0.72",
				"the summary of seat-a", "a concern from seat-a"} {
				if strings.Contains(open, leaked) {
					t.Errorf("sealed-open rendering leaks %q:\n%s", leaked, open)
				}
			}

			closed := render(sealedTestProposal(model.ProposalStatusApproved), votes)
			for _, want := range []string{"seat-a", string(model.VerdictApprove), "0.90"} {
				if !strings.Contains(closed, want) {
					t.Errorf("closed sealed rendering lacks %q:\n%s", want, closed)
				}
			}
		})
	}
}

// A sealed proposal's detail view withholds every cast to a name and a count
// while open, and renders everything once the tally closes it (DKT-2447).
func TestRenderProposalDetail_SealedOpenRendersNamesOnly(t *testing.T) {
	sealedRenderCases(t, func(p *model.Proposal, votes []*model.Vote) string {
		return RenderProposalDetail(p, votes, nil, nil)
	})
}

// The same rule on the result view: no breakdown table while sealed and open.
func TestRenderVoteResult_SealedOpenRendersNamesOnly(t *testing.T) {
	sealedRenderCases(t, RenderVoteResult)
}
