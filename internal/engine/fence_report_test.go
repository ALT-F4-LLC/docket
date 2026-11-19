package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestDryRunFenceReportReadsHarvestedCommands: the dry run's report, read
// inside the activation transaction, names every harvested command in body
// order with the gate that harvests its tag.
func TestDryRunFenceReportReadsHarvestedCommands(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, _ := seedFenceStep(t, conn, "/usr/bin/true first")
	_, err := conn.Exec(
		`INSERT INTO run_fences (run_id, issue_id, tag, ordinal, command, sha256)
		 VALUES (?, ?, 'checks', 1, '/usr/bin/true second', 'x')`, runID, issueID)
	testsupport.Must(t, err, "seeding a second fence: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	reports, err := buildFenceReportTx(tx, runID)
	testsupport.Must(t, err, "buildFenceReportTx: %v", err)
	if len(reports) != 2 {
		t.Fatalf("got %d reported commands, want 2", len(reports))
	}
	want := []string{"/usr/bin/true first", "/usr/bin/true second"}
	for i, r := range reports {
		if r.Command != want[i] || r.Ordinal != i {
			t.Errorf("report %d = %q at ordinal %d, want %q at ordinal %d",
				i, r.Command, r.Ordinal, want[i], i)
		}
		if r.Issue != model.FormatID(issueID) || r.Tag != "checks" || r.Gate != "checks" {
			t.Errorf("report %d = issue %s tag %q gate %q, want issue %s tag %q gate %q",
				i, r.Issue, r.Tag, r.Gate, model.FormatID(issueID), "checks", "checks")
		}
	}
}
