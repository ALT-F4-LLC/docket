package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// sizedBaselineSource and sizedSmallSource are disjoint by KIND, not by
// labels: [match] has no unless_sizes clause (sizes_any is an inclusion
// clause only, per workflow.Match.Matches), so a fixture proving sizes_any
// discriminates needs the two candidate workflows to stay mutually exclusive
// through the grammar's other terms — the same way the real corpus keeps
// small-change and trivial-change disjoint from standard-change through
// their own [match] clauses, not through a size exclusion that does not
// exist.
const sizedBaselineSource = `
[pipeline]
name = "sized-baseline"
version = 1
[match]
kind = ["bug"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`

// sizedSmallSource binds on the issue's SIZE, through [match].sizes_any
// (schema v34), the engine-side replacement for the small/trivial label
// convention as workflow-routing input.
const sizedSmallSource = `
[pipeline]
name = "sized-small-change"
version = 1
[match]
kind = ["task"]
sizes_any = ["small", "trivial"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`

// TestActivationBindsOnSizesAny is the activation-level counterpart to
// TestSizesAnyMatrix and TestSizesAnyParsesAndRoundTrips (internal/workflow):
// an issue whose stored Size is in a workflow's sizes_any list binds that
// workflow through the real activation path, exactly as an issue whose
// labels satisfy labels_any binds through it.
func TestActivationBindsOnSizesAny(t *testing.T) {
	conn := mustDB(t)
	baseline := registerSource(t, conn, []byte(sizedBaselineSource), "sized-baseline.toml")
	small := registerSource(t, conn, []byte(sizedSmallSource), "sized-small-change.toml")

	// createIssue does not expose Size, so the row is written directly
	// through the same db.CreateIssue path it calls, mirroring its shape.
	sizedID, err := db.CreateIssue(conn, &model.Issue{
		Title: "one-line typo fix", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask, Size: model.SizeSmall,
	}, nil, nil)
	testsupport.Must(t, err, "creating the sized issue: %v", err)

	// bug kind, no size: binds sized-baseline (kind = ["bug"]) and fails
	// sized-small-change's kind = ["task"] clause outright, so the two stay
	// mutually exclusive without needing a size exclusion the grammar has no
	// clause for.
	unsizedID := createIssue(t, conn, "a real defect", "a body", "bug", nil)

	run := startRun(t, conn, sizedID, unsizedID)
	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if result.Run.Status != model.RunActive {
		t.Fatalf("run status = %q, want %q", result.Run.Status, model.RunActive)
	}
	if len(result.BoundIssues) != 2 {
		t.Fatalf("bound issues = %+v, want 2", result.BoundIssues)
	}

	byID := make(map[string]string, len(result.BoundIssues))
	for _, b := range result.BoundIssues {
		byID[b.IssueID] = b.Workflow
	}

	if got := byID[model.FormatID(sizedID)]; got != small.Ref() {
		t.Errorf("the small-sized issue bound %q, want %q (sizes_any)", got, small.Ref())
	}
	if got := byID[model.FormatID(unsizedID)]; got != baseline.Ref() {
		t.Errorf("the unsized issue bound %q, want %q (the baseline)", got, baseline.Ref())
	}
}
