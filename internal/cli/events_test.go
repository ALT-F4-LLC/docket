package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
)

// renderEventList's issue column (DKT-74): engine.Event.Issue was already on
// the wire (events_read.go) but the human table never printed it.
func TestRenderEventListIncludesIssueColumn(t *testing.T) {
	events := []engine.Event{
		{Seq: 1, AtMS: 1000, Kind: "step.claimed", Run: "RUN-1", Issue: "DKT-74", Step: "implement@0"},
	}

	out := renderEventList(events, false)

	if !strings.Contains(out, "DKT-74") {
		t.Fatalf("renderEventList output missing issue DKT-74: %q", out)
	}
}

// An event with no issue (e.g. a trust event) holds the column open with "-"
// rather than collapsing it, matching the run column's existing convention.
func TestRenderEventListHoldsIssueColumnOpenWhenAbsent(t *testing.T) {
	events := []engine.Event{
		{Seq: 1, AtMS: 1000, Kind: "trust.added"},
	}

	out := renderEventList(events, false)

	fields := strings.Fields(out)
	if len(fields) < 5 {
		t.Fatalf("expected at least 5 columns (seq at kind run issue), got %d: %q", len(fields), out)
	}
	if fields[4] != "-" {
		t.Fatalf("expected issue column held open with %q, got %q in %q", "-", fields[4], out)
	}
}
