package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DKT-594 — the RENDERED document, which is what a post-mortem reader has.
//
// RUN-32's readers each went to git to find out that `ui-change@8` was five
// registered versions behind before trusting a finding; RUN-39's read the
// repin's event seqs against step ids by hand to learn which contract bytes a
// completed step had actually consumed. Neither fact was in this document.

// staleRunReport is the RUN-32/RUN-39 shape: two pinned workflows, one of them
// well behind the corpus, and a run whose agreement moved mid-flight with steps
// on both sides of the move.
func staleRunReport() *engine.RunReport {
	return &engine.RunReport{
		Run: &model.Run{ID: 39, Status: model.RunActive},
		PinnedWorkflows: []engine.PinnedWorkflowStaleness{
			{Ref: "standard-change@2", Name: "standard-change",
				PinnedVersion: 2, CurrentVersion: 2, Behind: 0},
			{Ref: "ui-change@8", Name: "ui-change",
				PinnedVersion: 8, CurrentVersion: 13, Behind: 5},
		},
		PinEpochs: []engine.PinEpoch{
			{Epoch: 1, FromSeq: 5301, Origin: engine.PinEpochActivation},
			{
				Epoch: 2, FromSeq: 5375, Origin: engine.PinEpochRepin,
				Reason: "corpus install 2026-08-23",
				Changes: []engine.RepinChange{{
					Kind: db.PinKindFile, Ref: "contracts/implement.md",
					OldSHA256: "aaaaaaaaaaaaaaaaaaaa", NewSHA256: "bbbbbbbbbbbbbbbbbbbb",
				}},
			},
		},
		Attempts: []engine.StepAttempt{
			{
				Step: "STEP-1350", Instance: "implement@0", Issue: "DKT-1",
				Status: db.StepDone, Attempts: 1, PinEpoch: 1,
			},
			{
				Step: "STEP-1353", Instance: "verify@0", Issue: "DKT-2",
				Status: db.StepDone, Attempts: 1, PinEpoch: 2,
			},
		},
	}
}

// TestReportPublishesTheStalenessCount is criterion 1: the subtraction is in
// the document, naming the version the corpus has reached.
func TestReportPublishesTheStalenessCount(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(staleRunReport()), "Pinned workflows")
	if len(lines) != 2 {
		t.Fatalf("Pinned workflows printed %d rows, want one per pinned workflow: %v",
			len(lines), lines)
	}
	if !linesContain(lines, `"ui-change@8"`) || !linesContain(lines, "5 version(s) behind") {
		t.Errorf("no staleness count for the drifted pin:\n  %s", strings.Join(lines, "\n  "))
	}
	if !linesContain(lines, `"ui-change@13"`) {
		t.Errorf("the staleness line does not name the version the corpus reached:\n  %s",
			strings.Join(lines, "\n  "))
	}
}

// TestAnUpToDatePinStillGetsALine is the point of the section, not an edge
// case: "the report did not mention it" is the same absence that sent RUN-32's
// readers to git.
func TestAnUpToDatePinStillGetsALine(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(staleRunReport()), "Pinned workflows")
	for _, l := range lines {
		if strings.Contains(l, `"standard-change@2"`) {
			if !strings.Contains(l, "current") {
				t.Errorf("the up-to-date pin's line does not say so:\n  %s", l)
			}
			return
		}
	}
	t.Errorf("the up-to-date pin has no line at all:\n  %s", strings.Join(lines, "\n  "))
}

// TestReportPartitionsStepsByPinEpoch is criterion 2: the grouping RUN-39's
// post-mortem assembled by hand, rendered.
func TestReportPartitionsStepsByPinEpoch(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(staleRunReport()), "Pin epochs")

	for _, want := range []string{
		"Epoch 1:", "activation at seq 5301",
		"Epoch 2:", "repin at seq 5375",
		"contracts/implement.md", "corpus install 2026-08-23",
	} {
		if !linesContain(lines, want) {
			t.Errorf("the epoch timeline is missing %q:\n  %s",
				want, strings.Join(lines, "\n  "))
		}
	}

	// The partition itself: each step is listed under the agreement its
	// recorded work ran under, and under no other.
	for _, tc := range []struct{ epoch, step, other string }{
		{"ran under 1:", "DKT-1 implement@0", "DKT-2 verify@0"},
		{"ran under 2:", "DKT-2 verify@0", "DKT-1 implement@0"},
	} {
		var found bool
		for _, l := range lines {
			if !strings.HasPrefix(l, tc.epoch) {
				continue
			}
			found = true
			if !strings.Contains(l, tc.step) {
				t.Errorf("%s does not list %s:\n  %s", tc.epoch, tc.step, l)
			}
			if strings.Contains(l, tc.other) {
				t.Errorf("%s also lists %s, which ran under the other agreement:\n  %s",
					tc.epoch, tc.other, l)
			}
		}
		if !found {
			t.Errorf("no %q line:\n  %s", tc.epoch, strings.Join(lines, "\n  "))
		}
	}
}

// TestARunWithOneAgreementHasNoEpochSection is the falsifier for the section's
// presence: on a run that never repinned the partition is a single group, the
// pins table already states it, and a section per report would be noise.
func TestARunWithOneAgreementHasNoEpochSection(t *testing.T) {
	r := staleRunReport()
	r.PinEpochs = nil
	for i := range r.Attempts {
		r.Attempts[i].PinEpoch = 0
	}
	if out := renderPlainRunReport(r); strings.Contains(out, "Pin epochs") {
		t.Errorf("a run that never repinned rendered an epoch section:\n%s", out)
	}
}

// TestPlanningRunRendersNoPinSections: a run that never activated pins nothing,
// and a section of zeros would report an agreement it does not have.
func TestPlanningRunRendersNoPinSections(t *testing.T) {
	out := renderPlainRunReport(&engine.RunReport{
		Run: &model.Run{ID: 40, Status: model.RunPlanning},
	})
	for _, section := range []string{"Pinned workflows", "Pin epochs"} {
		if strings.Contains(out, section) {
			t.Errorf("a planning run rendered %q:\n%s", section, out)
		}
	}
}
